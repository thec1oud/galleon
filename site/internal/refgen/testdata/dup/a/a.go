package a

import "github.com/thec1oud/galleon/errs"

// CodeTwice: described here.
const CodeTwice = "dup.twice"

func f() error { return errs.New(errs.NotFound, CodeTwice, "") }
