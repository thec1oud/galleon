package security

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/op"
)

// Option configures the security module. Packages with authenticators of
// their own, such as security/jwt, offer options too.
type Option func(*Setup)

// ObligationFunc fulfils an obligation of an allowing decision. An error
// denies the call.
type ObligationFunc func(ctx context.Context, q Query, o Obligation) error

// Setup is the security module's configuration while it registers. Options
// receive it; most apps never touch it directly.
type Setup struct {
	r           *galleon.Registry
	auths       []*part
	resolver    *part
	providers   []*part
	decider     *part
	obligations map[string]ObligationFunc
	errs        []error
}

// Registry returns the registry of the security module, so an option can
// load config or add readiness gates and lifecycle hooks.
func (s *Setup) Registry() *galleon.Registry { return s.r }

// Apply applies opts.
func (s *Setup) Apply(opts ...Option) {
	for _, o := range opts {
		if o == nil {
			s.Fail(errors.New("an option is nil"))
			continue
		}
		o(s)
	}
}

// Fail records a configuration problem, which fails boot.
func (s *Setup) Fail(err error) { s.errs = append(s.errs, err) }

// part is one authenticator, resolver, provider or decider: a value, or a
// constructor the module calls at start with values from dependency
// injection.
type part struct {
	kind string // "authenticator", …, for messages
	name string // the scheme or provider name, if it has one
	want reflect.Type
	site string

	value any           // the part, once known
	ctor  reflect.Value // its constructor, if it has one
}

var (
	authenticatorType = reflect.TypeFor[Authenticator]()
	resolverType      = reflect.TypeFor[ContextResolver]()
	providerType      = reflect.TypeFor[ContextProvider]()
	deciderType       = reflect.TypeFor[Decider]()
	errorType         = reflect.TypeFor[error]()
	namePattern       = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
)

// Authenticate adds authenticators, which every operation accepts unless it
// names the schemes it does. They are tried in the order added, and no two
// may have one scheme.
func Authenticate(as ...Authenticator) Option {
	site := callerSite()
	return func(s *Setup) {
		for _, a := range as {
			if isNil(a) {
				s.Fail(fmt.Errorf("%s: Authenticate: an authenticator is nil", site))
				continue
			}
			s.addAuth(&part{kind: "authenticator", name: a.Scheme(), want: authenticatorType, site: site, value: a})
		}
	}
}

// AuthenticateWith adds an authenticator for scheme, which the module builds
// at start by calling ctor, a function whose parameters are provided types,
// as galleon.Provide's are, and which returns an Authenticator, or one and
// an error. Its Scheme must be scheme.
func AuthenticateWith(scheme string, ctor any) Option {
	site := callerSite()
	return func(s *Setup) {
		if p := s.ctorPart("authenticator", scheme, authenticatorType, ctor, site); p != nil {
			s.addAuth(p)
		}
	}
}

func (s *Setup) addAuth(p *part) {
	if !namePattern.MatchString(p.name) {
		s.Fail(fmt.Errorf("%s: authentication scheme %q must be lower case, such as \"bearer\"", p.site, p.name))
		return
	}
	for _, q := range s.auths {
		if q.name == p.name {
			s.Fail(fmt.Errorf("%s: scheme %q already has an authenticator (%s)", p.site, p.name, q.site))
			return
		}
	}
	s.auths = append(s.auths, p)
}

// ResolveContext sets the context resolver. Without one, the module uses
// IdentityResolver, and an operation that names the contexts it accepts
// fails boot.
func ResolveContext(r ContextResolver) Option {
	site := callerSite()
	return func(s *Setup) {
		if isNil(r) {
			s.Fail(fmt.Errorf("%s: ResolveContext: the resolver is nil", site))
			return
		}
		s.setResolver(&part{kind: "context resolver", want: resolverType, site: site, value: r})
	}
}

