package galleon_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
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

	// Inside a group, the context attrs stay top-level keys.
	buf.Reset()
	got.WithGroup("http").With("status", 200).InfoContext(ctx, "grouped", "path", "/x")
	line = buf.String()
	for _, want := range []string{" trace_id=", " request_id=r-1", "http.status=200", "http.path=/x"} {
		if !strings.Contains(line, want) {
			t.Errorf("grouped log line missing %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "http.trace_id") {
		t.Errorf("trace_id was put inside the group:\n%s", line)
	}

	// No valid span: no trace IDs. A child's attrs don't leak into its parent.
	buf.Reset()
	parent := galleon.WithLogAttrs(context.Background(), slog.String("a", "1"))
	_ = galleon.WithLogAttrs(parent, slog.String("b", "2"))
	got.InfoContext(parent, "plain")
	line = buf.String()
	if strings.Contains(line, "trace_id") || strings.Contains(line, "b=2") || !strings.Contains(line, "a=1") {
		t.Errorf("unexpected attrs on a record without a span:\n%s", line)
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
	calls   *int
	ctxErr  *error
}

func (p flushingTP) Shutdown(ctx context.Context) error {
	*p.flushed = true
	if p.calls != nil {
		*p.calls++
	}
	if p.ctxErr != nil {
		*p.ctxErr = ctx.Err()
	}
	return nil
}

func newFlushing() (flushingTP, *int, *error) {
	calls, ctxErr := new(int), new(error)
	return flushingTP{flushed: new(bool), calls: calls, ctxErr: ctxErr}, calls, ctxErr
}

func TestOwnedProviderIsShutDownExactlyOnce(t *testing.T) {
	cases := map[string]func(t *testing.T, opts []galleon.Option){
		"after shutdown": func(t *testing.T, opts []galleon.Option) {
			app, err := galleon.New(opts...)
			if err != nil {
				t.Fatal(err)
			}
			_ = app.Start(context.Background())
			_ = app.Shutdown(context.Background())
			_ = app.Shutdown(context.Background())
		},
		"after a failed start": func(t *testing.T, opts []galleon.Option) {
			mod := galleon.ModuleFunc("m", func(r *galleon.Registry) error {
				galleon.Provide[failing](r, func() failing { return failing{} })
				return nil
			})
			app, err := galleon.New(append(opts, galleon.Modules(mod))...)
			if err != nil {
				t.Fatal(err)
			}
			if app.Start(context.Background()) == nil {
				t.Fatal("start should fail")
			}
			_ = app.Shutdown(context.Background())
		},
		"when New fails": func(t *testing.T, opts []galleon.Option) {
			if _, err := galleon.New(append(opts, galleon.ConfigJSON([]byte(`{"typo":{}}`)))...); err == nil {
				t.Fatal("New should fail")
			}
		},
		"when never started": func(t *testing.T, opts []galleon.Option) {
			app, err := galleon.New(opts...)
			if err != nil {
				t.Fatal(err)
			}
			_ = app.Shutdown(context.Background())
			_ = app.Shutdown(context.Background())
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			tp, calls, _ := newFlushing()
			run(t, append(base(), galleon.TracerProvider(tp)))
			if *calls != 1 {
				t.Fatalf("Shutdown called %d times, want 1", *calls)
			}
		})
	}
}

func TestFlushGetsItsOwnDeadline(t *testing.T) {
	tp, calls, ctxErr := newFlushing()
	app, err := galleon.New(append(base(), galleon.TracerProvider(tp))...)
	if err != nil {
		t.Fatal(err)
	}
	_ = app.Start(context.Background())
	spent, cancel := context.WithCancel(context.Background())
	cancel()
	_ = app.Shutdown(spent)
	if *calls != 1 || *ctxErr != nil {
		t.Fatalf("flush calls=%d ctx err=%v: the flush must not inherit a spent context", *calls, *ctxErr)
	}
}

func TestGlobalAndNilProvidersAreNeverShutDown(t *testing.T) {
	global, calls, _ := newFlushing()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(global)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	app, err := galleon.New(append(base(), galleon.TracerProvider(nil), galleon.MeterProvider(nil))...)
	if err != nil {
		t.Fatal(err)
	}
	_ = app.Start(context.Background())
	_ = app.Shutdown(context.Background())
	if *calls != 0 {
		t.Fatal("the global provider was shut down")
	}
}

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
	_, err = galleon.New(base()...)
	var be *galleon.BootError
	if !errors.As(err, &be) || !strings.Contains(err.Error(), `GALLEON_GENERATION="x" is not a generation number`) {
		t.Fatalf("invalid generation should be a boot error naming the variable, got %v", err)
	}
}
