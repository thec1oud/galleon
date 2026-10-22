// Command firstapp is the smallest Galleon app: one module, one config
// section, one service. The docs' introduction shows it.
package main

import (
	"context"
	"log"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
)

// docs:start app
func options() []galleon.Option {
	return []galleon.Option{
		galleon.Name("hello"),
		galleon.Coordination(local.New()), // required: there is no default backend
		galleon.Deploy(galleon.SingleNode),
		galleon.Modules(Greetings{}),
	}
}

func main() {
	app, err := galleon.New(options()...)
	if err != nil {
		log.Fatal(err) // every wiring, config and coordination problem, listed together
	}
	if err := app.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

// docs:end app
