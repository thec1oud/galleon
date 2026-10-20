package galleon_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

type GreetIn struct{ Name string }
type GreetOut struct{ Greeting string }

func greet(_ context.Context, in *GreetIn) (*GreetOut, error) {
	return &GreetOut{Greeting: "hello " + in.Name}, nil
}

var Greet = op.Unary[GreetIn, GreetOut]("greet.hello").HTTP("POST", "/hello").Public()

func opsModule(register func(r *galleon.Registry)) galleon.Module {
	return galleon.ModuleFunc("greet", func(r *galleon.Registry) error { register(r); return nil })
}

func bootOps(t *testing.T, mods ...galleon.Module) (*galleon.App, *galleon.Operations) {
	t.Helper()
	app, err := galleon.New(base(mods...)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	return app, galleon.MustResolve[*galleon.Operations](app)
}

func bootFails(t *testing.T, want string, mods ...galleon.Module) {
	t.Helper()
	_, err := galleon.New(base(mods...)...)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want a boot error containing %q, got %v", want, err)
	}
}

func TestOperationRunsThroughThePipeline(t *testing.T) {
	_, ops := bootOps(t, opsModule(func(r *galleon.Registry) { r.Operation(Greet, greet) }))
	out, err := ops.Call(context.Background(), "greet.hello", &GreetIn{Name: "ann"})
	if err != nil || out.(*GreetOut).Greeting != "hello ann" {
		t.Fatalf("got %v, %v", out, err)
	}
	x := ops.Lookup("greet.hello")
	if x == nil || x.Module != "greet" || !strings.Contains(x.Site, "operations_test.go") || len(ops.All()) != 1 {
		t.Fatalf("operation: %+v", x)
	}
	if strings.Join(x.Steps, ",") != "admit,decode,validate_shape,validate_meaning,handle" {
		t.Fatalf("steps: %v", x.Steps)
	}
}

func TestCallChecksTheInputType(t *testing.T) {
	_, ops := bootOps(t, opsModule(func(r *galleon.Registry) { r.Operation(Greet, greet) }))
	if _, err := ops.Call(context.Background(), "greet.hello", GreetIn{}); errs.KindOf(err) != errs.Internal {
		t.Fatalf("a value instead of a pointer must be refused, got %v", err)
	}
	if _, err := ops.Call(context.Background(), "greet.nope", &GreetIn{}); errs.KindOf(err) != errs.NotFound {
		t.Fatalf("an unknown operation is NotFound, got %v", err)
	}
}

func TestDecodeUsesTheTransportDecoder(t *testing.T) {
	_, ops := bootOps(t, opsModule(func(r *galleon.Registry) { r.Operation(Greet, greet) }))
	x := ops.Lookup("greet.hello")
	req := &op.Request{Transport: "test", Decode: func(into any) error { into.(*GreetIn).Name = "bob"; return nil }}
	out, err := x.Invoke(context.Background(), &op.Call{Desc: x.Desc, Request: req})
	if err != nil || out.(*GreetOut).Greeting != "hello bob" {
		t.Fatalf("got %v, %v", out, err)
	}
	bad := errs.New(errs.InvalidArgument, "galleon.bad_json", "bad")
	req.Decode = func(any) error { return bad }
	if _, err := x.Invoke(context.Background(), &op.Call{Desc: x.Desc, Request: req}); err != bad {
		t.Fatalf("the decoder's error must come back unchanged, got %v", err)
	}
	if _, err := x.Invoke(context.Background(), &op.Call{Desc: x.Desc, Request: &op.Request{}}); errs.KindOf(err) != errs.Internal {
		t.Fatalf("a transport without a decoder is an internal error, got %v", err)
	}
}

func TestRegistrationErrorsFailBoot(t *testing.T) {
	cases := map[string]func(r *galleon.Registry){
		"declares no security": func(r *galleon.Registry) {
			r.Operation(op.Unary[GreetIn, GreetOut]("greet.hello"), greet)
		},
		"want func(context.Context, *galleon_test.GreetIn)": func(r *galleon.Registry) {
			r.Operation(Greet, func(context.Context, *GreetOut) (*GreetOut, error) { return nil, nil })
		},
		"is already registered at": func(r *galleon.Registry) {
			r.Operation(Greet, greet)
			r.Operation(op.Unary[GreetIn, GreetOut]("greet.hello").Public(), greet)
		},
		`which operation "greet.hello" already binds`: func(r *galleon.Registry) {
			r.Operation(Greet, greet)
			r.Operation(op.Unary[GreetIn, GreetOut]("greet.other").HTTP("post", "/hello").Public(), greet)
		},
		"the definition is nil": func(r *galleon.Registry) { r.Operation(nil, greet) },
		"invalid position": func(r *galleon.Registry) {
			r.Intercept(op.Position{Stage: 99, Where: op.BeforeStage}, func(n op.Invoker) op.Invoker { return n })
		},
		"implemented by the kernel": func(r *galleon.Registry) {
			r.ImplementStage(op.StageDecode, func(n op.Invoker) op.Invoker { return n })
		},
	}
	for want, reg := range cases {
		t.Run(want, func(t *testing.T) { bootFails(t, want, opsModule(reg)) })
	}
}

