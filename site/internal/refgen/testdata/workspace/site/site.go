// Package site stands for the docs site, which the reference leaves out.
package site

import "github.com/thec1oud/galleon/errs"

func f() error { return errs.New(errs.NotFound, "site.left_out", "left out") }
