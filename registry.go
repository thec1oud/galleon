package galleon

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"slices"

	"github.com/thec1oud/galleon/coord"
)

// Module is the unit of encapsulation. Everything a module contributes goes
// through Register; nothing registers itself globally.
type Module interface {
	Name() string
	// Register must only record registrations: no I/O, goroutines or network.
	Register(r *Registry) error
}

// ModuleFunc adapts a function to Module.
func ModuleFunc(name string, register func(r *Registry) error) Module {
	return moduleFunc{name: name, register: register}
}

type moduleFunc struct {
	name     string
	register func(r *Registry) error
}

func (m moduleFunc) Name() string               { return m.name }
func (m moduleFunc) Register(r *Registry) error { return m.register(r) }

// Registry is handed to a module's Register. Every registration is attributed to
// the module and the calling source line, so boot errors say exactly where to look.
type Registry struct {
	app    *App
	module string

	needsCounters bool // the counters capability is declared for a rate-limited operation
}

// Module returns the name of the module that owns this registry.
func (r *Registry) Module() string { return r.module }

func (r *Registry) errorf(format string, args ...any) {
	r.app.bootErrs = append(r.app.bootErrs, fmt.Errorf("module %q (%s): %s", r.module, callerSite(3), fmt.Sprintf(format, args...)))
}

// Needs declares coordination capabilities this module relies on. Boot fails if
// the configured backend can't provide them.
func (r *Registry) Needs(caps ...coord.Capability) {
	for _, c := range caps {
		r.app.needs = append(r.app.needs, need{cap: c, module: r.module})
	}
}

// Fatal stops the app because of err, from a module's own goroutine after
// start, such as a server whose listener failed: readiness fails from then on,
// and Run shuts the app down and returns err.
func (r *Registry) Fatal(err error) {
	r.app.die(fmt.Errorf("module %q: %w", r.module, err))
}

// Requires declares that other modules must be installed.
func (r *Registry) Requires(modules ...string) {
	for _, m := range modules {
		r.app.requires = append(r.app.requires, require{module: r.module, needs: m, site: callerSite(2)})
	}
}

// Gate registers a readiness gate. The app reports ready only while every gate passes.
func (r *Registry) Gate(name string, check func(ctx context.Context) error) {
	r.app.gates = append(r.app.gates, gate{name: r.module + "." + name, check: check})
}

// OnStart runs after every provider has started, in registration order.
func (r *Registry) OnStart(fn func(ctx context.Context) error) {
	r.app.startHooks = append(r.app.startHooks, hook{module: r.module, site: callerSite(2), fn: fn})
}

// OnReady runs once, the first time every readiness gate passes after Start,
// in registration order. Listeners should start accepting here rather than in
// OnStart: with shared ports, a socket gets connections as soon as it listens,
// so a worker of a new generation would otherwise take traffic before it is
// ready. A hook must return promptly: start serving in a goroutine (for
// example go srv.Serve(ln)). ctx stays live after the hook returns and is
// cancelled as soon as Shutdown begins, before the drain grace and OnDrain,
// so it must not become the context of requests that should finish while
// draining; stop servers in OnDrain instead. A hook still running when
// Shutdown begins should return.
func (r *Registry) OnReady(fn func(ctx context.Context) error) {
	r.app.readyHooks = append(r.app.readyHooks, hook{module: r.module, site: callerSite(2), fn: fn})
}

// OnStop runs during shutdown, in reverse registration order, before providers stop.
func (r *Registry) OnStop(fn func(ctx context.Context) error) {
	r.app.stopHooks = append(r.app.stopHooks, hook{module: r.module, site: callerSite(2), fn: fn})
}

// OnDrain runs after readiness is turned off and the grace period has passed,
// in reverse registration order. Listeners stop and long-lived connections are
// told to reconnect here.
func (r *Registry) OnDrain(fn func(ctx context.Context) error) {
	r.app.drainHooks = append(r.app.drainHooks, hook{module: r.module, site: callerSite(2), fn: fn})
}

