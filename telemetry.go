package galleon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// TracerProvider sets the OpenTelemetry tracer provider every plane uses, and
// hands it to the app: if tp has a Shutdown(ctx) error method (the SDK's
// provider does), the app calls it exactly once, as the last step of Shutdown,
// or when New fails. Without this option the global provider is used and is
// never shut down by the app. A nil tp is ignored.
func TracerProvider(tp trace.TracerProvider) Option {
	return func(s *settings) {
		if tp != nil {
			s.tracerProvider, s.ownTracer = tp, true
		}
	}
}

// MeterProvider sets the OpenTelemetry meter provider, with the same ownership
// rule as TracerProvider.
func MeterProvider(mp metric.MeterProvider) Option {
	return func(s *settings) {
		if mp != nil {
			s.meterProvider, s.ownMeter = mp, true
		}
	}
}

// spentFlushBudget is how long a flush may take when the shutdown deadline is
// already spent.
const spentFlushBudget = 2 * time.Second

type shutdowner interface {
	Shutdown(ctx context.Context) error
}

// shutdownTelemetry flushes the providers s owns, once. It is the last step of
// shutdown so everything before it is exported. It uses what is left of the
// caller's deadline; only if drain and stop have already spent it does it get
// a short budget of its own, since a flush with a done context drops every
// buffered span.
func shutdownTelemetry(ctx context.Context, s *settings) error {
	if !s.ownTracer && !s.ownMeter {
		return nil
	}
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), spentFlushBudget)
		defer cancel()
	}
	var errs []error
	if p, ok := s.tracerProvider.(shutdowner); ok && s.ownTracer {
		s.ownTracer = false
		if err := p.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if p, ok := s.meterProvider.(shutdowner); ok && s.ownMeter {
		s.ownMeter = false
		if err := p.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("flushing telemetry: %w", err)
	}
	return nil
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
// from WithLogAttrs, to each record. They stay top-level keys even inside
// groups opened through the wrapped handler (a group already inside h when it
// is wrapped can't be undone). The
// kernel wraps the app's logger with it, so a log line written with a
// request's context can be found from its trace.
func ContextHandler(h slog.Handler) slog.Handler {
	if _, ok := h.(*contextHandler); ok {
		return h
	}
	return &contextHandler{base: h, cur: h}
}

// contextHandler keeps the handler as it was before any WithGroup, plus the
// With calls made since, so context attrs can be added outside the groups.
type contextHandler struct {
	base slog.Handler                      // before the first WithGroup
	ops  []func(slog.Handler) slog.Handler // WithGroup and later WithAttrs, in order
	cur  slog.Handler                      // base with ops applied
}

func (h *contextHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.cur.Enabled(ctx, l)
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	var attrs []slog.Attr
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs, slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	if extra, ok := ctx.Value(logAttrsKey{}).([]slog.Attr); ok {
		attrs = append(attrs, extra...)
	}
	if len(attrs) == 0 {
		return h.cur.Handle(ctx, r)
	}
	if len(h.ops) == 0 {
		r.AddAttrs(attrs...)
		return h.cur.Handle(ctx, r)
	}
	// Inside a group: rebuild the chain with the attrs added before it, so they
	// stay top-level keys like trace_id rather than group.trace_id.
	hh := h.base.WithAttrs(attrs)
	for _, op := range h.ops {
		hh = op(hh)
	}
	return hh.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(h.ops) == 0 {
		next := h.base.WithAttrs(attrs)
		return &contextHandler{base: next, cur: next}
	}
	return h.with(func(x slog.Handler) slog.Handler { return x.WithAttrs(attrs) })
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.with(func(x slog.Handler) slog.Handler { return x.WithGroup(name) })
}

func (h *contextHandler) with(op func(slog.Handler) slog.Handler) slog.Handler {
	ops := append(h.ops[:len(h.ops):len(h.ops)], op)
	return &contextHandler{base: h.base, ops: ops, cur: op(h.cur)}
}
