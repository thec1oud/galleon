package galleon

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/validate"
)

// Operation is one registered operation, with its pipeline prepared at boot.
type Operation struct {
	Desc   *op.Descriptor
	Module string
	Site   string
	// Invoke runs the whole pipeline for a call. Transports build the Call
	// and render what it returns.
	Invoke op.Invoker
	// Steps lists the pipeline's interceptors and stages, outermost first.
	Steps []string

	inType reflect.Type // *In
}

// Operations is every operation in the app. The kernel provides it, so
// transports can take it as a constructor parameter.
type Operations struct {
	all  []*Operation
	byID map[string]*Operation
}

// All returns every operation, in registration order.
func (o *Operations) All() []*Operation { return o.all }

// Lookup returns the operation with the given ID, or nil.
func (o *Operations) Lookup(id string) *Operation { return o.byID[id] }

// Call invokes the operation id in process with an already decoded input, a
// *In, through the whole pipeline. It is for tests and in-process callers;
// transports build their own Call.
func (o *Operations) Call(ctx context.Context, id string, in any) (any, error) {
	x := o.byID[id]
	if x == nil {
		return nil, errs.New(errs.NotFound, "galleon.unknown_operation", "no such operation")
	}
	if reflect.TypeOf(in) != x.inType || reflect.ValueOf(in).IsNil() {
		return nil, errs.New(errs.Internal, "galleon.wrong_input", fmt.Sprintf("operation %q takes a non-nil %v, not %T", id, x.inType, in))
	}
	return x.Invoke(ctx, &op.Call{Desc: x.Desc, Input: in, Request: &op.Request{Transport: "local"}})
}

type registeredOp struct {
	desc    op.Descriptor
	handler op.Handler
	module  string
	site    string
}

type stageImpl struct {
	i      op.Interceptor
	module string
	site   string
}

// Operation registers an operation and its handler, which must be a
// func(context.Context, *In) (*Out, error) matching the descriptor's types.
func (r *Registry) Operation(def op.Definition, handler any) {
	if def == nil || (reflect.ValueOf(def).Kind() == reflect.Pointer && reflect.ValueOf(def).IsNil()) {
		r.errorf("Operation: the definition is nil")
		return
	}
	d := def.Describe()
	bad := d.Check()
	var h op.Handler
	if len(bad) == 0 {
		if err := checkHandler(&d, handler); err != nil {
			bad = append(bad, fmt.Errorf("operation %q: %w", d.ID, err))
		} else if h, err = d.Adapt(handler); err != nil {
			bad = append(bad, fmt.Errorf("operation %q: %w", d.ID, err))
		}
	}
	if !d.Generated && d.Input.Type != nil && len(bad) == 0 {
		builtin, err := validate.ForOperation(d.Input.Type)
		switch {
		case err != nil:
			bad = append(bad, fmt.Errorf("operation %q: %w", d.ID, err))
		case builtin != nil:
			custom := d.Shape
			d.Shape = builtin
			if custom != nil {
				d.Shape = func(ctx context.Context, in any) *op.ValidationError {
					return builtin(ctx, in).Merge(custom(ctx, in))
				}
			}
		}
	}
	for _, err := range bad {
		r.errorf("%v", err)
	}
	if len(bad) > 0 {
		return
	}
	if d.Policies.RateLimit != nil {
		r.Needs(coord.CountersCap)
	}
	r.app.ops = append(r.app.ops, registeredOp{desc: d, handler: h, module: r.module, site: callerSite(2)})
}

// checkHandler compares the handler with the descriptor's own types, so a
// hand-written descriptor whose adapter disagrees with its TypeInfo fails boot
// instead of the first call.
func checkHandler(d *op.Descriptor, handler any) error {
	for _, ti := range []op.TypeInfo{d.Input, d.Output} {
		if got, want := reflect.TypeOf(ti.New()), reflect.PointerTo(ti.Type); got != want {
			return fmt.Errorf("TypeInfo for %v has a New that returns %v, want %v (use op.TypeOf)", ti.Type, got, want)
		}
	}
	ht := reflect.TypeOf(handler)
	want := reflect.FuncOf(
		[]reflect.Type{contextType, reflect.PointerTo(d.Input.Type)},
		[]reflect.Type{reflect.PointerTo(d.Output.Type), errorType}, false)
	switch {
	case ht != want:
		return fmt.Errorf("handler is %v, want %v", ht, want)
	case reflect.ValueOf(handler).IsNil():
		return fmt.Errorf("handler is a nil %v", ht)
	}
	return nil
}

var contextType = reflect.TypeFor[context.Context]()

