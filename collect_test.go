package galleon_test

import (
	"context"
	"errors"
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
