// Package notes is the sample behind the "Deploy migrations safely" guide:
// a module with a migrator, and a plan report for CI.
package notes

import (
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/migrate"
	"github.com/thec1oud/galleon/pg"
	"github.com/thec1oud/galleon/pg/sqlmigrate"
)

//go:embed migrations/*.sql
var files embed.FS

// Migrations is the service's schema, one numbered SQL file per change.
var Migrations, _ = fs.Sub(files, "migrations")

// docs:start module
// Notes registers the "notes" migrator over Files.
type Notes struct{ Files fs.FS }

func (Notes) Name() string { return "notes" }

func (n Notes) Register(r *galleon.Registry) error {
	galleon.Migrator(r, "notes", func(m *pg.Manager) (migrate.Migrator, error) {
		return sqlmigrate.New("notes", n.Files, m.Pool(""))
	})
	return nil
}

// docs:end module

// docs:start plan
// PrintPlan writes what applying would do to the database app is connected
// to: each migrator's pending steps, their risks, and the fingerprint to
// approve. It fails when a plan couldn't be made, or the policy would
// refuse it, so CI stops there.
func PrintPlan(ctx context.Context, app *galleon.App, w io.Writer) error {
	reports, err := app.Migrations().Plan(ctx, false)
	if err != nil {
		return err
	}
	if err := migrate.WritePlans(w, reports); err != nil {
		return err
	}
	for _, r := range reports {
		if r.Err != nil || r.Refused != nil {
			return fmt.Errorf("migrator %s can't be deployed as planned", r.Name)
		}
	}
	return nil
}

// docs:end plan