// Intercept attaches an interceptor before or after a stage of every
// operation's pipeline.
//
// Interceptors at Before and After a stage that acts on the way in (route to
// validate meaning) run before and after that stage's work. Route has already
// matched by the time the pipeline starts, so Before(StageRoute) is simply the
// outermost position. Commit hooks and encode act on the way out: an After
// interceptor there sees the output after the stage did its work, a Before
// one sees it first. Before(StageHandle) and After(StageHandle) both wrap the
// handler directly; After is the inner one, so it sees the output first. See
// op.Compile for the full order.
func (r *Registry) Intercept(at op.Position, i op.Interceptor) {
	site := callerSite(2)
	switch {
	case !at.Stage.Valid() || (at.Where != op.BeforeStage && at.Where != op.AfterStage):
		r.errorf("Intercept: invalid position %v", at)
		return
	case i == nil:
		r.errorf("Intercept: the interceptor is nil")
		return
	}
	r.app.interceptors = append(r.app.interceptors, op.Placed{At: at, Name: r.module + " (" + site + ")", I: i})
}

// implementable lists the stages a module may implement. The kernel owns the
// others.
var implementable = map[op.Stage]bool{
	op.StageAuthenticate:   true,
	op.StageResolveContext: true,
	op.StageGate:           true,
	op.StagePreAuthorize:   true,
	op.StageAuthorize:      true,
	op.StageCommitHooks:    true,
	op.StageEncode:         true,
}

// ImplementStage installs the implementation of a pipeline stage, such as
// authentication or authorization. Each stage has at most one implementation.
func (r *Registry) ImplementStage(s op.Stage, i op.Interceptor) {
	site := callerSite(2)
	switch {
	case !implementable[s]:
		r.errorf("ImplementStage: the %v stage is implemented by the kernel", s)
		return
	case i == nil:
		r.errorf("ImplementStage(%v): the implementation is nil", s)
		return
	}
	if prev, ok := r.app.stageImpls[s]; ok {
		r.errorf("the %v stage is already implemented by module %q (%s)", s, prev.module, prev.site)
		return
	}
	r.app.stageImpls[s] = stageImpl{i: i, module: r.module, site: site}
}

// compileOperations checks operations against each other and against the
// installed stages, then prepares every pipeline.
func (a *App) compileOperations() {
	type claim struct{ id, site string }
	ids := map[string]claim{}
	bindings := map[string]claim{}
	mux := http.NewServeMux()
	httpOwners := map[string]string{} // pattern -> operation ID
	needStage := func(o registeredOp, s op.Stage, why string) {
		if _, ok := a.stageImpls[s]; !ok {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("module %q (%s): operation %q %s, but no module implements the %v stage", o.module, o.site, o.desc.ID, why, s))
		}
	}
	for _, o := range a.ops {
		if prev, ok := ids[o.desc.ID]; ok {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("module %q (%s): operation %q is already registered at %s", o.module, o.site, o.desc.ID, prev.site))
			continue
		}
		ids[o.desc.ID] = claim{o.desc.ID, o.site}
		for _, b := range o.desc.Bindings {
			if hb, ok := b.(op.HTTP); ok {
				// ServeMux knows which patterns conflict, so ask it now
				// rather than when the transport mounts them.
				if err := probeHTTP(mux, hb); err != nil {
					a.bootErrs = append(a.bootErrs, fmt.Errorf("module %q (%s): operation %q binds %v, which conflicts with %s", o.module, o.site, o.desc.ID, b, httpOwner(err, httpOwners)))
					continue
				}
				httpOwners[hb.String()] = o.desc.ID
				continue
			}
			key := bindingKey(b)
			if prev, ok := bindings[key]; ok {
				a.bootErrs = append(a.bootErrs, fmt.Errorf("module %q (%s): operation %q binds %v, which operation %q already binds", o.module, o.site, o.desc.ID, b, prev.id))
				continue
			}
			bindings[key] = claim{o.desc.ID, o.site}
		}
		sec := o.desc.Security
		if !sec.Public {
			needStage(o, op.StagePreAuthorize, "requires permission for "+sec.Action)
		}
		if sec.Resource != nil {
			needStage(o, op.StageAuthorize, "checks access to a "+sec.Resource.Kind)
		}
		if len(sec.Schemes) > 0 {
			needStage(o, op.StageAuthenticate, "restricts authentication schemes")
		}
		if len(sec.Contexts) > 0 {
			needStage(o, op.StageResolveContext, "accepts only some operating contexts")
			needStage(o, op.StageGate, "accepts only some operating contexts")
		}
	}

	stages := map[op.Stage]op.Interceptor{
		op.StageAdmit:           a.admit,
		op.StageDecode:          decodeStage,
		op.StageValidateShape:   validateStage(func(d *op.Descriptor) func(context.Context, any) *op.ValidationError { return d.Shape }),
		op.StageValidateMeaning: validateStage(func(d *op.Descriptor) func(context.Context, any) *op.ValidationError { return d.Meaning }),
	}
	for s, impl := range a.stageImpls {
		stages[s] = impl.i
	}
	for _, o := range a.ops {
		if _, dup := a.operations.byID[o.desc.ID]; dup {
			continue
		}
		d := o.desc
		inv, steps := op.Compile(o.handler, op.Plan{Stages: stages, Placed: a.interceptors})
		x := &Operation{Desc: &d, Module: o.module, Site: o.site, Steps: steps, inType: reflect.PointerTo(d.Input.Type)}
		// The invoker always runs this operation's descriptor, whatever the
		// caller put in the call.
		x.Invoke = func(ctx context.Context, call *op.Call) (any, error) {
			call.Desc = x.Desc
			if call.Request == nil {
				call.Request = &op.Request{}
			}
			return inv(ctx, call)
		}
		a.operations.all = append(a.operations.all, x)
		a.operations.byID[d.ID] = x
	}
}

