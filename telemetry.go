package galleon

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// TracerProvider sets the OpenTelemetry tracer provider every plane uses. The
// app takes ownership: if tp has a Shutdown(ctx) error method (the SDK's
// provider does), it is called last during shutdown to flush spans.
// Without this option the global provider is used and left alone.
func TracerProvider(tp trace.TracerProvider) Option {
	return func(s *settings) { s.tracerProvider = tp; s.ownTracer = true }
}

// MeterProvider sets the OpenTelemetry meter provider, with the same ownership
// rule as TracerProvider.
func MeterProvider(mp metric.MeterProvider) Option {
	return func(s *settings) { s.meterProvider = mp; s.ownMeter = true }
}

type shutdowner interface {
	Shutdown(ctx context.Context) error
}

// shutdownTelemetry flushes the providers the app owns. It is the last step of
// shutdown so everything before it is exported.
func (a *App) shutdownTelemetry(ctx context.Context) error {
	var errs []error
	if s, ok := a.settings.tracerProvider.(shutdowner); ok && a.settings.ownTracer {
		if err := s.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if s, ok := a.settings.meterProvider.(shutdowner); ok && a.settings.ownMeter {
		if err := s.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// LogLevel returns the level of the logger the kernel built, which can be
// changed while the app runs. It is nil when the app was given its own logger
// through the Logger option.
func (a *App) LogLevel() *slog.LevelVar { return a.level }

type logAttrsKey struct{}

// WithLogAttrs returns a context whose log records carry attrs. Planes use it
// to put request and operation IDs on every record logged with that context.
func WithLogAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	prev, _ := ctx.Value(logAttrsKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(prev)+len(attrs))
	merged = append(append(merged, prev...), attrs...)
	return context.WithValue(ctx, logAttrsKey{}, merged)
}

// ContextHandler adds the trace and span IDs of the active span, and any attrs
// from WithLogAttrs, to each record. The kernel wraps the app's logger with it,
// so a log line written with a request's context can be found from its trace.
func ContextHandler(h slog.Handler) slog.Handler {
	if _, ok := h.(contextHandler); ok {
		return h
	}
	return contextHandler{h}
}

type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	if attrs, ok := ctx.Value(logAttrsKey{}).([]slog.Attr); ok {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