// Provide registers a constructor for T. ctor must be a function whose
// parameters are other provided types and which returns T or (T, error).
// Providers are singletons per process, built at Start in dependency order.
//
// Reflection is used here and at boot only; nothing on a request path uses it.
func Provide[T any](r *Registry, ctor any) {
	typ := reflect.TypeFor[T]()
	cv := reflect.ValueOf(ctor)
	ct := cv.Type()
	switch {
	case !cv.IsValid() || ct.Kind() != reflect.Func:
		r.errorf("Provide[%s]: constructor must be a function, got %T", typ, ctor)
		return
	case ct.IsVariadic():
		r.errorf("Provide[%s]: constructor must not be variadic", typ)
		return
	case ct.NumOut() == 0 || ct.NumOut() > 2:
		r.errorf("Provide[%s]: constructor must return %s or (%s, error)", typ, typ, typ)
		return
	case ct.NumOut() == 2 && ct.Out(1) != errorType:
		r.errorf("Provide[%s]: second return value must be error", typ)
		return
	case !ct.Out(0).AssignableTo(typ):
		r.errorf("Provide[%s]: constructor returns %s, which is not assignable to %s", typ, ct.Out(0), typ)
		return
	}
	p := &provider{typ: typ, ctor: cv, module: r.module, site: callerSite(2), returnsErr: ct.NumOut() == 2}
	for i := 0; i < ct.NumIn(); i++ {
		p.params = append(p.params, ct.In(i))
	}
	r.app.addProvider(r, p)
}

// Supply registers an already-built value of type T.
func Supply[T any](r *Registry, value T) {
	typ := reflect.TypeFor[T]()
	v := reflect.ValueOf(&value).Elem()
	p := &provider{typ: typ, module: r.module, site: callerSite(2), value: v, built: true}
	r.app.addProvider(r, p)
}

// Collect adds values to the app's collection of T, which the kernel
// provides as []T: the values every module collected, in the order the
// modules are installed and, within one, of the calls. Any module may add
// to a collection; a module that consumes one, whether through a
// constructor or Resolve, calls Collect with no values, which declares it, so
// that []T is provided even when no module adds to it. Nothing else may
// provide []T. Consumers must not modify the slice.
//
// A value with a Validate() error method, on T or on *T, is checked as it
// is collected, and one that fails it fails boot, naming the module and the
// line, as a config section does. A nil pointer or func isn't checked, as
// its Validate could dereference or call it.
//
// Collect belongs in Register: once New has provided the collections, it
// panics rather than drop the values. A collection with values that no
// module declares and no constructor takes fails boot, for the same reason.
//
// An ORM adapter module, for one, registers its models with
// galleon.Collect(r, models...), and the data module builds its model
// registry from []data.ModelInfo.
func Collect[T any](r *Registry, values ...T) {
	typ := reflect.TypeFor[[]T]()
	if r.app.collected {
		panic(fmt.Sprintf("galleon: Collect[%s] (module %q, %s) called after boot: the collection is already provided; collect in Register", reflect.TypeFor[T](), r.module, callerSite(2)))
	}
	c, ok := r.app.collections[typ]
	if !ok {
		c = &collection{typ: typ, values: reflect.MakeSlice(typ, 0, len(values)), module: r.module, site: callerSite(2)}
		r.app.collections[typ] = c
		r.app.collectionOrder = append(r.app.collectionOrder, c)
	}
	if len(values) == 0 {
		c.declared = true
	}
	for _, v := range values {
		if val := validator(&v); val != nil {
			if err := val.Validate(); err != nil {
				r.errorf("Collect[%s]: %v", reflect.TypeFor[T](), err)
				continue
			}
		}
		if c.valueSite == "" {
			c.valueModule, c.valueSite = r.module, callerSite(2)
		}
		c.values = reflect.Append(c.values, reflect.ValueOf(&v).Elem())
	}
}

