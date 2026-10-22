// Command reference regenerates the docs' reference pages for error codes
// and config from Galleon's source. Run it with `go generate ./...` in site/.
package main

import (
	"log"

	"github.com/thec1oud/galleon/site/internal/refgen"
)

func main() {
	if err := refgen.Write("..", "."); err != nil {
		log.Fatal(err)
	}
}