// ResolveContextWith sets the context resolver, which the module builds at
// start by calling ctor, as AuthenticateWith does.
func ResolveContextWith(ctor any) Option {
	site := callerSite()
	return func(s *Setup) {
		if p := s.ctorPart("context resolver", "", resolverType, ctor, site); p != nil {
			s.setResolver(p)
		}
	}
}

func (s *Setup) setResolver(p *part) {
	if s.resolver != nil {
		s.Fail(fmt.Errorf("%s: the context resolver is already set (%s)", p.site, s.resolver.site))
		return
	}
	s.validate(p)
	s.resolver = p
}

// Input adds context providers, which operations name with
// op.Builder.Inputs. No two may have one name, and none may take a built-in
// provider's name (InputRequest, InputTime).
func Input(ps ...ContextProvider) Option {
	site := callerSite()
	return func(s *Setup) {
		for _, p := range ps {
			if isNil(p) {
				s.Fail(fmt.Errorf("%s: Input: a provider is nil", site))
				continue
			}
			s.addProvider(&part{kind: "context provider", name: p.Name(), want: providerType, site: site, value: p})
		}
	}
}

// InputWith adds a context provider named name, which the module builds at
// start by calling ctor, as AuthenticateWith does. Its Name must be name.
func InputWith(name string, ctor any) Option {
	site := callerSite()
	return func(s *Setup) {
		if p := s.ctorPart("context provider", name, providerType, ctor, site); p != nil {
			s.addProvider(p)
		}
	}
}

func (s *Setup) addProvider(p *part) {
	switch {
	case !namePattern.MatchString(p.name):
		s.Fail(fmt.Errorf("%s: context provider name %q must be lower case, such as \"billing\"", p.site, p.name))
		return
	case p.name == InputRequest || p.name == InputTime:
		s.Fail(fmt.Errorf("%s: context provider name %q is a built-in provider's", p.site, p.name))
		return
	}
	for _, q := range s.providers {
		if q.name == p.name {
			s.Fail(fmt.Errorf("%s: context provider %q is already added (%s)", p.site, p.name, q.site))
			return
		}
	}
	s.providers = append(s.providers, p)
}

// Decide sets the decider. Without one, any operation that isn't Public
// fails boot.
func Decide(d Decider) Option {
	site := callerSite()
	return func(s *Setup) {
		if isNil(d) {
			s.Fail(fmt.Errorf("%s: Decide: the decider is nil", site))
			return
		}
		s.setDecider(&part{kind: "decider", want: deciderType, site: site, value: d})
	}
}

// DecideWith sets the decider, which the module builds at start by calling
// ctor, as AuthenticateWith does.
func DecideWith(ctor any) Option {
	site := callerSite()
	return func(s *Setup) {
		if p := s.ctorPart("decider", "", deciderType, ctor, site); p != nil {
			s.setDecider(p)
		}
	}
}

func (s *Setup) setDecider(p *part) {
	if s.decider != nil {
		s.Fail(fmt.Errorf("%s: the decider is already set (%s)", p.site, s.decider.site))
		return
	}
	s.validate(p)
	s.decider = p
}

// HandleObligation registers fn to fulfil the obligation named name, when
// an allowing decision carries it. A decision with an obligation that has
// no handler denies.
func HandleObligation(name string, fn ObligationFunc) Option {
	site := callerSite()
	return func(s *Setup) {
		switch {
		case name == "" || fn == nil:
			s.Fail(fmt.Errorf("%s: HandleObligation needs a name and a function", site))
		case s.obligations[name] != nil:
			s.Fail(fmt.Errorf("%s: obligation %q already has a handler", site, name))
		default:
			s.obligations[name] = fn
		}
	}
}

// validate runs a value part's Validate method, if it has one, so a bad
// RoleTable or ClaimResolver fails boot.
func (s *Setup) validate(p *part) {
	if v, ok := p.value.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			s.Fail(fmt.Errorf("%s: %w", p.site, err))
		}
	}
}

