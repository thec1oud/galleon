// Package ormtest is the conformance suite for ORM adapters: whatever runs
// an app's statements over the data seam, an ORM, a query builder or a
// hand-written repository. Every adapter runs it in its own tests, as a
// repository of the suite's model, Widget:
//
//	func TestConformance(t *testing.T) {
//		ormtest.Run(t, ormtest.Factory{New: func(t *testing.T, s ormtest.Setup) ormtest.Adapter {
//			return mywidgets.New(myorm.Table(s.Schema, s.Table), myorm.AfterSave(s.Saved))
//		}})
//	}
//
// It checks the rules of the ORM seam (§6.2 of the seam specifications,
// https://github.com/thec1oud/galleon/blob/0897d74/docs/design/06-seam-specifications.md):
// that every statement runs through the handle the adapter is given, or the
// one in ctx, never on a connection of its own, so it never chooses a host;
// that side effects wait for the commit; and that the models it registers
// are valid and match the tables it writes. Reference is an adapter that
// passes it.
//
// An ORM that connects when it is opened, as GORM pings its database, must
// have that turned off (for GORM, DisableAutomaticPing), or be opened
// without a DSN: the suite counts that connection as one of its own.
//
// The suite gets its database from testkit.PackageDSN, so it skips when
// testkit.PostgresEnv is unset, and fails in CI if it is.
package ormtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/pg"
	"github.com/thec1oud/galleon/testkit"
)

// Widget is the suite's model: a row of the suite's table,
//
//	CREATE TABLE <schema>.<table> (id bigint PRIMARY KEY, name text NOT NULL, count bigint NOT NULL)
type Widget struct {
	ID    int64
	Name  string
	Count int64
}

// Adapter is the adapter under test, as a repository of widgets. Each
// method is given a ctx that carries the transaction (data.From), and the
// same transaction explicitly; the adapter runs every statement through one
// of them. It begins nothing through a data.Manager: a nested transaction
// is the handle's own Begin or BeginWrite.
type Adapter interface {
	// Insert stores a new widget, and arranges for Setup.Saved to run with
	// it once the transaction commits.
	Insert(ctx context.Context, q data.Querier, w Widget) error
	// Find returns the widget with the id, as the transaction sees it.
	Find(ctx context.Context, r data.Reader, id int64) (Widget, error)
	// Update stores w's name and count over the widget with its id, and
	// arranges for Setup.Saved to run with it once the transaction commits.
	Update(ctx context.Context, q data.Querier, w Widget) error
	// Models returns the models the adapter registers with galleon.Collect,
	// or nil if it registers none.
	Models() []data.ModelInfo
}

// Setup is what the adapter under test is configured with.
type Setup struct {
	// DSN is the database, as an ORM configured for it would know it. The
	// adapter must not connect with it, not even to check it when it is
	// built: the suite gives it every handle it needs, and counts every
	// connection made with this DSN, which points at a proxy the suite
	// watches, from before the adapter is built.
	DSN string
	// Schema and Table are the widgets' table.
	Schema, Table string
	// Saved is the side effect of saving a widget, as an ORM's after-save
	// hook would run it: the adapter must register it with the AfterCommit
	// of the transaction it saved through, so it runs once that commits,
	// and never when it, or the savepoint it is, rolls back.
	Saved func(ctx context.Context, w Widget)
}

// Factory makes the adapter under test.
type Factory struct {
	// New returns an adapter configured from s.
	New func(t *testing.T, s Setup) Adapter
}

// Run runs every conformance test. Rule numbers are those of the ORM
// seam's specification (§6.2, linked above).
func Run(t *testing.T, f Factory) {
	t.Run("StatementsGoThroughTheHandle", func(t *testing.T) { report(t, statementsGoThroughTheHandle(newEnv(t, f))) }) // rule 1
	t.Run("NoConnectionOfItsOwn", func(t *testing.T) { report(t, noConnectionOfItsOwn(newEnv(t, f))) })                 // rules 1, 2
	t.Run("SideEffectsWaitForTheCommit", func(t *testing.T) { report(t, sideEffectsWaitForTheCommit(newEnv(t, f))) })   // rule 3
	t.Run("ModelsMatchTheTablesItWrites", func(t *testing.T) {
		e := newEnv(t, f)
		if e.a.Models() == nil {
			t.Skip("the adapter registers no models")
		}
		report(t, modelsMatchTheTablesItWrites(e))
	})
}

func report(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Error(err)
	}
}

