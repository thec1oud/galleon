package security

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

// engine is the security module's parts, built at start.
type engine struct {
	clock       clock.Clock
	log         *slog.Logger
	auths       []Authenticator // in the order added
	challenges  []string        // auths' challenges, "" for none
	resolver    ContextResolver
	providers   map[string]ContextProvider
	decider     Decider // nil if none was set
	obligations map[string]ObligationFunc
	started     []any // the parts, for Start and Stop
}

// Start starts the parts that hold resources, such as a key cache.
func (e *engine) Start(ctx context.Context) error {
	for i, p := range e.started {
		if s, ok := p.(interface{ Start(context.Context) error }); ok && !e.seen(i) {
			if err := s.Start(ctx); err != nil {
				return fmt.Errorf("security: starting %T: %w", p, err)
			}
		}
	}
	return nil
}

// Stop stops them, in reverse order.
func (e *engine) Stop(ctx context.Context) error {
	var errs []error
	for i := len(e.started) - 1; i >= 0; i-- {
		if s, ok := e.started[i].(interface{ Stop(context.Context) error }); ok && !e.seen(i) {
			if err := s.Stop(ctx); err != nil {
				errs = append(errs, fmt.Errorf("security: stopping %T: %w", e.started[i], err))
			}
		}
	}
	return errors.Join(errs...)
}

// seen reports whether the part at i is also at an earlier index, as one
// value set as both resolver and decider is, so it starts once.
func (e *engine) seen(i int) bool {
	p := e.started[i]
	if !reflect.TypeOf(p).Comparable() {
		return false
	}
	for _, q := range e.started[:i] {
		if reflect.TypeOf(q) == reflect.TypeOf(p) && q == p {
			return true
		}
	}
	return false
}

// inProcess reports whether a call was made in process, where the caller's
// principal carries over from ctx, rather than arriving over a network,
// where only the request's credentials count.
func inProcess(r *op.Request) bool { return r.Transport == "local" || r.Transport == "job" }

// notStarted is the error of a call that arrives before the module started.
func notStarted() error {
	return errs.New(errs.Unavailable, CodeUnavailable, "the service isn't ready")
}

// unavailable fails a call whose security part failed: the cause is logged,
// never sent.
func (e *engine) unavailable(ctx context.Context, what string, call *op.Call, err error) error {
	if ctx.Err() != nil {
		return errs.From(ctx.Err())
	}
	e.log.ErrorContext(ctx, "security: "+what+" failed, so the call is denied", "operation", call.Desc.ID, "error", err)
	return errs.Wrap(err, errs.Unavailable, CodeUnavailable, what+" is unavailable")
}

