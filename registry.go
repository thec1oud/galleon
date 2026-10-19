package galleon

import (
	"context"
	"fmt"
	"reflect"
	"runtime"

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

var errorType = reflect.TypeFor[error]()

func callerSite(skip int) string {
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%s:%d", file, line)
}
