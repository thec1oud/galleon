package main

import (
	"fmt"

	"github.com/thec1oud/galleon"
)

// docs:start module
// Greetings is a module: it registers everything it contributes in one place.
type Greetings struct{}

func (Greetings) Name() string { return "greetings" }

func (Greetings) Register(r *galleon.Registry) error {
	galleon.Config[GreeterConfig](r, "greeter") // typed config, also injectable as *GreeterConfig
	galleon.Provide[*Greeter](r, NewGreeter)    // built at boot from its parameters
	return nil
}

// GreeterConfig is the "greeter" section of the config.
type GreeterConfig struct {
	Greeting string `json:"greeting"`
}

// Default fills in what the config file leaves out.
func (c *GreeterConfig) Default() { c.Greeting = "Hello" }

// Greeter is a service other modules can depend on.
type Greeter struct{ greeting string }

func NewGreeter(cfg *GreeterConfig) *Greeter { return &Greeter{greeting: cfg.Greeting} }

func (g *Greeter) Greet(name string) string { return fmt.Sprintf("%s, %s!", g.greeting, name) }

// docs:end module
