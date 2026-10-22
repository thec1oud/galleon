package testkit_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thec1oud/galleon/testkit"
)

func TestReplicaSkipsWithoutAReplica(t *testing.T) {
	db := testkit.DB(t, notes)
	t.Setenv(testkit.ReplicaEnv, "")
	t.Setenv("CI", "")
	ran := false
	t.Run("inner", func(t *testing.T) {
		db.Replica(t)
		ran = true
	})
	if ran {
		t.Fatal("Replica returned without a replica configured")
	}
}

func TestReplicaLagsWhilePausedAndCatchesUpOnceResumed(t *testing.T) {
	db := testkit.DB(t, notes)
	rep := db.Replica(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	primary, err := pgx.Connect(ctx, db.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close(ctx)
	replica, err := pgx.Connect(ctx, rep.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close(ctx)
	count := func() int {
		var n int
		if err := replica.QueryRow(ctx, "SELECT count(*) FROM notes WHERE id = 424242").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	rep.Pause()
	if _, err := primary.Exec(ctx, "INSERT INTO notes VALUES (424242, 'replicated')"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = primary.Exec(context.Background(), "DELETE FROM notes WHERE id = 424242") })
	if count() != 0 {
		t.Fatal("a paused replica replayed a write")
	}
	rep.Resume()
	rep.CatchUp()
	if count() != 1 {
		t.Fatal("a caught-up replica hasn't the write")
	}
}

func TestProxyForwardsRecordsAndCuts(t *testing.T) {
	db := testkit.DB(t, notes)
	p := testkit.NewProxy(t, db.DSN())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, p.DSN(db.DSN()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	// pgx may try more than once, as it does when TLS is preferred.
	if n := p.Opened(); n < 1 {
		t.Fatalf("the proxy counts %d connections opened", n)
	}
	opened := p.Opened()
	p.Reset()
	if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if m := p.Messages(); len(m) != 1 || m[0].Type != 'Q' || m[0].SQL != "SELECT 1" || m[0].Conn != 1 {
		t.Fatalf("recorded %+v", m)
	}
	p.Cut()
	if _, err := conn.Exec(ctx, "SELECT 1"); err == nil {
		t.Fatal("a cut connection still works")
	}
	if _, err := pgx.Connect(ctx, p.DSN(db.DSN())); err == nil {
		t.Fatal("a cut proxy let a connection through")
	}
	// pgx tries again on its own, so a refused connect may count more than once.
	refused := p.Opened()
	if refused <= opened {
		t.Fatalf("the proxy counts %d connections opened, %d before the refused one", refused, opened)
	}
	p.Restore()
	again, err := pgx.Connect(ctx, p.DSN(db.DSN()))
	if err != nil {
		t.Fatalf("after Restore: %v", err)
	}
	again.Close(ctx)
	if n := p.Opened(); n <= refused {
		t.Fatalf("the proxy counts %d connections opened, %d before the last", n, refused)
	}
}

func TestProxyDSNsPointAtTheProxyInEitherForm(t *testing.T) {
	db := testkit.DB(t, notes)
	p := testkit.NewProxy(t, db.DSN())
	host, port, _ := net.SplitHostPort(p.Addr())
	if got := p.DSN("postgres://u:pw@db.example:5432/app?sslmode=disable&host=x"); got != "postgres://u:pw@"+host+":"+port+"/app?sslmode=disable" {
		t.Errorf("URL: %s", got)
	}
	if got := p.DSN("host=db port=5432 dbname=app"); !strings.HasSuffix(got, " host="+host+" port="+port) {
		t.Errorf("keyword/value: %s", got)
	}
}
