package testkit_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/testkit"
)

var notes = testkit.Schema(`CREATE TABLE notes (id int PRIMARY KEY, body text NOT NULL)`)

func TestDBSkipsWithoutPostgres(t *testing.T) {
	t.Setenv(testkit.PostgresEnv, "")
	t.Setenv("CI", "")
	ran := false
	t.Run("inner", func(t *testing.T) {
		testkit.DB(t, notes)
		ran = true
	})
	if ran {
		t.Fatal("DB returned without a server configured")
	}
}

func TestDBGivesEachTestItsOwnTransaction(t *testing.T) {
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			m := testkit.DB(t, notes).Manager()
			ctx := context.Background()
			if err := m.Read(ctx, func(ctx context.Context, r data.ReadTx) error {
				var n int
				if err := r.QueryRow(ctx, "SELECT count(*) FROM notes").Scan(&n); err != nil {
					return err
				}
				if n != 0 {
					t.Errorf("the %s test sees %d rows another test committed", name, n)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
				_, err := w.Exec(ctx, "INSERT INTO notes VALUES (1, 'hello')")
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDBClonesTheTemplateOncePerPackage(t *testing.T) {
	a, b := testkit.DB(t, notes), testkit.DB(t, notes)
	if a.DSN() != b.DSN() {
		t.Fatal("one schema got two package databases")
	}
	if other := testkit.DB(t, testkit.Schema("CREATE TABLE other (id int)")); other.DSN() == a.DSN() {
		t.Fatal("two schemas share a database")
	}
	conn, err := pgx.Connect(context.Background(), a.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var exists bool
	if err := conn.QueryRow(context.Background(), "SELECT to_regclass('notes') IS NOT NULL").Scan(&exists); err != nil || !exists {
		t.Fatalf("the package database lacks the schema: %v", err)
	}
}

func TestDBRootsBehaveAsRoots(t *testing.T) {
	m := testkit.DB(t, notes).Manager()
	ctx := context.Background()
	committed := false
	err := m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
		w.AfterCommit(func(context.Context) { committed = true })
		_, err := w.Exec(ctx, "INSERT INTO notes VALUES (1, 'a')")
		return err
	})
	if err != nil || !committed {
		t.Fatalf("a root's commit didn't run its callbacks: %v", err)
	}
	// Rolling back a root undoes it within the test's transaction.
	_ = m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
		_, _ = w.Exec(ctx, "INSERT INTO notes VALUES (2, 'b')")
		return errors.New("undo")
	})
	err = m.Read(ctx, func(ctx context.Context, r data.ReadTx) error {
		var n int
		if err := r.QueryRow(ctx, "SELECT count(*) FROM notes").Scan(&n); err != nil || n != 1 {
			t.Errorf("want only the committed row, got %d (%v)", n, err)
		}
		rows, err := r.Query(ctx, "INSERT INTO notes VALUES (3, 'c')")
		if err == nil {
			rows.Close()
			err = rows.Err()
		}
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "25006" {
			t.Errorf("a read wrote: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The read's READ ONLY ended with it.
	if err := m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
		_, err := w.Exec(ctx, "INSERT INTO notes VALUES (4, 'd')")
		return err
	}); err != nil {
		t.Fatalf("a write after a read: %v", err)
	}
	if err := m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
		return m.Read(ctx, func(context.Context, data.ReadTx) error { return nil }, data.Independent)
	}); err == nil {
		t.Fatal("an Independent transaction ran on the test's one connection")
	}
}

func TestDBRootsTakeTurns(t *testing.T) {
	m := testkit.DB(t, notes).Manager()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
				time.Sleep(time.Millisecond)
				_, err := w.Exec(ctx, "INSERT INTO notes VALUES ($1, 'x')", i)
				return err
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

type None struct{}

func TestDBModuleTracksLeaks(t *testing.T) {
	db := testkit.DB(t, notes)
	var m data.Manager
	mod := galleon.ModuleFunc("notes", func(r *galleon.Registry) error {
		r.Operation(op.Unary[None, None]("notes.leak").Public(), func(ctx context.Context, _ *None) (*None, error) {
			_, err := m.BeginWrite(ctx)
			return &None{}, err
		})
		return nil
	})
	h := testkit.New(t, galleon.Modules(db.Module(), mod))
	m = testkit.Get[data.Manager](h)
	_, err := testkit.Get[*galleon.Operations](h).Call(context.Background(), "notes.leak", &None{})
	if errs.From(err).Code != data.CodeTxLeaked {
		t.Fatalf("got %v", err)
	}
	// The leaked root gave its turn back.
	if err := m.Write(context.Background(), func(context.Context, data.WriteTx) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
