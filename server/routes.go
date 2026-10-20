package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/bind"
	"github.com/thec1oud/galleon/codec"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

// routeAll registers every HTTP binding and every RPC procedure it serves on
// a ServeMux, plus a catch-all that answers unknown paths with 404 and wrong
// methods with 405, as problem+json.
func (s *Server) routeAll(ops *galleon.Operations) (_ *http.ServeMux, err error) {
	mux := http.NewServeMux()
	var methods []string
	for _, x := range ops.All() {
		var binder bind.Func
		if s.serveHTTP && slices.ContainsFunc(x.Desc.Bindings, func(b op.Binding) bool { _, ok := b.(op.HTTP); return ok }) {
			// Only HTTP fills inputs from the path, query and headers.
			if binder, err = bind.Plan(x.Desc.Input.Type); err != nil {
				return nil, fmt.Errorf("http: operation %q (module %q, %s): %w", x.Desc.ID, x.Module, x.Site, err)
			}
		}
		in := s.codecs.With(x.Desc.Input.Codecs...)
		out := s.codecs.With(x.Desc.Output.Codecs...)
		for _, b := range x.Desc.Bindings {
			if rb, ok := b.(op.RPC); ok && s.serveRPC {
				if err := handle(mux, "POST "+rb.String(), s.rpcHandler(x, rb, in, out)); err != nil {
					return nil, fmt.Errorf("rpc: operation %q (module %q, %s): %w", x.Desc.ID, x.Module, x.Site, err)
				}
				s.routes = append(s.routes, Route{Method: "RPC", Path: rb.String(), Operation: x.Desc.ID})
				if !slices.Contains(methods, http.MethodPost) {
					methods = append(methods, http.MethodPost) // so a GET there is a 405
				}
				continue
			}
			hb, ok := b.(op.HTTP)
			if !ok || !s.serveHTTP {
				continue
			}
			pattern := hb.Method + " " + hb.Path
			h := &opHandler{s: s, x: x, b: hb, binder: binder, in: in, out: out}
			if err := handle(mux, pattern, h); err != nil {
				return nil, fmt.Errorf("http: operation %q (module %q, %s): %w", x.Desc.ID, x.Module, x.Site, err)
			}
			s.routes = append(s.routes, Route{Method: hb.Method, Path: hb.Path, Operation: x.Desc.ID})
			if !slices.Contains(methods, hb.Method) {
				methods = append(methods, hb.Method)
			}
		}
	}
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var allow []string
		for _, m := range methods {
			probe := r.Clone(r.Context())
			probe.Method = m
			if _, pattern := mux.Handler(probe); pattern != "/" {
				allow = append(allow, m)
			}
		}
		if len(allow) > 0 {
			w.Header().Set("Allow", joinMethods(allow))
			s.writeError(w, r, errs.New(errs.InvalidArgument, "galleon.method_not_allowed", "the method is not allowed for this path"), http.StatusMethodNotAllowed)
			return
		}
		s.writeError(w, r, errs.New(errs.NotFound, "galleon.route_not_found", "no such route"), 0)
	}))
	return mux, nil
}

// handle registers h, turning ServeMux's panic on a conflicting pattern into
// an error.
func handle(mux *http.ServeMux, pattern string, h http.Handler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	mux.Handle(pattern, h)
	return nil
}

