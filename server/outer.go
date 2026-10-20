package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/errs"
)

// propagator reads W3C trace context and baggage from incoming requests.
var propagator = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})

// outer wraps h in the built-in outer middleware, outermost first: tracing,
// panic recovery, request IDs, security headers and the access log. Module
// middleware sits inside these, around the router.
func (s *Server) outer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := s.tracer.Start(ctx, r.Method,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(r.Method),
				semconv.URLPath(r.URL.Path),
				semconv.URLScheme(scheme(r)),
			))
		defer span.End()

		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) {
			id = newRequestID()
		}
		ctx = galleon.WithLogAttrs(ctx, slog.String("request_id", id))
		sw := &statusWriter{ResponseWriter: w}
		hdr := w.Header()
		hdr.Set("X-Request-Id", id)
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Referrer-Policy", "no-referrer")
		r = r.WithContext(ctx)

		// finish records the request's outcome in the span and the access log.
		finish := func(aborted bool) {
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			span.SetAttributes(semconv.HTTPResponseStatusCode(status))
			if status >= 500 || aborted {
				span.SetStatus(codes.Error, http.StatusText(status))
			}
			if s.cfg.AccessLog {
				s.log.InfoContext(ctx, "http request", "method", r.Method, "path", r.URL.Path, "status", status, "duration", time.Since(start), "peer", s.peer(r), "aborted", aborted)
			}
		}
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					finish(true)
					panic(p)
				}
				s.log.ErrorContext(ctx, "panic while serving a request", "method", r.Method, "path", r.URL.Path, "panic", p, "stack", string(debug.Stack()))
				span.SetStatus(codes.Error, "panic")
				if sw.wrote {
					// The response has started, so it can't become an error.
					// Cut the connection, so the client can't mistake a
					// truncated response for a whole one.
					finish(true)
					panic(http.ErrAbortHandler)
				}
				// Drop whatever the handler set, such as Content-Encoding or
				// Set-Cookie, keeping only the headers set above. Module
				// middleware's headers, CORS ones included, go too, since a
				// panic may have left them half set.
				for k := range hdr {
					if k != "X-Request-Id" && k != "X-Content-Type-Options" && k != "X-Frame-Options" && k != "Referrer-Policy" {
						delete(hdr, k)
					}
				}
				s.writeError(sw, r, errs.New(errs.Internal, "galleon.panic", ""), 0)
			}
			finish(false)
		}()
		h.ServeHTTP(sw, r)
	})
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// peer works out the caller's IP. A connection from a trusted proxy is
// replaced by the right-most X-Forwarded-For address that isn't a trusted
// proxy itself.
func (s *Server) peer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// A Unix socket, or something we can't read. Behind a local proxy
		// listed as "unix", the header names the caller.
		if !s.trustUnix {
			return ""
		}
	} else if addr = addr.Unmap(); !s.isTrusted(addr) {
		return addr.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break // a malformed hop: stop at the last address we could trust
		}
		hop = hop.Unmap()
		if !s.isTrusted(hop) {
			return hop.String()
		}
		addr = hop
	}
	if !addr.IsValid() {
		return ""
	}
	return addr.String()
}

func (s *Server) isTrusted(a netip.Addr) bool {
	for _, p := range s.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// statusWriter records the status written, for telemetry and the access log.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	// An informational response, such as 103 Early Hints, isn't the final
	// status, and the response can still become an error after it.
	if !w.wrote && (code < 100 || code >= 200) {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

// Flush lets streaming handlers flush through the wrapper.
func (w *statusWriter) Flush() {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap gives http.ResponseController the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