// validator returns *v as a Validator, through T's methods or *T's, or nil
// when it has neither, or is a nil pointer or func, whose Validate may
// dereference or call it, held in an interface or not.
func validator[T any](v *T) Validator {
	if rv := reflect.ValueOf(any(*v)); !rv.IsValid() || (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Func) && rv.IsNil() {
		return nil
	}
	if val, ok := any(*v).(Validator); ok {
		return val
	}
	val, _ := any(v).(Validator)
	return val
}

// collection is the values modules collected of one type, which the kernel
// provides as a slice.
type collection struct {
	typ          reflect.Type // []T
	values       reflect.Value
	module, site string // where it was first collected
	// valueModule and valueSite are where its first value was added.
	valueModule, valueSite string
	// declared is whether a module called Collect[T] with no values, as a
	// consumer does.
	declared bool
}

// provideCollections provides each collection, once every module has
// registered. Each slice is full to its capacity, so a consumer that
// appends to it gets a copy rather than writing into another's.
//
// A collection with values that no module declares (Collect[T] with no
// values) and no constructor takes fails boot, naming where its first value
// was added: its values would go nowhere, as models collected as
// *data.ModelInfo rather than data.ModelInfo would. A declared collection
// counts as taken, since it may be read through Resolve rather than a
// constructor.
func (a *App) provideCollections() {
	a.collected = true
	wanted := map[reflect.Type]bool{}
	for _, p := range a.providerOrder {
		for _, t := range p.params {
			wanted[t] = true
		}
	}
	for _, m := range a.migratorRegs {
		for _, t := range m.params {
			wanted[t] = true
		}
	}
	for _, c := range a.collectionOrder {
		if c.declared {
			wanted[c.typ] = true
		}
	}
	replaced := map[reflect.Type]bool{}
	for _, r := range a.settings.replacements {
		replaced[r.typ] = true
	}
	for _, c := range a.collectionOrder {
		if prev, ok := a.providers[c.typ]; ok {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("%s is provided by %s, but it is a collection (galleon.Collect, first by module %q, %s), which the kernel provides", c.typ, prev.describe(), c.module, c.site))
			continue
		}
		if c.valueSite != "" && !wanted[c.typ] && !replaced[c.typ] {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("module %q (%s): Collect[%s]: nothing declares or takes %s, so its values would go nowhere%s", c.valueModule, c.valueSite, c.typ.Elem(), c.typ, collectHint(c.typ.Elem(), wanted)))
		}
		n := c.values.Len()
		p := &provider{typ: c.typ, module: kernelModule, site: c.site, value: c.values.Slice3(0, n, n), built: true}
		a.providers[c.typ] = p
		a.providerOrder = append(a.providerOrder, p)
	}
}

// collectHint says what a collection of elem nothing takes was likely
// meant to be: a collection of the pointer or the value of elem, or of an
// interface elem implements, that something takes; or else that the module
// that takes it may not be installed.
func collectHint(elem reflect.Type, wanted map[reflect.Type]bool) string {
	var hints []string
	for t := range wanted {
		if t.Kind() != reflect.Slice {
			continue
		}
		switch e := t.Elem(); {
		case elem.Kind() == reflect.Pointer && elem.Elem() == e,
			e.Kind() == reflect.Pointer && e.Elem() == elem:
			hints = append(hints, fmt.Sprintf(" (collect %s values: %s is what is taken)", e, t))
		case e.Kind() == reflect.Interface && e != elem && elem.Implements(e):
			hints = append(hints, fmt.Sprintf(" (%s implements %s: collect it as galleon.Collect[%s], since %s is what is taken)", elem, e, e, t))
		}
	}
	if len(hints) == 0 {
		return fmt.Sprintf("; is the module that consumes it installed? A consumer declares it with galleon.Collect[%s](r)", elem)
	}
	slices.Sort(hints)
	return hints[0]
}

var errorType = reflect.TypeFor[error]()

func callerSite(skip int) string {
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%s:%d", file, line)
}