// prepareAuthenticate prepares stage 3 for every operation: it chooses the
// operation's authenticators once, at boot.
func (m *module) prepareAuthenticate(d *op.Descriptor) (op.Interceptor, error) {
	var chain []int
	if len(d.Security.Schemes) == 0 {
		for i := range m.setup.auths {
			chain = append(chain, i)
		}
	}
	var bad []string
	for _, name := range d.Security.Schemes {
		i := m.schemeIndex(name)
		switch {
		case i < 0:
			bad = append(bad, fmt.Sprintf("%q", name))
		case !slices.Contains(chain, i):
			chain = append(chain, i)
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("it accepts the scheme %s, but the security module has no authenticator for it (%s)", strings.Join(bad, ", "), m.schemes())
	}
	return func(next op.Invoker) op.Invoker {
		return func(ctx context.Context, call *op.Call) (any, error) {
			e := m.engine.Load()
			if e == nil {
				return nil, notStarted()
			}
			st := &state{e: e, call: call, chain: chain}
			if p := inherited(ctx); p != nil && inProcess(call.Request) {
				st.principal = p
			} else {
				p, err := e.authenticate(ctx, st)
				if err != nil {
					return nil, err
				}
				st.principal = p
			}
			return next(context.WithValue(ctx, stateKey{}, st), call)
		}
	}, nil
}

// authenticate runs the call's chain of authenticators.
func (e *engine) authenticate(ctx context.Context, st *state) (Principal, error) {
	for _, i := range st.chain {
		p, err := e.auths[i].Authenticate(ctx, st.call.Request)
		switch {
		case errors.Is(err, ErrNoCredentials):
			continue
		case err != nil:
			var ee *errs.Error
			if errors.As(err, &ee) && ee.Kind == errs.Unauthenticated {
				st.challenge(i)
				return nil, err
			}
			return nil, e.unavailable(ctx, "authentication", st.call, fmt.Errorf("scheme %q: %w", e.auths[i].Scheme(), err))
		case IsAnonymous(p):
			continue // as no credentials
		}
		st.scheme = e.auths[i].Scheme()
		return p, nil
	}
	return Anonymous(), nil
}

// prepareResolve prepares stage 4 for every operation: it checks the
// operation's contexts and inputs against the module once, at boot.
func (m *module) prepareResolve(d *op.Descriptor) (op.Interceptor, error) {
	if len(d.Security.Contexts) > 0 && m.setup.resolver == nil {
		return nil, fmt.Errorf("it accepts only the contexts %s, but the security module has no context resolver, so no call could be in one: set one with security.ResolveContext", strings.Join(d.Security.Contexts, ", "))
	}
	var names []string
	for _, name := range d.Security.Inputs {
		if slices.Contains(names, name) {
			continue
		}
		if !m.hasProvider(name) {
			return nil, fmt.Errorf("it names the input %q, but the security module has no context provider of that name", name)
		}
		names = append(names, name)
	}
	return func(next op.Invoker) op.Invoker {
		return func(ctx context.Context, call *op.Call) (any, error) {
			st := stateFrom(ctx)
			if st == nil {
				return nil, notStarted()
			}
			e := st.e
			in := &inputs{e: e, names: names, p: st.principal, r: call.Request}
			oc, err := e.resolver.Resolve(ctx, st.principal, call.Request, in)
			if err != nil {
				var ee *errs.Error
				if errors.As(err, &ee) {
					return nil, err
				}
				return nil, e.unavailable(ctx, "context resolution", call, err)
			}
			if isNil(oc) {
				oc = nil
			}
			if oc != nil {
				if exp := oc.ExpiresAt(); !exp.IsZero() && !e.clock.Now().Before(exp) {
					return nil, errs.New(errs.Unauthenticated, CodeContextExpired, "the operating context has expired")
				}
			}
			st.context = oc
			return next(ctx, call)
		}
	}, nil
}

func (m *module) hasProvider(name string) bool {
	if name == InputRequest || name == InputTime {
		return true
	}
	for _, p := range m.setup.providers {
		if p.name == name {
			return true
		}
	}
	return false
}

// prepareGate prepares stage 5 for the operations that name the contexts
// they accept; the others skip it.
func (m *module) prepareGate(d *op.Descriptor) (op.Interceptor, error) {
	if len(d.Security.Contexts) == 0 {
		return nil, nil
	}
	accepted := slices.Clone(d.Security.Contexts)
	return func(next op.Invoker) op.Invoker {
		return func(ctx context.Context, call *op.Call) (any, error) {
			st := stateFrom(ctx)
			if st == nil {
				return nil, notStarted()
			}
			if st.context == nil || !slices.Contains(accepted, st.context.Domain()) {
				if IsAnonymous(st.principal) {
					return nil, st.unauthenticated()
				}
				return nil, errs.New(errs.PermissionDenied, CodeContextNotAccepted, "the operation isn't available in this context")
			}
			return next(ctx, call)
		}
	}, nil
}

// preparePreAuthorize prepares stage 5b for the operations that aren't
// public; public ones skip it.
func (m *module) preparePreAuthorize(d *op.Descriptor) (op.Interceptor, error) {
	if d.Security.Public {
		return nil, nil
	}
	if m.setup.decider == nil {
		return nil, fmt.Errorf("it requires permission for %q, but the security module has no decider, so it could never be allowed: set one with security.Decide", d.Security.Action)
	}
	action := d.Security.Action
	return func(next op.Invoker) op.Invoker {
		return func(ctx context.Context, call *op.Call) (any, error) {
			st := stateFrom(ctx)
			if st == nil {
				return nil, notStarted()
			}
			if err := st.require(ctx, action, nil); err != nil {
				return nil, err
			}
			return next(ctx, call)
		}
	}, nil
}

// prepareAuthorize prepares stage 8 for the operations that check access to
// a resource, planning how to read its ID from their input; the others skip
// it.
func (m *module) prepareAuthorize(d *op.Descriptor) (op.Interceptor, error) {
	ref := d.Security.Resource
	if ref == nil {
		return nil, nil
	}
	if ref.Kind == "" {
		return nil, errors.New("its resource has no kind")
	}
	id, err := planResourceID(d.Input.Type, ref.Field)
	if err != nil {
		return nil, err
	}
	action, kind, field := d.Security.Action, ref.Kind, ref.Field
	return func(next op.Invoker) op.Invoker {
		return func(ctx context.Context, call *op.Call) (any, error) {
			st := stateFrom(ctx)
			if st == nil {
				return nil, notStarted()
			}
			rid := id(call.Input)
			if rid == "" {
				return nil, (&op.ValidationError{}).Add("/"+field, "required", nil).Err()
			}
			if err := st.require(ctx, action, &Resource{Type: kind, ID: rid}); err != nil {
				return nil, err
			}
			return next(ctx, call)
		}
	}, nil
}

// inputs gives a resolver its operation's providers, each evaluated at most
// once.
type inputs struct {
	e     *engine
	names []string
	p     Principal
	r     *op.Request

	mu   sync.Mutex
	done map[string]Attributes
}

func (in *inputs) Names() []string { return slices.Clone(in.names) }

func (in *inputs) Get(ctx context.Context, name string) (Attributes, error) {
	if !slices.Contains(in.names, name) {
		return Attributes{}, fmt.Errorf("security: the operation doesn't name the input %q (it names %v)", name, in.names)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if a, ok := in.done[name]; ok {
		return a, nil
	}
	a, err := in.e.providers[name].Provide(ctx, in.p, in.r)
	if err != nil {
		return Attributes{}, fmt.Errorf("security: context provider %q: %w", name, err)
	}
	if in.done == nil {
		in.done = map[string]Attributes{}
	}
	in.done[name] = a
	return a, nil
}