// env is one test's adapter, the Manager whose transactions it is given,
// and a pool outside them for checking what was committed.
type env struct {
	t     *testing.T
	s     Setup
	a     Adapter
	m     *pg.Manager
	pool  *pgxpool.Pool
	proxy *testkit.Proxy // Setup.DSN's
	saved saved
}

// saved records the widgets Setup.Saved ran with.
type saved struct {
	mu sync.Mutex
	ws []Widget
}

func (s *saved) add(_ context.Context, w Widget) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ws = append(s.ws, w)
}

func (s *saved) get() []Widget {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ws)
}

func newEnv(t *testing.T, f Factory) *env {
	t.Helper()
	dsn := testkit.PackageDSN(t)
	var b [6]byte
	_, _ = rand.Read(b[:])
	e := &env{t: t, proxy: testkit.NewProxy(t, dsn)}
	e.s = Setup{DSN: e.proxy.DSN(dsn), Schema: "ormtest_" + hex.EncodeToString(b[:]), Table: "widgets", Saved: e.saved.add}
	c := ctxFor(t)
	pool, err := pgxpool.New(c, dsn)
	if err != nil {
		t.Fatal(err)
	}
	e.pool = pool
	if _, err := pool.Exec(c, "CREATE SCHEMA "+e.s.Schema+"; CREATE TABLE "+e.table()+" (id bigint PRIMARY KEY, name text NOT NULL, count bigint NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DROP SCHEMA "+e.s.Schema+" CASCADE"); err != nil {
			t.Logf("dropping %s: %v", e.s.Schema, err)
		}
		pool.Close()
	})
	if e.m, err = pg.New(pg.Config{Primary: pg.Source{DSN: dsn, MaxConns: 4}}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Start(c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := e.m.Stop(ctx); err != nil {
			t.Errorf("stopping the Manager: %v", err)
		}
	})
	if e.a = f.New(t, e.s); e.a == nil {
		t.Fatal("the factory returned no adapter")
	}
	return e
}

func ctxFor(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func (e *env) table() string { return data.Identifier{e.s.Schema, e.s.Table}.Sanitize() }

// committed returns the widget with the id as committed, outside every
// transaction of the suite's, or false when there is none.
func (e *env) committed(ctx context.Context, id int64) (Widget, bool, error) {
	var w Widget
	err := e.pool.QueryRow(ctx, "SELECT id, name, count FROM "+e.table()+" WHERE id = $1", id).Scan(&w.ID, &w.Name, &w.Count)
	if errors.Is(err, data.ErrNoRows) {
		return Widget{}, false, nil
	}
	return w, err == nil, err
}

// errRollback ends a closure so that its transaction rolls back.
var errRollback = errors.New("ormtest: rolling back on purpose")

// call runs one of the adapter's methods, named name, failing unless it
// returns nil and runs at least one statement through rec.
func call(rec *recording, name string, fn func() error) error {
	before := rec.count()
	if err := fn(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if rec.count() == before {
		return fmt.Errorf("%s ran no statement through the handle it was given, or the one in ctx: it bypassed both", name)
	}
	return nil
}

// write runs fn in a write transaction, handing it a ctx that carries the
// recorded handle, and that handle.
func (e *env) write(ctx context.Context, rec *recording, fn func(ctx context.Context, q data.WriteTx) error, opts ...data.WriteOption) error {
	return e.m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
		q := rec.writer(w)
		return fn(data.With(ctx, q), q)
	}, opts...)
}

