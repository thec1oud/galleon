package op_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

type In struct{ Name string }
type Out struct{ Greeting string }

func hello(_ context.Context, in *In) (*Out, error) { return &Out{Greeting: "hi " + in.Name}, nil }

func TestBuilderDescribesWhatWasDeclared(t *testing.T) {
	b := op.Unary[In, Out]("greet.hello").
		HTTP("POST", "/hello").
		RPC("greet.v1.Greeter", "Hello").
		AuthorizeResource("greet:hello", op.Resource("org", "org_id")).
		Contexts("org").
		Timeout(time.Second).
		BodyLimit(1<<10).
		RateLimit(5, time.Minute).
		Idempotent().
		AllowUnknownFields().
		Errors("greet.blocked").
		Meta("k", "v")
	d := b.Describe()
	if d.ID != "greet.hello" || d.Kind != op.KindUnary {
		t.Fatalf("id/kind: %q %v", d.ID, d.Kind)
	}
	if d.Input.Type != reflect.TypeFor[In]() || d.Output.Type != reflect.TypeFor[Out]() {
		t.Fatalf("types: %v %v", d.Input.Type, d.Output.Type)
	}
	if _, ok := d.Input.New().(*In); !ok {
		t.Fatal("Input.New must return *In")
	}
	want := []op.Binding{op.HTTP{Method: "POST", Path: "/hello"}, op.RPC{Service: "greet.v1.Greeter", Method: "Hello"}}
	if !reflect.DeepEqual(d.Bindings, want) {
		t.Fatalf("bindings: %v", d.Bindings)
	}
	if d.Security.Action != "greet:hello" || *d.Security.Resource != (op.ResourceRef{Kind: "org", Field: "org_id"}) || d.Security.Contexts[0] != "org" {
		t.Fatalf("security: %+v", d.Security)
	}
	p := d.Policies
	if p.Timeout != time.Second || p.BodyLimit != 1<<10 || p.RateLimit.Limit != 5 || !p.Idempotent || p.UnknownFields != op.IgnoreUnknown {
		t.Fatalf("policies: %+v", p)
	}
	if len(d.Check()) != 0 {
		t.Fatalf("a well-formed descriptor has no problems: %v", d.Check())
	}

	// Describe returns a copy: changing it, or the builder afterwards, doesn't
	// change the other.
	d.Bindings[0] = op.HTTP{Method: "GET", Path: "/x"}
	d.Meta["k"] = "changed"
	d.Security.Resource.Kind = "changed"
	d.Policies.RateLimit.Limit = 99
	again := b.Describe()
	if again.Bindings[0] != want[0] || again.Meta["k"] != "v" || again.Security.Resource.Kind != "org" || again.Policies.RateLimit.Limit != 5 {
		t.Fatal("Describe must not share slices, maps or pointers with the builder")
	}
}

func TestHandWrittenDescriptorMatchesBuilder(t *testing.T) {
	built := op.Unary[In, Out]("greet.hello").HTTP("POST", "/hello").Public().Describe()
	hand := op.Descriptor{
		ID:       "greet.hello",
		Kind:     op.KindUnary,
		Bindings: []op.Binding{op.HTTP{Method: "POST", Path: "/hello"}},
		Input:    op.TypeOf[In](),
		Output:   op.TypeOf[Out](),
		Security: op.Security{Public: true},
		Adapt:    op.Adapter[In, Out](),
	}
	for _, d := range []op.Descriptor{built, hand.Describe()} {
		if len(d.Check()) != 0 {
			t.Fatal(d.Check())
		}
		h, err := d.Adapt(hello)
		if err != nil {
			t.Fatal(err)
		}
		out, err := h(context.Background(), &In{Name: "ann"})
		if err != nil || out.(*Out).Greeting != "hi ann" {
			t.Fatalf("got %v, %v", out, err)
		}
	}
}

func TestAdapterRejectsMismatchedHandler(t *testing.T) {
	adapt := op.Adapter[In, Out]()
	for _, h := range []any{
		func(context.Context, *Out) (*Out, error) { return nil, nil },
		func(context.Context, In) (*Out, error) { return nil, nil },
		"not a func",
		nil,
		(func(context.Context, *In) (*Out, error))(nil),
	} {
		if _, err := adapt(h); err == nil {
			t.Errorf("handler %T must be rejected", h)
		}
	}
}

