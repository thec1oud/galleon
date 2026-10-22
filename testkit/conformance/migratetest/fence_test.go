package migratetest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thec1oud/galleon/pg"
)

// raise raises owner's row of f to l in a transaction of its own.
func raise(t *testing.T, pool *pgxpool.Pool, f pg.Fence, owner string, l pg.FenceLease) error {
	t.Helper()
	return pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return f.Raise(context.Background(), tx, owner, l)
	})
}

func create(t *testing.T, pool *pgxpool.Pool, f pg.Fence) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error { return f.Create(context.Background(), tx) }); err != nil {
		t.Fatal(err)
	}
}

func TestAFenceRefusesAStaleLeaseWithAStaleLeaseError(t *testing.T) {
	pool := schemaPool(t)
	f := pg.Fence{Table: pgx.Identifier{"f_fence"}}
	create(t, pool, f)
	create(t, pool, f) // creating it again changes nothing
	for _, l := range []pg.FenceLease{{Epoch: "e1", Token: 5}, {Epoch: "e1", Token: 5}, {Epoch: "e1", Token: 6}} {
		if err := raise(t, pool, f, "m", l); err != nil {
			t.Fatalf("%+v: %v", l, err)
		}
	}
	var stale *pg.StaleLeaseError
	err := raise(t, pool, f, "m", pg.FenceLease{Epoch: "e1", Token: 4})
	if !errors.As(err, &stale) || stale.Superseded || stale.Held != (pg.FenceLease{Epoch: "e1", Token: 6}) || stale.Owner != "m" {
		t.Fatalf("a lower token of the epoch: %#v", err)
	}
	// Another owner's row is its own.
	if err := raise(t, pool, f, "other", pg.FenceLease{Epoch: "e1", Token: 1}); err != nil {
		t.Fatalf("another owner: %v", err)
	}
	if err := raise(t, pool, f, "m", pg.FenceLease{Epoch: "e2", Token: 1}); err != nil {
		t.Fatalf("a new epoch: %v", err)
	}
	err = raise(t, pool, f, "m", pg.FenceLease{Epoch: "e1", Token: 99})
	if !errors.As(err, &stale) || !stale.Superseded || stale.Held.Epoch != "e2" {
		t.Fatalf("a superseded epoch: %#v", err)
	}
	var superseded []string
	if err := pool.QueryRow(context.Background(), "SELECT superseded FROM f_fence WHERE migrator = 'm'").Scan(&superseded); err != nil || len(superseded) != 1 || superseded[0] != "e1" {
		t.Errorf("superseded %q, %v", superseded, err)
	}
}

// A raised row is locked until the raising transaction ends, and a raise
// that rolls back leaves the row as it was.
func TestAFenceRowIsLockedUntilTheTransactionEnds(t *testing.T) {
	ctx := context.Background()
	pool := schemaPool(t)
	f := pg.Fence{Table: pgx.Identifier{"f_fence"}}
	create(t, pool, f)
	if err := raise(t, pool, f, "m", pg.FenceLease{Epoch: "e", Token: 1}); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := f.Raise(ctx, tx, "m", pg.FenceLease{Epoch: "e", Token: 2}); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if _, err := pool.Exec(ctx, "SELECT 1 FROM f_fence WHERE migrator = 'm' FOR UPDATE NOWAIT"); !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("locking the row while a holder had raised it: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := raise(t, pool, f, "m", pg.FenceLease{Epoch: "e", Token: 1}); err != nil {
		t.Errorf("the rolled back raise kept the token: %v", err)
	}
}
