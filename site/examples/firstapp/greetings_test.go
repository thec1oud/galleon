package main

import (
	"testing"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/testkit"
)

func TestGreeterUsesTheDefaultGreeting(t *testing.T) {
	h := testkit.New(t, galleon.Modules(Greetings{}))
	if got := testkit.Get[*Greeter](h).Greet("Ada"); got != "Hello, Ada!" {
		t.Errorf("Greet = %q", got)
	}
}

func TestGreeterReadsItsConfig(t *testing.T) {
	h := testkit.New(t, galleon.Modules(Greetings{}), galleon.ConfigJSON([]byte(`{"greeter": {"greeting": "Ahoy"}}`)))
	if got := testkit.Get[*Greeter](h).Greet("Ada"); got != "Ahoy, Ada!" {
		t.Errorf("Greet = %q", got)
	}
}

func TestTheAppBoots(t *testing.T) {
	h := testkit.New(t, options()...)
	if got := testkit.Get[*Greeter](h).Greet("Ada"); got != "Hello, Ada!" {
		t.Errorf("Greet = %q", got)
	}
}
