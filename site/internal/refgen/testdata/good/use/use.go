// Package use returns codes in every way the scanner follows.
package use

import (
	"fmt"

	g "github.com/thec1oud/galleon"
	e "github.com/thec1oud/galleon/errs"
)

// CodeDoc is documented.
const CodeDoc = "use.doc" // SQLSTATE 40001

const prefix = "use."

func aliased() error { return e.New(e.NotFound, "use.aliased", "no such thing") }

func constant() error { return e.New(e.Conflict, CodeDoc, "the doc") }

func folded() error { return e.New(e.Conflict, prefix+"folded", "folded "+fmt.Sprint(1)+" times") }

func formatted(n int) error {
	return e.New(e.InvalidArgument, "use.formatted", fmt.Sprintf("%d is %q", n, "x"))
}

func literal() error { return &e.Error{Kind: e.PermissionDenied, Code: "use.literal", Message: "no"} }

func returned() (e.Kind, string, string) { return e.Unauthenticated, "use.returned", "who" }

func helper(code, msg string) error { return e.New(e.NotFound, code, msg) }

type thing struct{}

func (thing) refuse(code string) error { return e.New(e.Conflict, code, "") }

func callers() []error {
	closure := func(code string) error { return e.New(e.InvalidArgument, code, "closure") }
	return []error{helper("use.helper", "by helper"), thing{}.refuse("use.method"), closure("use.closure")}
}

func outer(code string) error { return helper(code, "by outer") }

func fail(k e.Kind, code string) error { return e.New(k, code, "by fail") }

func forwarders() []error {
	return []error{outer("use.two_level"), fail(e.Conflict, "use.wrapper")}
}

func twoSites(b bool, code string) error {
	if b {
		return e.New(e.NotFound, code, "first")
	}
	return e.New(e.Conflict, code, "second")
}

type entry struct {
	state string
	kind  e.Kind
	code  string
}

var table = []entry{{state: "1", kind: e.Unauthenticated, code: "use.keyed"}}

type guard[T any] struct{}

func (guard[T]) deny(code string) error { return e.New(e.PermissionDenied, code, "denied") }

func more() []error {
	return []error{twoSites(true, "use.two_sites"), guard[int]{}.deny("use.generic")}
}

func zero() (e.Kind, string, bool) { return 0, "", false }

type Embedded struct {
	// Deep is in an embedded struct.
	Deep int `json:"deep"`
}

type Item struct {
	// Name names it.
	Name string `json:"name"`
	// Next is recursive through a slice, which ends.
	Next []Other `json:"next"`
}

type Other struct {
	// Value is a value.
	Value string `json:"value"`
}

// Section is a config section.
type Section struct {
	Embedded
	// Top is a top-level scalar.
	Top string `json:"top" secret:"true"`
	// Items are items.
	Items map[string]Item `json:"items"`
	// A and B share a doc.
	A      int `json:"a"`
	B      int `json:"b"`
	C      int `json:"c"`
	hidden int
	Skip   int `json:"-"`
}

func load(r *g.Registry) { g.Config[Section](r, "use") }

func assigned() error {
	var f func(string) error
	f = func(code string) error { return e.New(e.NotFound, code, "assigned") }
	return f("use.assigned")
}