// ctorPart checks a part's constructor, as galleon.Provide would.
func (s *Setup) ctorPart(kind, name string, want reflect.Type, ctor any, site string) *part {
	cv := reflect.ValueOf(ctor)
	if !cv.IsValid() || cv.Kind() != reflect.Func || cv.IsNil() {
		s.Fail(fmt.Errorf("%s: the %s's constructor must be a function, not %T", site, kind, ctor))
		return nil
	}
	ct := cv.Type()
	switch {
	case ct.IsVariadic():
		s.Fail(fmt.Errorf("%s: the %s's constructor must not be variadic", site, kind))
	case ct.NumOut() == 0 || ct.NumOut() > 2 || (ct.NumOut() == 2 && ct.Out(1) != errorType):
		s.Fail(fmt.Errorf("%s: the %s's constructor must return a %v, or one and an error", site, kind, want))
	case !ct.Out(0).Implements(want):
		s.Fail(fmt.Errorf("%s: the %s's constructor returns %v, which isn't a %v", site, kind, ct.Out(0), want))
	default:
		return &part{kind: kind, name: name, want: want, site: site, ctor: cv}
	}
	return nil
}

// Module returns the security module. It fills the pipeline's
// authenticate, resolve context, gate, pre-authorize and authorize stages,
// prepared for each operation at boot. Boot fails when an operation names a
// scheme no authenticator has, accepts contexts without a resolver set,
// names a provider the module doesn't have, needs a decision without a
// decider set, or names a resource ID field its input lacks.
func Module(opts ...Option) galleon.Module {
	return galleon.ModuleFunc("security", func(r *galleon.Registry) error {
		s := &Setup{r: r, obligations: map[string]ObligationFunc{}}
		s.Apply(opts...)
		if len(s.errs) > 0 {
			return errors.Join(s.errs...)
		}
		m := &module{setup: s}
		galleon.Provide[*engine](r, m.constructor())
		r.PrepareStage(op.StageAuthenticate, m.prepareAuthenticate)
		r.PrepareStage(op.StageResolveContext, m.prepareResolve)
		r.PrepareStage(op.StageGate, m.prepareGate)
		r.PrepareStage(op.StagePreAuthorize, m.preparePreAuthorize)
		r.PrepareStage(op.StageAuthorize, m.prepareAuthorize)
		return nil
	})
}

// module is the security module as registered: its setup, and the engine
// once built.
type module struct {
	setup  *Setup
	engine atomic.Pointer[engine]
}

// constructor returns the engine's constructor for dependency injection:
// its parameters are the clock, the logger and every parameter of the
// parts' constructors, so a missing dependency fails boot as any other.
func (m *module) constructor() any {
	params := []reflect.Type{reflect.TypeFor[clock.Clock](), reflect.TypeFor[*slog.Logger]()}
	index := map[reflect.Type]int{params[0]: 0, params[1]: 1}
	for _, p := range m.setup.parts() {
		if !p.ctor.IsValid() {
			continue
		}
		for i := 0; i < p.ctor.Type().NumIn(); i++ {
			t := p.ctor.Type().In(i)
			if _, ok := index[t]; !ok {
				index[t] = len(params)
				params = append(params, t)
			}
		}
	}
	ft := reflect.FuncOf(params, []reflect.Type{reflect.TypeFor[*engine](), errorType}, false)
	return reflect.MakeFunc(ft, func(args []reflect.Value) []reflect.Value {
		e, err := m.build(args, index)
		if err != nil {
			return []reflect.Value{reflect.Zero(reflect.TypeFor[*engine]()), reflect.ValueOf(&err).Elem()}
		}
		m.engine.Store(e)
		return []reflect.Value{reflect.ValueOf(e), reflect.Zero(errorType)}
	}).Interface()
}

