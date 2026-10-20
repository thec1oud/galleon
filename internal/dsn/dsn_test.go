package dsn_test

import (
	"testing"

	"github.com/thec1oud/galleon/internal/dsn"
)

func TestIsURL(t *testing.T) {
	for s, want := range map[string]bool{
		"postgres://app@db/orders":                       true,
		"postgress://app@db":                             true,
		"  postgres://app@db\n":                          true,
		"host=db user=app":                               false,
		"application_name=http://x host=db user=app":     false,
		"host=db application_name=postgres://x user=app": false,
	} {
		if got := dsn.IsURL(s); got != want {
			t.Errorf("IsURL(%q) = %v", s, got)
		}
	}
}
