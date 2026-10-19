package coordtest

import (
	"strings"
	"testing"

	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/coord/local"
)

func TestCheckCapabilitiesCatchesDishonestScope(t *testing.T) {
	cases := []struct {
		caps    coord.Capabilities
		peer    bool
		wantErr string
	}{
		{coord.Capabilities{Name: "x", Scope: coord.ScopeProcess}, false, ""},
		{coord.Capabilities{Name: "x", Scope: coord.ScopeProcess}, true, "cannot have peers"},
		{coord.Capabilities{Name: "x", Scope: coord.ScopeCluster}, false, "must give the suite a Peer"},
		{coord.Capabilities{Name: "x", Scope: coord.ScopeCluster, Provides: []coord.Capability{coord.MembershipCap}}, true, "failure detection bound"},
		{coord.Capabilities{Scope: coord.ScopeProcess}, false, "Name is empty"},
	}
	for _, c := range cases {
		err := checkCapabilities(c.caps, c.peer)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%+v peer=%v: unexpected %v", c.caps, c.peer, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%+v peer=%v: got %v, want %q", c.caps, c.peer, err, c.wantErr)
		}
	}
}

// partial declares only locks, so the suite must skip everything else.
type partial struct{ *local.Backend }

func (p partial) Capabilities() coord.Capabilities {
	c := p.Backend.Capabilities()
	c.Provides = []coord.Capability{coord.Locks}
	return c
}

func TestUndeclaredCapabilitiesAreSkipped(t *testing.T) {
	var skipped []string
	for _, name := range []string{"Membership", "LeaderElection", "PubSub", "KV", "Counters"} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if t.Skipped() {
					skipped = append(skipped, name)
				}
			}()
			need(t, Factory{New: func(*testing.T) coord.Backend { return partial{local.New()} }}, map[string]coord.Capability{
				"Membership": coord.MembershipCap, "LeaderElection": coord.LeaderElection,
				"PubSub": coord.PubSubCap, "KV": coord.KVCap, "Counters": coord.CountersCap,
			}[name])
		})
	}
	if len(skipped) != 5 {
		t.Fatalf("skipped %v, want all five undeclared capabilities", skipped)
	}
	if selfID(partial{local.New()}) != "" {
		t.Fatal("selfID must not call Membership on a backend that doesn't declare it")
	}
}
