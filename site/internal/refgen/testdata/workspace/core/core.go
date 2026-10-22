package galleon

import "github.com/thec1oud/galleon/errs"

func fail() error { return errs.New(errs.NotFound, "core.missing", "missing") }