func TestErrorsNameTheModuleAndSite(t *testing.T) {
	bootFails(t, `module "greet" (`, opsModule(func(r *galleon.Registry) {
		r.Operation(op.Unary[GreetIn, GreetOut]("greet.hello"), greet)
	}))
	bootFails(t, "operations_test.go:", opsModule(func(r *galleon.Registry) {
		r.Operation(Greet, greet)
		r.Operation(Greet, greet)
	}))
}

func TestNonPublicOperationsNeedAuthorization(t *testing.T) {
	secured := op.Unary[GreetIn, GreetOut]("greet.secret").Authorize("greet:secret", op.Resource("org", "org_id")).Contexts("org").Schemes("bearer")
	mod := opsModule(func(r *galleon.Registry) { r.Operation(secured, greet) })
	for _, want := range []string{
		"no module implements the pre_authorize stage",
		"no module implements the authorize stage",
		"no module implements the authenticate stage",
		"no module implements the resolve_context stage",
		"no module implements the gate stage",
	} {
		bootFails(t, want, mod)
	}

	pass := func(n op.Invoker) op.Invoker { return n }
	security := galleon.ModuleFunc("security", func(r *galleon.Registry) error {
		for _, s := range []op.Stage{op.StageAuthenticate, op.StageResolveContext, op.StageGate, op.StagePreAuthorize, op.StageAuthorize} {
			r.ImplementStage(s, pass)
		}
		return nil
	})
	_, ops := bootOps(t, mod, security)
	if _, err := ops.Call(context.Background(), "greet.secret", &GreetIn{}); err != nil {
		t.Fatal(err)
	}
}

func TestAStageHasOneImplementation(t *testing.T) {
	impl := func(name string) galleon.Module {
		return galleon.ModuleFunc(name, func(r *galleon.Registry) error {
			r.ImplementStage(op.StageAuthorize, func(n op.Invoker) op.Invoker { return n })
			return nil
		})
	}
	bootFails(t, `the authorize stage is already implemented by module "authz1"`, impl("authz1"), impl("authz2"))
}

