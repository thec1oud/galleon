// Command service is the bookmarks service as the first chapter of Learn
// leaves it: two operations over HTTP, with bookmarks kept in memory.
package main

import (
	"context"
	"log"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/server"
)

// docs:start main
func options() []galleon.Option {
	return []galleon.Option{
		galleon.Name("bookmarks"),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
		galleon.Modules(server.Module(), Bookmarks{}),
	}
}

func main() {
	app, err := galleon.New(options()...)
	if err != nil {
		log.Fatal(err)
	}
	if err := app.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

// docs:end main
