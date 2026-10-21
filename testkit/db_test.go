package testkit_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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
	"github.com/thec1oud/galleon/testkit/conformance/datatest"
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

func TestDBPingDoesNotRaceTransactions(t *testing.T) {
	m := testkit.DB(t, notes).Manager()
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 50 {
			if err := m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
				_, err := w.Exec(ctx, "INSERT INTO notes VALUES ($1, 'x')", i)
				return err
			}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			if err := m.Ping(ctx, ""); err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
		if err := m.Ping(ctx, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDBBuildsTheTemplateUnderATemporaryName(t *testing.T) {
	// The schema fails if it runs in a database that already has the
	// template's final name, which a later run would clone even if this one
	// died halfway. The comment makes the schema, and so its template, new.
	schema := fmt.Sprintf(`-- %d
CREATE TABLE marker (id int);
DO $$ BEGIN
	IF current_database() ~ '^galleon_tpl_[0-9a-f]+$' THEN
		RAISE EXCEPTION 'the template is built under its final name';
	END IF;
END $$`, time.Now().UnixNano())
	var name string
	// Registered first, so it runs after DB's own cleanup.
	t.Cleanup(func() {
		if name == "" {
			return
		}
		admin, err := pgx.Connect(context.Background(), strings.TrimSpace(os.Getenv(testkit.PostgresEnv)))
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close(context.Background())
		// galleon_pkg_<dir>_<key>_<pid>_<random>
		parts := strings.Split(name, "_")
		if len(parts) != 6 {
			t.Fatalf("can't read the schema's key from %s", name)
		}
		key := parts[3]
		for _, db := range []string{name, "galleon_tpl_" + key} {
			if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)"); err != nil {
				t.Error(err)
			}
		}
	})
	db := testkit.DB(t, testkit.Schema(schema))
	conn, err := pgx.Connect(context.Background(), db.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var exists bool
	if err := conn.QueryRow(context.Background(), "SELECT to_regclass('marker') IS NOT NULL, current_database()").Scan(&exists, &name); err != nil || !exists {
		t.Fatalf("the clone lacks the schema: %v", err)
	}
}

func TestDBModuleUsesTheAppsTelemetry(t *testing.T) {
	db := testkit.DB(t, notes)
	var m data.Manager
	mod := galleon.ModuleFunc("notes", func(r *galleon.Registry) error {
		r.Operation(op.Unary[None, None]("notes.leak").Public(), func(ctx context.Context, _ *None) (*None, error) {
			_, err := m.BeginRead(ctx)
			return &None{}, err
		})
		return nil
	})
	tp := &datatest.Recorder{}
	h := testkit.New(t, galleon.Modules(db.Module(), mod), galleon.TracerProvider(tp))
	m = testkit.Get[data.Manager](h)
	if err := m.Write(context.Background(), func(ctx context.Context, w data.WriteTx) error {
		_, err := w.Exec(ctx, "INSERT INTO notes VALUES (1, 'x')")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(tp.Find(func(s *datatest.Span) bool { return s.Attr("galleon.data.access") == "write" })) != 1 {
		t.Fatalf("no transaction span among %v", tp.Names())
	}
	if len(tp.Find(func(s *datatest.Span) bool { return strings.HasPrefix(s.Attr("db.query.text"), "INSERT") })) != 1 {
		t.Fatalf("no statement span among %v", tp.Names())
	}
	_, _ = testkit.Get[*galleon.Operations](h).Call(context.Background(), "notes.leak", &None{})
	if !h.Logs.Contains("transaction still open") {
		t.Fatalf("the leak wasn't logged to the app's logger:\n%s", h.Logs)
	}
}

// Each app on one DB gets the spans sent while it runs, and none after it
// stops.
func TestDBModuleScopesTelemetryToTheRunningApp(t *testing.T) {
	db := testkit.DB(t, notes)
	m := db.Manager()
	write := func() {
		t.Helper()
		if err := m.Write(context.Background(), func(ctx context.Context, w data.WriteTx) error {
			_, err := w.Exec(ctx, "SELECT 1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	writes := func(tp *datatest.Recorder) int {
		return len(tp.Find(func(s *datatest.Span) bool { return s.Attr("galleon.data.access") == "write" }))
	}
	first, second := &datatest.Recorder{}, &datatest.Recorder{}
	t.Run("first", func(t *testing.T) {
		testkit.New(t, galleon.Modules(db.Module()), galleon.TracerProvider(first))
		write()
	})
	t.Run("second", func(t *testing.T) {
		testkit.New(t, galleon.Modules(db.Module()), galleon.TracerProvider(second))
		write()
	})
	write()
	if got := writes(first); got != 1 {
		t.Errorf("the first app got %d write spans, want 1", got)
	}
	if got := writes(second); got != 1 {
		t.Errorf("the second app got %d write spans, want 1", got)
	}
}

// Apps on one DB that stop in another order than they started leave the
// spans to the one still running, then to none.
func TestDBModuleTelemetryFollowsTheAppsStillRunning(t *testing.T) {
	db := testkit.DB(t, notes)
	m := db.Manager()
	write := func() {
		t.Helper()
		if err := m.Write(context.Background(), func(ctx context.Context, w data.WriteTx) error {
			_, err := w.Exec(ctx, "SELECT 1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	writes := func(tp *datatest.Recorder) int {
		return len(tp.Find(func(s *datatest.Span) bool { return s.Attr("galleon.data.access") == "write" }))
	}
	stop := func(h *testkit.Harness) {
		t.Helper()
		if err := h.App.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	a, b := &datatest.Recorder{}, &datatest.Recorder{}
	ha := testkit.New(t, galleon.Modules(db.Module()), galleon.TracerProvider(a))
	hb := testkit.New(t, galleon.Modules(db.Module()), galleon.TracerProvider(b))
	write() // to b, started last
	stop(ha)
	write() // to b, still running
	stop(hb)
	write() // to neither
	if got := writes(a); got != 0 {
		t.Errorf("a got %d write spans, want 0", got)
	}
	if got := writes(b); got != 2 {
		t.Errorf("b got %d write spans, want 2", got)
	}
}

// writesAtStart is a provider that uses the data plane as it starts.
type writesAtStart struct{ m data.Manager }

func (w *writesAtStart) Start(ctx context.Context) error {
	return w.m.Write(ctx, func(ctx context.Context, tx data.WriteTx) error {
		_, err := tx.Exec(ctx, "SELECT 1")
		return err
	})
}

// A provider that depends on data.Manager starts once the app's telemetry
// is in place, whatever order the modules are listed in.
func TestDBModuleTelemetryIsInPlaceForProvidersThatStartWithIt(t *testing.T) {
	db := testkit.DB(t, notes)
	mod := galleon.ModuleFunc("early", func(r *galleon.Registry) error {
		galleon.Provide[*writesAtStart](r, func(m data.Manager) *writesAtStart { return &writesAtStart{m: m} })
		return nil
	})
	tp := &datatest.Recorder{}
	testkit.New(t, galleon.Modules(mod, db.Module()), galleon.TracerProvider(tp))
	if len(tp.Find(func(s *datatest.Span) bool { return s.Attr("galleon.data.access") == "write" })) != 1 {
		t.Fatalf("the write at start went unrecorded among %v", tp.Names())
	}
}

func TestPackageDSNIsTheDatabaseDBUsesWithoutATransaction(t *testing.T) {
	dsn := testkit.PackageDSN(t)
	if db := testkit.DB(t); db.DSN() != dsn {
		t.Fatalf("PackageDSN is %s, DB's %s", dsn, db.DSN())
	}
}
