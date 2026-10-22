package digest

import (
	"github.com/thec1oud/galleon"
	coordpg "github.com/thec1oud/galleon/coord-pg"
)

// docs:start cluster
// clusterOptions runs the digest on every node of a cluster. The backend
// on PostgreSQL makes its lock hold across all of them.
func clusterOptions(more ...galleon.Option) []galleon.Option {
	return append([]galleon.Option{
		galleon.Name("digest"),
		galleon.Coordination(coordpg.New()), // reads the coord_pg config section
		galleon.Deploy(galleon.MultiNode),
		galleon.Modules(Module()),
	}, more...)
}

// docs:end cluster
