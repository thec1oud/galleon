package galleon_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thec1oud/galleon"
)

type tag string

// tags consumes the collection of tags.
type tags struct{ all []tag }

func TestACollectionGathersEveryModulesValuesInInstallationOrder(t *testing.T) {
	consumer := galleon.ModuleFunc("consumer", func(r *galleon.Registry) error {
		galleon.Collect[tag](r)
		galleon.Provide[*tags](r, func(all []tag) *tags { return &tags{all: all} })
		return nil
	})
	first := galleon.ModuleFunc("first", func(r *galleon.Registry) error {
		galleon.Collect(r, tag("a"), tag("b"))
		galleon.Collect(r, tag("c"))
		return nil
	})
	second := galleon.ModuleFunc("second", func(r *galleon.Registry) error {
		galleon.Collect(r, tag("d"))
		return nil
	})
	app, err := galleon.New(base(consumer, first, second)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	if got := galleon.MustResolve[*tags](app).all; !slices.Equal(got, []tag{"a", "b", "c", "d"}) {
		t.Fatalf("got %v", got)
	}
	if got := galleon.MustResolve[[]tag](app); len(got) != 4 {
		t.Fatalf("resolved %v", got)
	}
}

func TestAnEmptyCollectionIsStillProvided(t *testing.T) {
	consumer := galleon.ModuleFunc("consumer", func(r *galleon.Registry) error {
		galleon.Collect[tag](r)
		galleon.Provide[*tags](r, func(all []tag) *tags { return &tags{all: all} })
		return nil
	})
	app, err := galleon.New(base(consumer)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	if got := galleon.MustResolve[*tags](app).all; len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestACollectionCantAlsoBeProvided(t *testing.T) {
	mod := galleon.ModuleFunc("orders", func(r *galleon.Registry) error {
		galleon.Collect(r, tag("a"))
		galleon.Provide[[]tag](r, func() []tag { return nil })
		return nil
	})
	_, err := galleon.New(base(mod)...)
	if err == nil || !strings.Contains(err.Error(), "[]galleon_test.tag is provided by") || !strings.Contains(err.Error(), `a collection (galleon.Collect, first by module "orders", `) {
		t.Fatalf("got %v", err)
	}
}

type checked string

func (c checked) Validate() error {
	if c == "" {
		return errors.New("empty")
	}
	return nil
}

func TestCollectedValuesAreValidatedAtBoot(t *testing.T) {
	mod := galleon.ModuleFunc("orders", func(r *galleon.Registry) error {
		galleon.Collect(r, checked("fine"), checked(""))
		return nil
	})
	_, err := galleon.New(base(mod)...)
	if err == nil || !strings.Contains(err.Error(), `module "orders" (`) || !strings.Contains(err.Error(), "collect_test.go:") ||
		!strings.Contains(err.Error(), "Collect[galleon_test.checked]: empty") {
		t.Fatalf("got %v", err)
	}
}

type checkedPtr struct{ ok bool }

func (c *checkedPtr) Validate() error {
	if !c.ok {
		return errors.New("not ok")
	}
	return nil
}

func TestANilCollectedValueIsntValidated(t *testing.T) {
	mod := galleon.ModuleFunc("orders", func(r *galleon.Registry) error {
		galleon.Collect(r, (*checkedPtr)(nil), &checkedPtr{ok: true})
		return nil
	})
	if _, err := galleon.New(base(mod)...); err != nil {
		t.Fatal(err)
	}
}

// pchecker is an interface collection whose values may be nil pointers.
type pchecker interface{ Validate() error }

func TestANilPointerInAnInterfaceCollectionIsntValidated(t *testing.T) {
	mod := galleon.ModuleFunc("orders", func(r *galleon.Registry) error {
		galleon.Collect[pchecker](r, (*checkedPtr)(nil), nil, &checkedPtr{ok: true})
		return nil
	})
	if _, err := galleon.New(base(mod)...); err != nil {
		t.Fatal(err)
	}
	bad := galleon.ModuleFunc("orders", func(r *galleon.Registry) error {
		galleon.Collect[pchecker](r, &checkedPtr{})
		return nil
	})
	if _, err := galleon.New(base(bad)...); err == nil || !strings.Contains(err.Error(), "not ok") {
		t.Fatalf("an invalid value in an interface collection: %v", err)
	}
}

func TestAValueWhoseValidateHasAPointerReceiverIsValidated(t *testing.T) {
	mod := galleon.ModuleFunc("orders", func(r *galleon.Registry) error {
		galleon.Collect(r, checkedPtr{ok: true}, checkedPtr{})
		return nil
	})
	_, err := galleon.New(base(mod)...)
	if err == nil || !strings.Contains(err.Error(), "Collect[galleon_test.checkedPtr]: not ok") {
		t.Fatalf("got %v", err)
	}
}

func TestCollectAfterBootPanicsRatherThanDropTheValues(t *testing.T) {
	var reg *galleon.Registry
	mod := galleon.ModuleFunc("orders", func(r *galleon.Registry) error {
		reg = r
		galleon.Collect[tag](r)
		return nil
	})
	if _, err := galleon.New(base(mod)...); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if p := recover(); p == nil || !strings.Contains(fmt.Sprint(p), "Collect[galleon_test.tag] (module \"orders\", ") || !strings.Contains(fmt.Sprint(p), "called after boot") {
			t.Fatalf("got %v", p)
		}
	}()
	galleon.Collect(reg, tag("late"))
}

func TestAConsumerAppendingToACollectionDoesntChangeAnothers(t *testing.T) {
	type second struct{ all []tag }
	mod := galleon.ModuleFunc("orders", func(r *galleon.Registry) error {
		galleon.Collect(r, tag("a"), tag("b"))
		galleon.Collect(r, tag("c")) // grows the slice past its length
		galleon.Provide[*tags](r, func(all []tag) *tags { return &tags{all: append(all, "mine")} })
		galleon.Provide[*second](r, func(all []tag, _ *tags) *second { return &second{all: append(all, "theirs")} })
		return nil
	})
	app, err := galleon.New(base(mod)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	if got := galleon.MustResolve[*tags](app).all; !slices.Equal(got, []tag{"a", "b", "c", "mine"}) {
		t.Fatalf("the first consumer has %v", got)
	}
	if got := galleon.MustResolve[[]tag](app); !slices.Equal(got, []tag{"a", "b", "c"}) {
		t.Fatalf("the collection is %v", got)
	}
}

func TestACollectionCantBeReplaced(t *testing.T) {
	mod := galleon.ModuleFunc("orders", func(r *galleon.Registry) error {
		galleon.Collect(r, tag("a"))
		return nil
	})
	_, err := galleon.New(append(base(mod), galleon.Replace([]tag{"b"}))...)
	if err == nil || !strings.Contains(err.Error(), "[]galleon_test.tag is a collection") {
		t.Fatalf("got %v", err)
	}
}
