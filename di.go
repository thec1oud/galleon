package galleon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/coord"
)

type provider struct {
	typ        reflect.Type
	ctor       reflect.Value
	params     []reflect.Type
	returnsErr bool
	module     string
	site       string

	value reflect.Value
	built bool
}

func (p *provider) describe() string {
	return fmt.Sprintf("%s (module %q, %s)", p.typ, p.module, p.site)
}

func (a *App) addProvider(r *Registry, p *provider) {
	if prev, ok := a.providers[p.typ]; ok {
		r.errorf("%s is already provided by %s", p.typ, prev.describe())
		return
	}
	a.providers[p.typ] = p
	a.providerOrder = append(a.providerOrder, p)
}

// resolveGraph checks that every dependency is provided and that there are no
// cycles, and returns providers in an order where dependencies come first.
func (a *App) resolveGraph() ([]*provider, error) {
	var errs []error
	for _, p := range a.providerOrder {
		for _, dep := range p.params {
			if _, ok := a.providers[dep]; !ok {
				errs = append(errs, fmt.Errorf("%s needs %s, but nothing provides it%s", p.describe(), dep, suggest(dep, a.providers)))
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	const (
		unvisited = iota
		visiting
		done
	)
	state := map[*provider]int{}
	var order []*provider
	var stack []*provider
	var visit func(p *provider) error
	visit = func(p *provider) error {
		switch state[p] {
		case done:
			return nil
		case visiting:
			var path []string
			start := 0
			for i, s := range stack {
				if s == p {
					start = i
				}
			}
			for _, s := range stack[start:] {
				path = append(path, s.typ.String())
			}
			path = append(path, p.typ.String())
			return fmt.Errorf("dependency cycle: %s (first registered at %s)", strings.Join(path, " -> "), stack[start].site)
		}
		state[p] = visiting
		stack = append(stack, p)
		for _, dep := range p.params {
			if err := visit(a.providers[dep]); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[p] = done
		order = append(order, p)
		return nil
	}
	for _, p := range a.providerOrder {
		if err := visit(p); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// suggest names a provided type that would satisfy dep with a pointer/value or
// interface adjustment, the most common wiring mistake.
func suggest(dep reflect.Type, providers map[reflect.Type]*provider) string {
	for t := range providers {
		switch {
		case dep.Kind() == reflect.Pointer && dep.Elem() == t,
			t.Kind() == reflect.Pointer && t.Elem() == dep:
			return fmt.Sprintf(" (did you mean %s?)", t)
		case dep.Kind() == reflect.Interface && t.Implements(dep):
			return fmt.Sprintf(" (%s implements it: provide it as %s)", t, dep)
		}
	}
	return ""
}

func (a *App) build(p *provider) error {
	if p.built {
		return nil
	}
	args := make([]reflect.Value, len(p.params))
	for i, dep := range p.params {
		args[i] = a.providers[dep].value
	}
	out := p.ctor.Call(args)
	if p.returnsErr && !out[1].IsNil() {
		return fmt.Errorf("constructing %s: %w", p.describe(), out[1].Interface().(error))
	}
	v := reflect.New(p.typ).Elem()
	v.Set(out[0])
	p.value = v
	p.built = true
	return nil
}

// Resolve returns the instance of T. It only works after Start.
func Resolve[T any](a *App) (T, error) {
	var zero T
	a.mu.Lock()
	started := a.state >= stateStarted
	a.mu.Unlock()
	if !started {
		return zero, errors.New("galleon: Resolve called before Start")
	}
	p, ok := a.providers[reflect.TypeFor[T]()]
	if !ok {
		return zero, fmt.Errorf("galleon: nothing provides %s", reflect.TypeFor[T]())
	}
	return p.value.Interface().(T), nil
}

// MustResolve is Resolve for tests and main functions.
func MustResolve[T any](a *App) T {
	v, err := Resolve[T](a)
	if err != nil {
		panic(err)
	}
	return v
}

// Starter and Stopper are implemented by provided values that hold resources.
type Starter interface {
	Start(ctx context.Context) error
}
type Stopper interface {
	Stop(ctx context.Context) error
}

type reflectType = reflect.Type

type replacement struct {
	typ   reflect.Type
	value reflect.Value
	site  string
}

// Replace swaps whatever provides T for value, which is used as is: it is not
// started or stopped. It exists for tests, where testkit uses it to put fakes
// in place of real seams. Boot fails if nothing provides T, so a replacement
// that has gone stale can't silently do nothing.
func Replace[T any](value T) Option {
	r := replacement{typ: reflect.TypeFor[T](), value: reflect.ValueOf(&value).Elem(), site: callerSite(2)}
	return func(s *settings) { s.replacements = append(s.replacements, r) }
}

// kernelOptions names the option to set instead of Replace for the values the
// kernel provides, where there is one. Every kernel value is refused by
// Replace, since the kernel also uses them directly.
var kernelOptions = map[reflect.Type]string{
	reflect.TypeFor[*slog.Logger]():         "galleon.Logger",
	reflect.TypeFor[clock.Clock]():          "galleon.Clock",
	reflect.TypeFor[coord.Backend]():        "galleon.Coordination",
	reflect.TypeFor[trace.TracerProvider](): "galleon.TracerProvider",
	reflect.TypeFor[metric.MeterProvider](): "galleon.MeterProvider",
}

func (a *App) applyReplacements() {
	replaced := map[reflect.Type]string{}
	for _, r := range a.settings.replacements {
		if isNil(r.value) {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("Replace[%s] (%s): the replacement is nil", r.typ, r.site))
			continue
		}
		if prev, ok := replaced[r.typ]; ok {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("Replace[%s] (%s): %s is already replaced at %s", r.typ, r.site, r.typ, prev))
			continue
		}
		replaced[r.typ] = r.site
		p, ok := a.providers[r.typ]
		if !ok {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("Replace[%s] (%s): nothing provides %s%s", r.typ, r.site, r.typ, suggest(r.typ, a.providers)))
			continue
		}
		if p.module == kernelModule {
			hint := ""
			if opt, ok := kernelOptions[r.typ]; ok {
				hint = "; set it with " + opt + " instead"
			}
			a.bootErrs = append(a.bootErrs, fmt.Errorf("Replace[%s] (%s): the kernel provides %s and uses it itself%s", r.typ, r.site, r.typ, hint))
			continue
		}
		p.ctor, p.params = reflect.Value{}, nil
		p.value, p.built = r.value, true
		p.module, p.site = "replaced", r.site
	}
}

func isNil(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	}
	return false
}