func statementsGoThroughTheHandle(e *env) error {
	ctx := ctxFor(e.t)
	rec := &recording{}
	var errs []error
	w := Widget{ID: 1, Name: "first", Count: 1}
	err := e.write(ctx, rec, func(ctx context.Context, q data.WriteTx) error {
		if err := call(rec, "Insert", func() error { return e.a.Insert(ctx, q, w) }); err != nil {
			return err
		}
		if err := e.found(ctx, rec, q, w, "the widget it inserted in the same transaction, uncommitted"); err != nil {
			return err
		}
		w.Name, w.Count = "renamed", 2
		if err := call(rec, "Update", func() error { return e.a.Update(ctx, q, w) }); err != nil {
			return err
		}
		if err := e.found(ctx, rec, q, w, "the update it made in the same transaction, uncommitted"); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		errs = append(errs, err)
	}
	if got, ok, err := e.committed(ctx, w.ID); err != nil {
		errs = append(errs, err)
	} else if ok {
		errs = append(errs, fmt.Errorf("the transaction rolled back, yet %+v is committed: a statement ran outside the handle", got))
	}

	// Committed outside, then read in a read transaction.
	w = Widget{ID: 2, Name: "second", Count: 3}
	if _, err := e.pool.Exec(ctx, "INSERT INTO "+e.table()+" (id, name, count) VALUES ($1, $2, $3)", w.ID, w.Name, w.Count); err != nil {
		return errors.Join(append(errs, err)...)
	}
	err = e.m.Read(ctx, func(ctx context.Context, r data.ReadTx) error {
		q := rec.reader(r)
		return e.found(data.With(ctx, q), rec, q, w, "a committed widget in a read transaction")
	})
	if err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// found checks that Find, through rec, returns want.
func (e *env) found(ctx context.Context, rec *recording, r data.Reader, want Widget, what string) error {
	var got Widget
	if err := call(rec, "Find", func() (err error) { got, err = e.a.Find(ctx, r, want.ID); return err }); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if got != want {
		return fmt.Errorf("Find returned %+v for %s, %+v: it didn't read through the transaction", got, what, want)
	}
	return nil
}

func noConnectionOfItsOwn(e *env) error {
	ctx := ctxFor(e.t)
	rec := &recording{}
	w := Widget{ID: 1, Name: "first", Count: 1}
	err := e.write(ctx, rec, func(ctx context.Context, q data.WriteTx) error {
		if err := e.a.Insert(ctx, q, w); err != nil {
			return fmt.Errorf("Insert: %w", err)
		}
		w.Count = 2
		if err := e.a.Update(ctx, q, w); err != nil {
			return fmt.Errorf("Update: %w", err)
		}
		_, err := e.a.Find(ctx, q, w.ID)
		return err
	})
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	err = e.m.Read(ctx, func(ctx context.Context, r data.ReadTx) error {
		q := rec.reader(r)
		_, err := e.a.Find(data.With(ctx, q), q, w.ID)
		return err
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("Find in a read transaction: %w", err))
	}
	if n := e.proxy.Opened(); n > 0 {
		errs = append(errs, fmt.Errorf("the adapter opened %d connection(s) of its own to the database: it chose where to run statements rather than use the handle it was given", n))
	}
	return errors.Join(errs...)
}

func sideEffectsWaitForTheCommit(e *env) error {
	ctx := ctxFor(e.t)
	rec := &recording{}
	var errs []error
	// since returns what Saved ran with since the mark, a count of runs.
	since := func(mark int) []Widget { return e.saved.get()[mark:] }
	early := func(mark int, when string) {
		if got := since(mark); len(got) > 0 {
			errs = append(errs, fmt.Errorf("Saved ran %s, before any commit, with %+v: side effects must wait for AfterCommit", when, got))
		}
	}
	// Each part saves widgets of its own, and stands on its own.

	// Rolled back: Saved never runs.
	mark := len(e.saved.get())
	w := Widget{ID: 1, Name: "first", Count: 1}
	err := e.write(ctx, rec, func(ctx context.Context, q data.WriteTx) error {
		if err := e.a.Insert(ctx, q, w); err != nil {
			return fmt.Errorf("Insert: %w", err)
		}
		early(mark, "after Insert")
		if err := e.a.Update(ctx, q, w); err != nil {
			return fmt.Errorf("Update: %w", err)
		}
		early(mark, "after Update")
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		errs = append(errs, err)
	} else if got := since(mark); len(got) > 0 {
		errs = append(errs, fmt.Errorf("Saved ran for a transaction that rolled back, with %+v", got))
	}

	// Committed: once for each save, after the commit, in order.
	mark = len(e.saved.get())
	inserted, updated := Widget{ID: 2, Name: "second", Count: 1}, Widget{ID: 2, Name: "second", Count: 2}
	err = e.write(ctx, rec, func(ctx context.Context, q data.WriteTx) error {
		if err := e.a.Insert(ctx, q, inserted); err != nil {
			return fmt.Errorf("Insert: %w", err)
		}
		if err := e.a.Update(ctx, q, updated); err != nil {
			return fmt.Errorf("Update: %w", err)
		}
		early(mark, "before the commit")
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	} else if got := since(mark); !slices.Equal(got, []Widget{inserted, updated}) {
		errs = append(errs, fmt.Errorf("after the commit Saved ran with %+v, want once for each save: %+v", got, []Widget{inserted, updated}))
	}

	// Saved through a savepoint that rolled back: never, though the
	// transaction commits.
	mark = len(e.saved.get())
	dropped := Widget{ID: 3, Name: "third", Count: 1}
	err = e.write(ctx, rec, func(ctx context.Context, q data.WriteTx) error {
		sp, err := q.BeginWrite(ctx)
		if err != nil {
			return err
		}
		if err := e.a.Insert(data.With(ctx, sp), sp, dropped); err != nil {
			return fmt.Errorf("Insert in a savepoint: %w", err)
		}
		return sp.Rollback(ctx)
	})
	if err != nil {
		errs = append(errs, err)
	} else if got := since(mark); len(got) > 0 {
		errs = append(errs, fmt.Errorf("Saved ran with %+v, saved through a savepoint that rolled back: register it with the handle the save went through", got))
	}

	// Retried: once, for the attempt that committed.
	mark = len(e.saved.get())
	attempts := 0
	retried := Widget{ID: 4, Name: "fourth", Count: 1}
	err = e.write(ctx, rec, func(ctx context.Context, q data.WriteTx) error {
		attempts++
		if err := e.a.Insert(ctx, q, retried); err != nil {
			return fmt.Errorf("Insert: %w", err)
		}
		if attempts == 1 {
			return &pgconn.PgError{Code: "40001", Message: "ormtest: an injected serialization failure"}
		}
		return nil
	}, data.Retry(data.RetryPolicy{Attempts: 2, Backoff: time.Millisecond}))
	if err != nil {
		errs = append(errs, err)
	} else if got := since(mark); attempts != 2 || !slices.Equal(got, []Widget{retried}) {
		errs = append(errs, fmt.Errorf("a closure that ran %d times, retried once, ran Saved with %+v, want once: %+v", attempts, got, retried))
	}
	return errors.Join(errs...)
}

func modelsMatchTheTablesItWrites(e *env) error {
	ctx := ctxFor(e.t)
	models := e.a.Models()
	if _, err := data.NewModels(models...); err != nil {
		return fmt.Errorf("the models the adapter registers are invalid: %w", err)
	}
	var errs []error
	if !slices.ContainsFunc(models, func(m data.ModelInfo) bool { return m.Schema == e.s.Schema && m.Table == e.s.Table }) {
		errs = append(errs, fmt.Errorf("no model it registers is stored in %s, the table it writes", e.table()))
	}
	for _, m := range models {
		if err := e.matches(ctx, m); err != nil {
			errs = append(errs, fmt.Errorf("model %q: %w", m.Name, err))
		}
	}

	// The models its statements name are among them.
	rec := &recording{}
	w := Widget{ID: 1, Name: "first", Count: 1}
	err := e.write(ctx, rec, func(ctx context.Context, q data.WriteTx) error {
		if err := e.a.Insert(ctx, q, w); err != nil {
			return fmt.Errorf("Insert: %w", err)
		}
		if err := e.a.Update(ctx, q, w); err != nil {
			return fmt.Errorf("Update: %w", err)
		}
		_, err := e.a.Find(ctx, q, w.ID)
		return err
	})
	if err != nil {
		errs = append(errs, err)
	}
	for _, name := range rec.models() {
		if !slices.ContainsFunc(models, func(m data.ModelInfo) bool { return m.Name == name }) {
			errs = append(errs, fmt.Errorf("a statement is marked for model %q (data.ForModel), which the adapter doesn't register", name))
		}
	}
	return errors.Join(errs...)
}

// matches checks m against the catalog: its table exists, its key is the
// table's primary key, in order, and its columns are the table's.
func (e *env) matches(ctx context.Context, m data.ModelInfo) error {
	table := m.Identifier().Sanitize()
	var exists bool
	if err := e.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("its table %s doesn't exist", table)
	}
	key, err := e.names(ctx, `SELECT a.attname FROM pg_index i
		CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indrelid = $1::regclass AND i.indisprimary ORDER BY k.ord`, table)
	if err != nil {
		return err
	}
	if !slices.Equal(key, m.Key) {
		return fmt.Errorf("its key is %v, but %s's primary key is %v", m.Key, table, key)
	}
	cols, err := e.names(ctx, "SELECT attname FROM pg_attribute WHERE attrelid = $1::regclass AND attnum > 0 AND NOT attisdropped", table)
	if err != nil {
		return err
	}
	for _, c := range m.Columns {
		if !slices.Contains(cols, c) {
			return fmt.Errorf("it maps column %q, which %s doesn't have", c, table)
		}
	}
	return nil
}

func (e *env) names(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := e.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
