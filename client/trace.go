package client

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/thec1oud/galleon/errs"
)

var propagator = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})

// tracing makes each attempt a client span and sends the trace context with
// it. The span ends when the body is closed.
type tracing struct {
	next   http.RoundTripper
	tracer trace.Tracer
	name   string
}

func (t *tracing) RoundTrip(req *http.Request) (*http.Response, error) {
	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(req.Method),
		semconv.ServerAddress(req.URL.Hostname()),
		semconv.URLFull(redact(req.URL)),
		attribute.String("galleon.client", t.name),
	}
	if p := port(req.URL); p > 0 {
		attrs = append(attrs, semconv.ServerPort(p))
	}
	if n := resendCount(req.Context()); n > 0 {
		attrs = append(attrs, semconv.HTTPRequestResendCount(n))
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	ctx, span := t.tracer.Start(req.Context(), method, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	req = req.Clone(ctx)
	propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))

	res, err := t.next.RoundTrip(req)
	if err != nil {
		span.SetAttributes(semconv.ErrorTypeKey.String(errorType(err)))
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}
	span.SetAttributes(semconv.HTTPResponseStatusCode(res.StatusCode))
	if res.StatusCode >= 400 {
		span.SetAttributes(semconv.ErrorTypeKey.String(strconv.Itoa(res.StatusCode)))
		span.SetStatus(codes.Error, http.StatusText(res.StatusCode))
	}
	res.Body = &onClose{ReadCloser: res.Body, fn: func() { span.End() }}
	return res, nil
}

func errorType(err error) string {
	var e *errs.Error
	if errors.As(err, &e) && e.Code != "" {
		return e.Code
	}
	return "_OTHER"
}

// redact drops the URL's credentials and replaces query values, which often
// carry tokens or signatures.
func redact(u *url.URL) string {
	c := *u
	c.User = nil
	c.Fragment, c.RawFragment = "", ""
	if c.RawQuery != "" {
		q := c.Query()
		for k, vs := range q {
			for i := range vs {
				vs[i] = "REDACTED"
			}
			q[k] = vs
		}
		c.RawQuery = q.Encode()
	}
	return c.String()
}

func port(u *url.URL) int {
	if p := u.Port(); p != "" {
		n, _ := strconv.Atoi(p)
		return n
	}
	switch u.Scheme {
	case "https":
		return 443
	case "http":
		return 80
	}
	return 0
}