func joinMethods(m []string) string {
	if slices.Contains(m, http.MethodGet) && !slices.Contains(m, http.MethodHead) {
		m = append(m, http.MethodHead)
	}
	out := ""
	for i, x := range m {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

// opHandler serves one HTTP binding of one operation.
type opHandler struct {
	s       *Server
	x       *galleon.Operation
	b       op.HTTP
	binder  bind.Func
	in, out *codec.Set
}

func (h *opHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := trace.SpanFromContext(ctx)
	span.SetName(h.b.Method + " " + h.b.Path)
	span.SetAttributes(semconv.HTTPRoute(h.b.Path), attribute.String("galleon.operation", h.x.Desc.ID))

	// Pick the response format first, so a client that accepts nothing we
	// speak is told before any work is done.
	enc := h.out.Negotiate(r.Header.Get("Accept"))
	if enc == nil {
		w.Header().Set("Accept", joinTypes(h.out.MediaTypes()))
		h.s.writeError(w, r, errs.New(errs.InvalidArgument, "galleon.not_acceptable", "none of the accepted media types can be produced"), http.StatusNotAcceptable)
		return
	}

	call := &op.Call{Request: &op.Request{
		Transport: "http",
		Binding:   h.b,
		Header:    r.Header,
		Peer:      peerFrom(ctx),
		Decode:    func(into any) error { return h.decode(w, r, into) },
	}, Response: &op.Response{Header: http.Header{}}}
	out, err := h.x.Invoke(ctx, call)
	addHeaders(w.Header(), call.Response.Header)
	if err != nil {
		status := 0
		if errors.Is(err, errUnsupportedMediaType) {
			w.Header().Set("Accept", joinTypes(h.in.MediaTypes()))
			status = http.StatusUnsupportedMediaType
		}
		h.s.writeError(w, r, err, status)
		return
	}
	if out == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, err := codec.Marshal(enc, out)
	if err != nil {
		h.s.writeError(w, r, errs.Wrap(err, errs.Internal, "galleon.encode_failed", ""), 0)
		return
	}
	w.Header().Set("Content-Type", enc.MediaType())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// addHeaders adds the headers interceptors set for the response to dst,
// leaving alone any dst already has, and never ones that frame the message
// or belong to the connection or the protocol (see reservedHeader): the
// transport owns those.
func addHeaders(dst, src http.Header) {
	for k, v := range src {
		k = http.CanonicalHeaderKey(k)
		if _, ok := dst[k]; ok || len(v) == 0 || reservedHeader(k) {
			continue
		}
		dst[k] = v
	}
}

// reservedHeader reports whether an interceptor's response header k, in
// canonical form, is the transport's: framing and hop-by-hop headers, and
// gRPC's and Connect's own. HTTP and RPC refuse the same ones.
func reservedHeader(k string) bool {
	switch k {
	case "Content-Length", "Content-Encoding", "Content-Type", "Transfer-Encoding", "Connection", "Keep-Alive",
		"Upgrade", "Trailer", "Te":
		return true
	}
	return strings.HasPrefix(k, "Proxy-") || strings.HasPrefix(k, "Grpc-") || strings.HasPrefix(k, "Connect-")
}

var errUnsupportedMediaType = errs.New(errs.InvalidArgument, "galleon.unsupported_media_type", "the request body's media type is not supported")

// decode fills into from the body, then from path, query and header
// parameters, which win over body fields of the same name.
func (h *opHandler) decode(w http.ResponseWriter, r *http.Request, into any) error {
	if hasBody(r) {
		dec := h.in.ForContentType(r.Header.Get("Content-Type"))
		if dec == nil {
			return errUnsupportedMediaType
		}
		limit := h.x.Desc.Policies.BodyLimit
		if limit <= 0 {
			limit = h.s.cfg.BodyLimit
		}
		body := http.MaxBytesReader(w, r.Body, limit)
		if err := dec.Decode(body, into, h.x.Desc.Policies.UnknownFields); err != nil {
			if r.Context().Err() != nil && !timedOut(err) {
				// The client went away mid-body. Over HTTP/1 that is the
				// only way a body ends short, so don't blame its syntax. A
				// read timeout cancels the request too, but that one is ours.
				return errs.Wrap(err, errs.Canceled, "galleon.body_read", "reading the request body was canceled")
			}
			return err
		}
	}
	if h.binder != nil {
		if err := h.binder(into, requestSource{r}).Err(); err != nil {
			return err
		}
	}
	return nil
}

func timedOut(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func hasBody(r *http.Request) bool {
	return r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0
}

func joinTypes(t []string) string {
	out := ""
	for i, x := range t {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

type requestSource struct{ r *http.Request }

func (s requestSource) PathValue(name string) string { return s.r.PathValue(name) }
func (s requestSource) Query(name string) []string   { return s.r.URL.Query()[name] }
func (s requestSource) Header(name string) []string  { return s.r.Header.Values(name) }

// writeError renders err as problem+json. status overrides the status the
// error's kind maps to, for HTTP-only answers such as 405 and 415.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error, status int) {
	ctx := r.Context()
	e := errs.From(err)
	span := trace.SpanFromContext(ctx)
	traceID := ""
	if sc := span.SpanContext(); sc.HasTraceID() {
		traceID = sc.TraceID().String()
	}
	if e.Kind == errs.Internal {
		s.log.ErrorContext(ctx, "request failed", "method", r.Method, "path", r.URL.Path, "error", errorText(e))
		span.SetStatus(codes.Error, e.Code)
	}
	if status == 0 {
		errs.WriteProblem(w, e, traceID)
		return
	}
	p := errs.ToProblem(e, traceID)
	p.Status = status
	writeProblem(w, p)
}

func errorText(e *errs.Error) string {
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Error()
}

func writeProblem(w http.ResponseWriter, p errs.Problem) {
	b, _ := codec.Marshal(codec.JSON, p)
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_, _ = w.Write(b)
}
