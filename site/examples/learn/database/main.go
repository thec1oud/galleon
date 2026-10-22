// Command database is the bookmarks service as the third chapter of Learn
// leaves it: bookmarks live in PostgreSQL.
package main

import (
	"context"
	"log"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/pg"
	"github.com/thec1oud/galleon/server"
)

// docs:start main
func options() []galleon.Option {
	return []galleon.Option{
		galleon.Name("bookmarks"),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
		galleon.ConfigFile("config.json"),
		galleon.Modules(server.Module(), pg.Module(), Bookmarks{}),
	}
}

// docs:end main

func main() {
	app, err := galleon.New(options()...)
	if err != nil {
		log.Fatal(err)
	}
	if err := app.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
