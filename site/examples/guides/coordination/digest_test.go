package digest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/testkit"
)

// docs:start single
func TestOneNodeSendsAtATime(t *testing.T) {
	h := testkit.New(t, galleon.Modules(Module())) // local.New() as a single node
	d := testkit.Get[*Digest](h)
	ctx := context.Background()

	// Another node holds the lock: this one doesn't send.
	lease, _, err := h.Coordination().Locks().TryAcquire(ctx, "digest.daily", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if sent, err := d.Send(ctx); sent || err != nil {
		t.Fatalf("Send while the lock was held = %v, %v", sent, err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if sent, err := d.Send(ctx); !sent || err != nil {
		t.Fatalf("Send = %v, %v", sent, err)
	}
	if !h.Logs.Contains("digest sent") {
		t.Fatal("nothing logged")
	}
}

// docs:end single

// docs:start multi
func TestAnInProcessBackendRefusesSeveralNodes(t *testing.T) {
	_, err := galleon.New(
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.MultiNode),
		galleon.Modules(Module()),
	)
	if err == nil || !strings.Contains(err.Error(), "only guarantees within one process") {
		t.Fatalf("New = %v", err)
	}
}

// docs:end multi
