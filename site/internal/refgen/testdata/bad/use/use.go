// Package use returns codes the scanner can't resolve.
package use

import (
	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/errs"
)

var code = "use.var"

func variable() error { return errs.New(errs.NotFound, code, "") }

func notCatalog() error { return errs.New(errs.NotFound, "Use.Bad", "") }

func kindVar(k errs.Kind) error { return errs.New(k, "use.kind_var", "") }

func helper(c string) error { return errs.New(errs.NotFound, c, "") }

func callsHelper(s string) error { return helper(s + "x") }

type S struct{ A int }

func load(r *galleon.Registry, name string) { galleon.Config[S](r, name) }

func localKind(c string) error {
	k := errs.NotFound
	return errs.New(k, c, "")
}

func codeOnly() error { return &errs.Error{Code: "use.code_only"} }

func asValue() func(string) error { return helper }

type Inner struct {
	Key string `json:"key" secret:"true"`
}

type T struct {
	Inner Inner `json:"inner"`
}

func loadTwice(r *galleon.Registry) {
	galleon.Config[T](r, "twice")
	galleon.Config[S](r, "twice")
}

type failer interface{ fail(string) error }

type thing struct{}

func (thing) fail(c string) error { return errs.New(errs.NotFound, c, "") }

func viaInterface() error {
	var f failer = thing{}
	return f.fail("use.via_interface")
}