// parts returns every part, in a fixed order.
func (s *Setup) parts() []*part {
	ps := append([]*part(nil), s.auths...)
	ps = append(ps, s.providers...)
	if s.resolver != nil {
		ps = append(ps, s.resolver)
	}
	if s.decider != nil {
		ps = append(ps, s.decider)
	}
	return ps
}

// build calls the parts' constructors and assembles the engine.
func (m *module) build(args []reflect.Value, index map[reflect.Type]int) (*engine, error) {
	s := m.setup
	e := &engine{
		clock:       args[0].Interface().(clock.Clock),
		log:         args[1].Interface().(*slog.Logger),
		providers:   map[string]ContextProvider{},
		obligations: s.obligations,
	}
	var errs []error
	value := func(p *part) any {
		if !p.ctor.IsValid() {
			return p.value
		}
		in := make([]reflect.Value, p.ctor.Type().NumIn())
		for i := range in {
			in[i] = args[index[p.ctor.Type().In(i)]]
		}
		out := p.ctor.Call(in)
		if len(out) == 2 && !out[1].IsNil() {
			errs = append(errs, fmt.Errorf("security: constructing the %s (%s): %w", p.kind, p.site, out[1].Interface().(error)))
			return nil
		}
		v := out[0].Interface()
		if isNil(v) {
			errs = append(errs, fmt.Errorf("security: the %s's constructor (%s) returned nil", p.kind, p.site))
			return nil
		}
		return v
	}
	for _, p := range s.auths {
		v := value(p)
		if v == nil {
			continue
		}
		a := v.(Authenticator)
		if a.Scheme() != p.name {
			errs = append(errs, fmt.Errorf("security: the authenticator for scheme %q (%s) has scheme %q", p.name, p.site, a.Scheme()))
			continue
		}
		e.auths = append(e.auths, a)
		if c, ok := a.(Challenger); ok {
			e.challenges = append(e.challenges, c.Challenge())
		} else {
			e.challenges = append(e.challenges, "")
		}
		e.started = append(e.started, a)
	}
	e.providers[InputRequest] = requestProvider{}
	e.providers[InputTime] = timeProvider{clock: e.clock}
	for _, p := range s.providers {
		v := value(p)
		if v == nil {
			continue
		}
		cp := v.(ContextProvider)
		if cp.Name() != p.name {
			errs = append(errs, fmt.Errorf("security: the context provider %q (%s) is named %q", p.name, p.site, cp.Name()))
			continue
		}
		e.providers[p.name] = cp
		e.started = append(e.started, cp)
	}
	e.resolver = ContextResolver(IdentityResolver{})
	if s.resolver != nil {
		if v := value(s.resolver); v != nil {
			e.resolver = v.(ContextResolver)
			e.started = append(e.started, v)
			if sl, ok := v.(Slow); ok && sl.Slow() {
				e.log.Info("security: the context resolver may do I/O on every call", "resolver", fmt.Sprintf("%T", v))
			}
		}
	}
	if s.decider != nil {
		if v := value(s.decider); v != nil {
			e.decider = v.(Decider)
			e.started = append(e.started, v)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return e, nil
}

func (m *module) schemeIndex(name string) int {
	for i, p := range m.setup.auths {
		if p.name == name {
			return i
		}
	}
	return -1
}

func (m *module) schemes() string {
	if len(m.setup.auths) == 0 {
		return "it has none"
	}
	names := make([]string, len(m.setup.auths))
	for i, p := range m.setup.auths {
		names[i] = p.name
	}
	return "it has " + strings.Join(names, ", ")
}

// isNil reports whether v is nil, or a nil pointer, func, map or the like
// in an interface.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Func, reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// callerSite returns file:line of the call of the function that called it.
func callerSite() string {
	_, file, line, ok := runtime.Caller(2)
	if !ok {
		return "unknown"
	}
	if i := strings.LastIndex(file, "/"); i >= 0 {
		file = file[i+1:]
	}
	return fmt.Sprintf("%s:%d", file, line)
}
