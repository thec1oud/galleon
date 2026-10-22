// Package extra is a module beside the core, with codes and a section of
// its own, and a code of the core's that it returns too.
package extra

import (
	g "github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/errs"
)

// CodeBusy: the extra module is busy.
const CodeBusy = "extra.busy"

func busy() error { return errs.New(errs.Conflict, CodeBusy, "busy") }

func alsoCore() error { return errs.New(errs.NotFound, "core.missing", "missing here too") }

type Settings struct {
	// Level is a level.
	Level int `json:"level"`
}

func load(r *g.Registry) *Settings { return g.Config[Settings](r, "extra") }