// probeHTTP registers b on a scratch ServeMux, turning its panic on a
// conflicting pattern into an error.
func probeHTTP(mux *http.ServeMux, b op.HTTP) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	mux.Handle(b.String(), http.NotFoundHandler())
	return nil
}

// httpOwner names the operation that owns the pattern ServeMux's error
// mentions, or repeats the error when it can't tell.
func httpOwner(err error, owners map[string]string) string {
	msg := err.Error()
	for pattern, id := range owners {
		if strings.Contains(msg, `"`+pattern+`"`) {
			return fmt.Sprintf("%s of operation %q", pattern, id)
		}
	}
	return msg
}

func bindingKey(b op.Binding) string {
	switch b := b.(type) {
	case op.RPC:
		return "rpc " + b.Service + "/" + b.Method
	case op.Topic:
		return "topic " + b.Name
	}
	return fmt.Sprintf("%T", b)
}

// admit applies the operation's timeout and rate limit. The limit is kept per
// caller IP, or per key when the operation sets one; a call with no key, such
// as one made in process, isn't limited. A coordination error while counting
// lets the call through, so an outage of the backend doesn't take down every
// rate-limited operation; it is logged.
func (a *App) admit(next op.Invoker) op.Invoker {
	return func(ctx context.Context, call *op.Call) (any, error) {
		p := &call.Desc.Policies
		if p.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, p.Timeout)
			defer cancel()
		}
		if rl := p.RateLimit; rl != nil {
			key := call.Request.Peer
			if rl.Key != nil {
				key = rl.Key(call)
			}
			if key != "" {
				n, err := a.backend.Counters().Add(ctx, "galleon/ratelimit/"+call.Desc.ID+"/"+key, 1, rl.Window)
				switch {
				case err != nil:
					a.log.WarnContext(ctx, "rate limit not checked: counting failed", "operation", call.Desc.ID, "error", err)
				case n > rl.Limit:
					return nil, errs.New(errs.TooManyRequests, "galleon.rate_limited", "too many requests", errs.RetryInfo{After: rl.Window})
				}
			}
		}
		return next(ctx, call)
	}
}

func decodeStage(next op.Invoker) op.Invoker {
	return func(ctx context.Context, call *op.Call) (any, error) {
		if call.Input == nil {
			if call.Request == nil || call.Request.Decode == nil {
				return nil, errs.New(errs.Internal, "galleon.no_decoder", "")
			}
			in := call.Desc.Input.New()
			if err := call.Request.Decode(in); err != nil {
				return nil, err
			}
			call.Input = in
		}
		return next(ctx, call)
	}
}

func validateStage(pick func(*op.Descriptor) func(context.Context, any) *op.ValidationError) op.Interceptor {
	return func(next op.Invoker) op.Invoker {
		return func(ctx context.Context, call *op.Call) (any, error) {
			if v := pick(call.Desc); v != nil {
				if err := v(ctx, call.Input).Err(); err != nil {
					return nil, err
				}
			}
			return next(ctx, call)
		}
	}
}

// Middleware adds outer HTTP middleware, which runs before routing for every
// request the HTTP transport serves. It suits concerns that don't depend on
// the operation, such as CORS or compression; any func(http.Handler)
// http.Handler from the Go ecosystem works. Middleware runs in registration
// order, earliest outermost.
func (r *Registry) Middleware(mw func(http.Handler) http.Handler) {
	if mw == nil {
		r.errorf("Middleware: the middleware is nil")
		return
	}
	r.app.middleware.list = append(r.app.middleware.list, mw)
}

// HTTPMiddleware is the outer middleware modules registered. The kernel
// provides it for the HTTP transport.
type HTTPMiddleware struct {
	list []func(http.Handler) http.Handler
}

// Wrap returns h wrapped in every middleware, earliest outermost.
func (m *HTTPMiddleware) Wrap(h http.Handler) http.Handler {
	for i := len(m.list) - 1; i >= 0; i-- {
		h = m.list[i](h)
	}
	return h
}
