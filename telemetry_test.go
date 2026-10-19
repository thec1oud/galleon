package galleon_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/coord/local"
)

func TestLogsCarryNodeTraceAndContextAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	var got *slog.Logger
	mod := galleon.ModuleFunc("m", func(r *galleon.Registry) error {
		galleon.Provide[*Service](r, func(l *slog.Logger) *Service { got = l; return &Service{log: new([]string)} })
		return nil
	})
	app, err := galleon.New(galleon.Logger(logger), galleon.Name("orders"), galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode), galleon.DrainGrace(0), galleon.Modules(mod))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3},
		SpanID:     trace.SpanID{4, 5, 6},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	ctx = galleon.WithLogAttrs(ctx, slog.String("request_id", "r-1"))
	buf.Reset()
	got.InfoContext(ctx, "hello")
	line := buf.String()
	for _, want := range []string{"app=orders", "node=" + app.Node().String(), "trace_id=" + sc.TraceID().String(), "span_id=" + sc.SpanID().String(), "request_id=r-1"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q:\n%s", want, line)
		}
	}
}

func TestDefaultLoggerLevelIsAdjustable(t *testing.T) {
	app, err := galleon.New(galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode))
	if err != nil {
		t.Fatal(err)
	}
	if app.LogLevel() == nil {
		t.Fatal("kernel-built logger should expose its level")
	}
	app.LogLevel().Set(slog.LevelDebug)

	app2, err := galleon.New(base()...)
	if err != nil {
		t.Fatal(err)
	}
	if app2.LogLevel() != nil {
		t.Fatal("a supplied logger's level is not the kernel's to change")
	}
}

type flushingTP struct {
	noop.TracerProvider
	flushed *bool
}

func (p flushingTP) Shutdown(context.Context) error { *p.flushed = true; return nil }

func TestOwnedTracerProviderIsFlushedLast(t *testing.T) {
	var flushed bool
	var order []string
	mod := galleon.ModuleFunc("m", func(r *galleon.Registry) error {
		galleon.Provide[*Repo](r, func(tp trace.TracerProvider, mp metric.MeterProvider, clk clock.Clock, n galleon.Node) *Repo {
			if tp == nil || mp == nil || clk == nil || n.PID == 0 {
				t.Error("kernel values not injected")
			}
			return &Repo{log: &order}
		})
		r.OnStop(func(context.Context) error {
			if flushed {
				t.Error("telemetry flushed before stop hooks ran")
			}
			return nil
		})
		return nil
	})
	app, err := galleon.New(append(base(mod), galleon.TracerProvider(flushingTP{flushed: &flushed}))...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !flushed {
		t.Fatal("owned tracer provider was not shut down")
	}
}

func TestClockOptionIsInjected(t *testing.T) {
	fake := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	app, err := galleon.New(append(base(), galleon.Clock(fake))...)
	if err != nil {
		t.Fatal(err)
	}
	_ = app.Start(context.Background())
	defer app.Shutdown(context.Background())
	if galleon.MustResolve[clock.Clock](app) != clock.Clock(fake) {
		t.Fatal("configured clock not provided")
	}
}

func TestGenerationFromEnvironment(t *testing.T) {
	t.Setenv(galleon.GenerationEnv, "7")
	app, err := galleon.New(base()...)
	if err != nil {
		t.Fatal(err)
	}
	if app.Node().Generation != 7 {
		t.Fatalf("generation = %d", app.Node().Generation)
	}
	t.Setenv(galleon.GenerationEnv, "x")
	if _, err := galleon.New(base()...); err == nil {
		t.Fatal("invalid generation should fail boot")
	}
}