func TestAdapterReturnsNilOutputOnError(t *testing.T) {
	h, _ := op.Adapter[In, Out]()(func(context.Context, *In) (*Out, error) { return &Out{}, errors.New("boom") })
	out, err := h(context.Background(), &In{})
	if err == nil || out != nil {
		t.Fatalf("an error must come with a nil output, got %v", out)
	}
}

func TestCheckReportsProblems(t *testing.T) {
	cases := map[string]op.Descriptor{
		"dotted lower snake case":  op.Unary[In, Out]("Orders").Public().Describe(),
		"declares no security":     op.Unary[In, Out]("orders.create").Describe(),
		"is Public but also":       op.Unary[In, Out]("orders.create").Public().Authorize("a").Describe(),
		"upper-case method and":    op.Unary[In, Out]("orders.create").Public().HTTP("POST", "orders").Describe(),
		"needs a protobuf service": op.Unary[In, Out]("orders.create").Public().RPC("", "Create").Describe(),
		"a method name":            op.Unary[In, Out]("orders.create").Public().RPC("orders.v1.Orders", "Cre/ate").Describe(),
		"positive limit":           op.Unary[In, Out]("orders.create").Public().RateLimit(0, time.Second).Describe(),
		"must be a struct":         {ID: "orders.create", Kind: op.KindUnary, Input: op.TypeOf[[]string](), Output: op.TypeOf[Out](), Security: op.Security{Public: true}, Adapt: op.Adapter[In, Out]()},
		"no handler adapter":       {ID: "orders.create", Kind: op.KindUnary, Input: op.TypeOf[In](), Output: op.TypeOf[Out](), Security: op.Security{Public: true}},
		"input and output types":   {ID: "orders.create", Kind: op.KindUnary, Security: op.Security{Public: true}, Adapt: op.Adapter[In, Out]()},
		"not supported yet":        {ID: "orders.create", Kind: op.KindServerStream, Input: op.TypeOf[In](), Output: op.TypeOf[Out](), Security: op.Security{Public: true}, Adapt: op.Adapter[In, Out]()},
	}
	for want, d := range cases {
		var msgs []string
		for _, err := range d.Check() {
			msgs = append(msgs, err.Error())
		}
		if !strings.Contains(strings.Join(msgs, "; "), want) {
			t.Errorf("want a problem containing %q, got %q", want, msgs)
		}
	}
}

// recorder returns an interceptor that appends name on the way in and
// name+"'" on the way out.
func recorder(log *[]string, name string) op.Interceptor {
	return func(next op.Invoker) op.Invoker {
		return func(ctx context.Context, c *op.Call) (any, error) {
			*log = append(*log, name)
			out, err := next(ctx, c)
			*log = append(*log, name+"'")
			return out, err
		}
	}
}

func TestCompileRunsStagesInFixedOrder(t *testing.T) {
	var log []string
	stages := map[op.Stage]op.Interceptor{}
	for s := op.StageRoute; s <= op.StageEncode; s++ {
		if s != op.StageHandle {
			stages[s] = recorder(&log, s.String())
		}
	}
	placed := []op.Placed{
		{At: op.After(op.StageHandle), Name: "after-handle", I: recorder(&log, "A(handle)")},
		{At: op.Before(op.StageDecode), Name: "b1", I: recorder(&log, "B(decode)1")},
		{At: op.Before(op.StageEncode), Name: "before-encode", I: recorder(&log, "B(encode)")},
		{At: op.After(op.StageEncode), Name: "after-encode", I: recorder(&log, "A(encode)")},
		{At: op.Before(op.StageDecode), Name: "b2", I: recorder(&log, "B(decode)2")},
		{At: op.Before(op.StageHandle), Name: "before-handle", I: recorder(&log, "B(handle)")},
		{At: op.After(op.StageAuthorize), Name: "after-authz", I: recorder(&log, "A(authorize)")},
	}
	h := func(context.Context, any) (any, error) { log = append(log, "HANDLER"); return "out", nil }
	inv, steps := op.Compile(h, op.Plan{Stages: stages, Placed: placed})
	out, err := inv(context.Background(), &op.Call{})
	if err != nil || out != "out" {
		t.Fatalf("got %v, %v", out, err)
	}
	in := []string{
		"route", "admit", "authenticate", "resolve_context", "gate", "pre_authorize",
		"B(decode)1", "B(decode)2", "decode", "validate_shape", "authorize", "A(authorize)", "validate_meaning",
		"A(encode)", "encode", "B(encode)", "commit_hooks", "B(handle)", "A(handle)", "HANDLER",
	}
	if got := strings.Join(log[:len(in)], " "); got != strings.Join(in, " ") {
		t.Fatalf("order on the way in:\n got %s\nwant %s", got, strings.Join(in, " "))
	}
	// On the way out, after-handle sees the output first, then commit hooks,
	// then encode, and the outer stages last.
	outOrder := []string{"A(handle)'", "B(handle)'", "commit_hooks'", "B(encode)'", "encode'", "A(encode)'"}
	if got := strings.Join(log[len(in):len(in)+len(outOrder)], " "); got != strings.Join(outOrder, " ") {
		t.Fatalf("order on the way out: %s", got)
	}
	if steps[len(steps)-1] != "handle" || steps[0] != "route" || len(steps) != len(in) {
		t.Fatalf("steps: %v", steps)
	}
}