func TestPreAuthorizeRunsBeforeDecodeAndShapeBeforeAuthorize(t *testing.T) {
	var log []string
	mark := func(name string) op.Interceptor {
		return func(next op.Invoker) op.Invoker {
			return func(ctx context.Context, c *op.Call) (any, error) {
				log = append(log, name)
				return next(ctx, c)
			}
		}
	}
	shape := op.ValidatorFunc[GreetIn](func(context.Context, *GreetIn) *op.ValidationError {
		log = append(log, "shape")
		return nil
	})
	meaning := op.MeaningValidatorFunc[GreetIn](func(context.Context, *GreetIn) *op.ValidationError {
		log = append(log, "meaning")
		return nil
	})
	d := op.Unary[GreetIn, GreetOut]("greet.secret").Authorize("greet:secret", op.Resource("org", "id")).Validate(shape).ValidateMeaning(meaning)
	mod := opsModule(func(r *galleon.Registry) {
		r.Operation(d, func(ctx context.Context, in *GreetIn) (*GreetOut, error) {
			log = append(log, "handle")
			return greet(ctx, in)
		})
		r.ImplementStage(op.StagePreAuthorize, mark("pre_authorize"))
		r.ImplementStage(op.StageAuthorize, mark("authorize"))
	})
	_, ops := bootOps(t, mod)
	x := ops.Lookup("greet.secret")
	req := &op.Request{Decode: func(any) error { log = append(log, "decode"); return nil }}
	if _, err := x.Invoke(context.Background(), &op.Call{Desc: x.Desc, Request: req}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "pre_authorize,decode,shape,authorize,meaning,handle" {
		t.Fatalf("order: %s", got)
	}
}

func TestShapeViolationsStopTheCall(t *testing.T) {
	required := op.ValidatorFunc[GreetIn](func(_ context.Context, in *GreetIn) *op.ValidationError {
		if in.Name == "" {
			return (*op.ValidationError)(nil).Add("/name", "required", nil)
		}
		return nil
	})
	handled := false
	_, ops := bootOps(t, opsModule(func(r *galleon.Registry) {
		r.Operation(op.Unary[GreetIn, GreetOut]("greet.hello").Public().Validate(required), func(context.Context, *GreetIn) (*GreetOut, error) {
			handled = true
			return &GreetOut{}, nil
		})
	}))
	_, err := ops.Call(context.Background(), "greet.hello", &GreetIn{})
	e := errs.From(err)
	if e.Kind != errs.InvalidArgument || e.Details[0].(errs.FieldViolations)[0].Path != "/name" || handled {
		t.Fatalf("got %v (handled %v)", err, handled)
	}
}

func TestInterceptorsWrapEveryOperation(t *testing.T) {
	var seen []string
	_, ops := bootOps(t, opsModule(func(r *galleon.Registry) {
		r.Operation(Greet, greet)
		r.Intercept(op.After(op.StageHandle), func(next op.Invoker) op.Invoker {
			return func(ctx context.Context, c *op.Call) (any, error) {
				out, err := next(ctx, c)
				seen = append(seen, c.Desc.ID+":"+out.(*GreetOut).Greeting)
				return out, err
			}
		})
	}))
	if _, err := ops.Call(context.Background(), "greet.hello", &GreetIn{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "greet.hello:hello x" {
		t.Fatalf("seen: %v", seen)
	}
	if steps := ops.Lookup("greet.hello").Steps; !strings.HasPrefix(steps[len(steps)-2], "after handle: greet (") {
		t.Fatalf("the interceptor must be listed with its module: %v", steps)
	}
}

func TestTimeoutPolicySetsADeadline(t *testing.T) {
	var deadline time.Time
	_, ops := bootOps(t, opsModule(func(r *galleon.Registry) {
		r.Operation(op.Unary[GreetIn, GreetOut]("greet.hello").Public().Timeout(time.Minute), func(ctx context.Context, _ *GreetIn) (*GreetOut, error) {
			deadline, _ = ctx.Deadline()
			return &GreetOut{}, nil
		})
	}))
	if _, err := ops.Call(context.Background(), "greet.hello", &GreetIn{}); err != nil {
		t.Fatal(err)
	}
	if d := time.Until(deadline); d <= 0 || d > time.Minute {
		t.Fatalf("deadline in %v", d)
	}
}

func TestRateLimitUsesCoordinationCounters(t *testing.T) {
	limited := op.Unary[GreetIn, GreetOut]("greet.hello").Public().RateLimit(2, time.Hour)
	_, ops := bootOps(t, opsModule(func(r *galleon.Registry) { r.Operation(limited, greet) }))
	x := ops.Lookup("greet.hello")
	call := func(peer string) error {
		_, err := x.Invoke(context.Background(), &op.Call{Desc: x.Desc, Input: &GreetIn{}, Request: &op.Request{Peer: peer}})
		return err
	}
	for i := 0; i < 2; i++ {
		if err := call("10.0.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	e := errs.From(call("10.0.0.1"))
	if e.Kind != errs.TooManyRequests || e.Details[0].(errs.RetryInfo).After != time.Hour {
		t.Fatalf("the third call must be limited with retry info, got %v", e)
	}
	if err := call("10.0.0.2"); err != nil {
		t.Fatalf("another peer has its own limit, got %v", err)
	}
}

func TestRateLimitKeyCanSkip(t *testing.T) {
	limited := op.Unary[GreetIn, GreetOut]("greet.hello").Public().RateLimitBy(1, time.Hour, func(*op.Call) string { return "" })
	_, ops := bootOps(t, opsModule(func(r *galleon.Registry) { r.Operation(limited, greet) }))
	for i := 0; i < 3; i++ {
		if _, err := ops.Call(context.Background(), "greet.hello", &GreetIn{}); err != nil {
			t.Fatalf("an empty key skips the limit, got %v", err)
		}
	}
}

// noCounters is a backend that doesn't provide counters.
type noCounters struct{ coord.Backend }

func (b noCounters) Capabilities() coord.Capabilities {
	c := b.Backend.Capabilities()
	var keep []coord.Capability
	for _, p := range c.Provides {
		if p != coord.CountersCap {
			keep = append(keep, p)
		}
	}
	c.Provides = keep
	return c
}

func TestRateLimitNeedsTheCountersCapability(t *testing.T) {
	limited := op.Unary[GreetIn, GreetOut]("greet.hello").Public().RateLimit(2, time.Hour)
	opts := append(base(opsModule(func(r *galleon.Registry) { r.Operation(limited, greet) })), galleon.Coordination(noCounters{local.New()}))
	_, err := galleon.New(opts...)
	if err == nil || !strings.Contains(err.Error(), "counters") {
		t.Fatalf("want a boot error about counters, got %v", err)
	}
}

// failingCounters is a backend whose counters always fail.
type failingCounters struct{ coord.Backend }
type brokenCounters struct{}

func (failingCounters) Counters() coord.Counters { return brokenCounters{} }
func (brokenCounters) Add(context.Context, string, int64, time.Duration) (int64, error) {
	return 0, errors.New("backend down")
}

func TestRateLimitLetsCallsThroughWhenCountingFails(t *testing.T) {
	limited := op.Unary[GreetIn, GreetOut]("greet.hello").Public().RateLimit(1, time.Hour)
	opts := append(base(opsModule(func(r *galleon.Registry) { r.Operation(limited, greet) })), galleon.Coordination(failingCounters{local.New()}))
	app, err := galleon.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	ops := galleon.MustResolve[*galleon.Operations](app)
	for i := 0; i < 3; i++ {
		if _, err := ops.Call(context.Background(), "greet.hello", &GreetIn{}); err != nil {
			t.Fatalf("a counting failure must not reject calls, got %v", err)
		}
	}
}

func TestOperationsCannotBeReplaced(t *testing.T) {
	opts := append(base(), galleon.Replace(&galleon.Operations{}))
	if _, err := galleon.New(opts...); err == nil || !strings.Contains(err.Error(), "the kernel provides") {
		t.Fatalf("got %v", err)
	}
}
