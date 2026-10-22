package digest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/testkit"
)

// Two nodes of one cluster, as two apps on one database, take turns with
// the lock.
func TestTwoNodesShareTheLock(t *testing.T) {
	dsn := testkit.PackageDSN(t)
	schema := fmt.Sprintf("digest_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), dsn)
		if err == nil {
			_, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = conn.Close(context.Background())
		}
	})
	// docs:start nodes
	config := fmt.Appendf(nil, `{"coord_pg": {"dsn": %q, "schema": %q}}`, dsn, schema)
	start := func() *galleon.App {
		app, err := galleon.New(clusterOptions(galleon.ConfigJSON(config), galleon.DrainGrace(0))...)
		if err != nil {
			t.Fatal(err)
		}
		if err := app.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
		return app
	}
	a, b := start(), start()
	ctx := context.Background()

	// Node b holds the lock: node a doesn't send.
	lease, _, err := b.Coordination().Locks().TryAcquire(ctx, "digest.daily", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if sent, err := galleon.MustResolve[*Digest](a).Send(ctx); sent || err != nil {
		t.Fatalf("Send on node a while node b held the lock = %v, %v", sent, err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if sent, err := galleon.MustResolve[*Digest](a).Send(ctx); !sent || err != nil {
		t.Fatalf("Send on node a = %v, %v", sent, err)
	}
	// docs:end nodes
}