func TestCompileSkipsMissingStagesAndPropagatesErrors(t *testing.T) {
	boom := errs.New(errs.PermissionDenied, "x.denied", "no")
	deny := func(op.Invoker) op.Invoker {
		return func(context.Context, *op.Call) (any, error) { return nil, boom }
	}
	called := false
	h := func(context.Context, any) (any, error) { called = true; return nil, nil }
	inv, steps := op.Compile(h, op.Plan{Stages: map[op.Stage]op.Interceptor{op.StagePreAuthorize: deny}})
	if _, err := inv(context.Background(), &op.Call{}); err != boom {
		t.Fatalf("the stage's error must come back unchanged, got %v", err)
	}
	if called {
		t.Fatal("the handler must not run after a stage fails")
	}
	if strings.Join(steps, ",") != "pre_authorize,handle" {
		t.Fatalf("steps: %v", steps)
	}
}

func TestValidationErrorMergeAndErr(t *testing.T) {
	var none *op.ValidationError
	if none.Merge(nil) != nil || none.Err() != nil {
		t.Fatal("nil merges to nil and is no error")
	}
	a := none.Add("/name", "required", nil)
	b := (&op.ValidationError{}).Add("/age", "min", map[string]any{"min": 18})
	m := a.Merge(b)
	if len(m.Violations) != 2 || len(a.Violations) != 1 {
		t.Fatalf("merge must not modify its operands: %+v %+v", m, a)
	}
	e := errs.From(m.Err())
	if e.Kind != errs.InvalidArgument || e.Code != "galleon.invalid_input" {
		t.Fatalf("got %v", e)
	}
	fv := e.Details[0].(errs.FieldViolations)
	if fv[0].Path != "/name" || fv[0].MsgKey != "validation.required" || fv[1].Params["min"] != 18 {
		t.Fatalf("violations: %+v", fv)
	}
}

func TestBuilderChainsValidators(t *testing.T) {
	d := op.Unary[In, Out]("greet.hello").Public().
		Validate(op.ValidatorFunc[In](func(_ context.Context, in *In) *op.ValidationError {
			if in.Name == "" {
				return (*op.ValidationError)(nil).Add("/name", "required", nil)
			}
			return nil
		})).
		Validate(op.ValidatorFunc[In](func(_ context.Context, in *In) *op.ValidationError {
			if len(in.Name) < 3 {
				return (*op.ValidationError)(nil).Add("/name", "min_length", nil)
			}
			return nil
		})).
		Describe()
	if v := d.Shape(context.Background(), &In{}); v == nil || len(v.Violations) != 2 {
		t.Fatalf("both validators must report: %+v", v)
	}
	if v := d.Shape(context.Background(), &In{Name: "bob"}); v != nil {
		t.Fatalf("valid input: %+v", v)
	}
}

func TestCheckRejectsPointerBindings(t *testing.T) {
	d := op.Unary[In, Out]("orders.create").Public().Describe()
	d.Bindings = append(d.Bindings, &op.HTTP{Method: "GET", Path: "/o"})
	errs := d.Check()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "not supported") {
		t.Fatalf("got %v", errs)
	}
}

func TestValidateNilPanicsAtDeclaration(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Validate(nil) must panic")
		}
	}()
	op.Unary[In, Out]("orders.create").Validate(nil)
}
