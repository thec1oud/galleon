// Command migrations is the bookmarks service as the fourth chapter of
// Learn leaves it: the service migrates its own schema.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/pg"
	"github.com/thec1oud/galleon/server"
)

// docs:start main
func options(env galleon.Env) []galleon.Option {
	return []galleon.Option{
		galleon.Name("bookmarks"),
		galleon.Environment(env),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
		galleon.ConfigFile("config.json"),
		galleon.Modules(server.Module(), pg.Module(), Bookmarks{}),
	}
}

func main() {
	dev := flag.Bool("dev", false, "apply migrations at boot, as in development")
	flag.Parse()
	env := galleon.Production
	if *dev {
		env = galleon.Development
	}
	app, err := galleon.New(options(env)...)
	if err != nil {
		log.Fatal(err)
	}
	if err := app.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

// docs:end main
