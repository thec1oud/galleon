package galleon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"

	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/process"
)

// GenerationEnv names the environment variable a supervisor sets on the
// processes it starts, so each generation of workers is distinguishable during
// a zero-downtime upgrade.
const GenerationEnv = process.EnvGeneration

// Node identifies this process in coordination, telemetry and leases. It is
// provided to dependency injection as Node.
type Node struct {
	Host       string
	PID        int
	Generation uint64
	// Instance is random per process, so a restarted process with a reused PID
	// is never mistaken for its predecessor.
	Instance string
}

// String returns host/pid/generation/instance.
func (n Node) String() string {
	return fmt.Sprintf("%s/%d/%d/%s", n.Host, n.PID, n.Generation, n.Instance)
}

// CoordID is the node ID to give a coordination backend.
func (n Node) CoordID() coord.NodeID { return coord.NodeID(n.String()) }

func currentNode() (Node, error) {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	var gen uint64
	s := process.Generation()
	if v, ok := os.LookupEnv(GenerationEnv); ok {
		s = v // set directly, without a supervisor
	}
	if s != "" {
		if gen, err = strconv.ParseUint(s, 10, 64); err != nil {
			return Node{Host: host, PID: os.Getpid()}, fmt.Errorf("%s=%q is not a generation number", GenerationEnv, s)
		}
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return Node{Host: host, PID: os.Getpid(), Generation: gen, Instance: hex.EncodeToString(b[:])}, nil
}
