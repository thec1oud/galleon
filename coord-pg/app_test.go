package coordpg_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord"
	coordpg "github.com/thec1oud/galleon/coord-pg"
	"github.com/thec1oud/galleon/testkit"
)

// In an app, the backend reads the coord_pg section, starts before the
// providers that use it, and stops after them.
func TestTheBackendRunsInAnApp(t *testing.T) {
	schema := store(t)
	dsn := testkit.PackageDSN(t)
	var held coord.Lease
	mod := galleon.ModuleFunc("jobs", func(r *galleon.Registry) error {
		r.Needs(coord.Locks)
		galleon.Provide[*worker](r, func(b coord.Backend) *worker { return &worker{b: b, held: &held} })
		return nil
	})
	app, err := galleon.New(
		galleon.Logger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		galleon.Coordination(coordpg.New()),
		galleon.Deploy(galleon.MultiNode),
		galleon.DrainGrace(0),
		galleon.ConfigJSON(fmt.Appendf(nil, `{"coord_pg": {"dsn": %q, "schema": %q}}`, dsn, schema)),
		galleon.Modules(mod),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held.Lost():
	default:
		t.Fatal("the worker's lease outlived the app")
	}
	// A second app on the same store finds the lease free.
	other := node(t, schema)
	if _, ok, err := other.Locks().TryAcquire(ctx(t), "worker", time.Minute); err != nil || !ok {
		t.Fatalf("acquire after the app stopped: %v %v", ok, err)
	}
}

type worker struct {
	b    coord.Backend
	held *coord.Lease
}

// Start takes a lease through the backend, which has started already.
func (w *worker) Start(ctx context.Context) error {
	l, ok, err := w.b.Locks().TryAcquire(ctx, "worker", time.Minute)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("worker lease is held")
	}
	*w.held = l
	return nil
}

func TestBootNeedsADatabase(t *testing.T) {
	_, err := galleon.New(galleon.Coordination(coordpg.New()))
	if err == nil || !strings.Contains(err.Error(), "set coord_pg.dsn") {
		t.Fatalf("New without a database = %v", err)
	}
	_, err = galleon.New(galleon.Coordination(coordpg.New(coordpg.WithConfig(coordpg.Config{DSN: "postgres://x"}))))
	if err == nil || !strings.Contains(err.Error(), "use the coord_pg config section") {
		t.Fatalf("New with WithConfig = %v", err)
	}
	_, err = galleon.New(galleon.Coordination(coordpg.New()),
		galleon.ConfigJSON([]byte(`{"coord_pg": {"dsn": "postgres://x", "heartbeat": 1000000000, "failure_detection": 1000000000}}`)))
	if err == nil || !strings.Contains(err.Error(), "must be at least twice heartbeat") {
		t.Fatalf("New with a short failure_detection = %v", err)
	}
}
