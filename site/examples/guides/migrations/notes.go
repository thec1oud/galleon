// Command notes is the sample behind the "Deploy migrations safely" guide:
// a service with a migrator, whose binary plans and applies its own
// migrations.
package main

import (
	"context"
	"embed"
	"io/fs"
	"os"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/cli"
	"github.com/thec1oud/galleon/coord/local"
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

// docs:start main
// options is the service, as every command runs it, over the migrations
// in files; more adds to it, such as its config.
func options(files fs.FS, more ...galleon.Option) []galleon.Option {
	return append([]galleon.Option{
		galleon.Name("notes"),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
		galleon.Modules(pg.Module(), Notes{files}),
	}, more...)
}

// main serves without a command, and runs `notes migrate status|plan|apply`
// with one.
func main() {
	os.Exit(cli.Main(context.Background(), os.Args[1:], options(Migrations, galleon.ConfigFile("config.json"))...))
}

// docs:end main
