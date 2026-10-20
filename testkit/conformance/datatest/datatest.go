// Package datatest is the conformance suite for data.Manager
// implementations, run against a real Postgres. Every implementation runs
// it in its own tests:
//
//	func TestConformance(t *testing.T) {
//		datatest.Run(t, datatest.Factory{Options: func(t *testing.T, s datatest.Setup) []galleon.Option {
//			return []galleon.Option{galleon.Modules(mydata.Module(s.DSN, ...))}
//		}})
//	}
//
// The suite gets its database from testkit.DB, so it skips when
// testkit.PostgresEnv is unset, and fails in CI if it is.
package datatest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/testkit"
)

// Setup is what the module under test is configured with.
type Setup struct {
	// DSN is the database the primary source uses. The module must also
	// have a source named "other" on it, so the suite can check that
	// transactions don't span sources.
	DSN string
	// LogLeaks asks for a leaked transaction to be logged only, as in
	// production, rather than to fail the call.
	LogLeaks bool
}

// Factory installs the implementation under test.
type Factory struct {
	// Options returns the app options that install the data module,
	// configured from s. The module must provide data.Manager.
	Options func(t *testing.T, s Setup) []galleon.Option
}

// Run runs every conformance test. Rule numbers are those of the data
// seam's specification (06-seam-specifications.md, §6.1).
func Run(t *testing.T, f Factory) {
	t.Run("ReadTxCannotWrite", func(t *testing.T) { readTxCannotWrite(t, f) })
	t.Run("JoinIsTheDefault", func(t *testing.T) { joinIsTheDefault(t, f) })         // rule 1
	t.Run("Independent", func(t *testing.T) { independent(t, f) })                   // §7.4
	t.Run("Nested", func(t *testing.T) { nested(t, f) })                             // rule 2
	t.Run("MandatoryAndNever", func(t *testing.T) { mandatoryAndNever(t, f) })       // §7.4
	t.Run("Ownership", func(t *testing.T) { ownership(t, f) })                       // rules 2, 3
	t.Run("EndedHandles", func(t *testing.T) { endedHandles(t, f) })                 // rule 3
	t.Run("CommitHooks", func(t *testing.T) { commitHooks(t, f) })                   // rule 5
	t.Run("RetryOnInjectedFailures", func(t *testing.T) { retryOnInjected(t, f) })   // rule 6
	t.Run("RetryOnRealConflict", func(t *testing.T) { retryOnRealConflict(t, f) })   // rule 6
	t.Run("ReadOnlyAndIsolation", func(t *testing.T) { readOnlyAndIsolation(t, f) }) // rule 4
	t.Run("NoTwoPhaseCommit", func(t *testing.T) { noTwoPhaseCommit(t, f) })         // rule 9
	t.Run("LeaksFailInDevelopment", func(t *testing.T) { leaks(t, f, false) })       // rule 8
	t.Run("LeaksAreLoggedInProduction", func(t *testing.T) { leaks(t, f, true) })    // rule 8
	t.Run("Errors", func(t *testing.T) { errorKinds(t, f) })                         // conventions 1, 3
	t.Run("UnreachableIsUnavailable", func(t *testing.T) { unreachable(t, f) })      // convention 3
	t.Run("Spans", func(t *testing.T) { spans(t, f) })                               // convention 6
}

// env is one test's app, its Manager, and a pool outside it for checking
// what was committed.
type env struct {
	t     *testing.T
	h     *testkit.Harness
	m     data.Manager
	pool  *pgxpool.Pool
	tp    *Recorder
	table string
	dsn   string
}

var tables atomic.Int64

func newEnv(t *testing.T, f Factory, s Setup, opts ...galleon.Option) *env {
	t.Helper()
	db := testkit.DB(t)
	s.DSN = db.DSN()
	e := &env{t: t, tp: &Recorder{}, dsn: s.DSN}
	e.table = fmt.Sprintf("datatest_%d_%d", time.Now().UnixNano()%1e9, tables.Add(1))
	pool, err := pgxpool.New(context.Background(), s.DSN)
	if err != nil {
		t.Fatal(err)
	}
	e.pool = pool
	t.Cleanup(pool.Close)
	e.exec("CREATE TABLE " + e.table + " (id int PRIMARY KEY, v text)")
	t.Cleanup(func() { e.drop("TABLE", e.table) })
	all := append(f.Options(t, s), galleon.TracerProvider(e.tp))
	e.h = testkit.New(t, append(all, opts...)...)
	e.m = testkit.Get[data.Manager](e.h)
	return e
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// exec runs SQL outside the app, committed at once.
func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// drop drops what a test created. A transaction a broken implementation
// leaked can hold a lock on it, so it gives up after a while rather than
// hang the run; the package's database is replaced next time anyway.
func (e *env) drop(what, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.pool.Exec(ctx, "DROP "+what+" IF EXISTS "+name); err != nil {
		e.t.Logf("dropping %s: %v", name, err)
	}
}

func (e *env) insert(ctx context.Context, q data.Querier, id int) error {
	return insertRow(ctx, q, e.table, id)
}

func (e *env) mustInsert(ctx context.Context, q data.Querier, id int) {
	e.t.Helper()
	if err := e.insert(ctx, q, id); err != nil {
		e.t.Fatalf("insert %d: %v", id, err)
	}
}

// committed reports whether row id is visible outside the app, that is,
// committed.
func (e *env) committed(id int) bool {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+e.table+" WHERE id = $1", id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n == 1
}

// sees reports whether row id is visible through r.
func (e *env) sees(ctx context.Context, r data.Reader, id int) bool {
	e.t.Helper()
	var n int
	if err := r.QueryRow(ctx, "SELECT count(*) FROM "+e.table+" WHERE id = $1", id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n == 1
}

// wantCode fails unless err is an *errs.Error of kind and code.
func wantCode(t *testing.T, err error, kind errs.Kind, code string) {
	t.Helper()
	var e *errs.Error
	if !errors.As(err, &e) || e.Kind != kind || e.Code != code {
		t.Fatalf("want %v %s, got %v", kind, code, err)
	}
}

var errUndo = errors.New("undo")

func readTxCannotWrite(t *testing.T, f Factory) {
	querier := reflect.TypeFor[data.Querier]()
	rt := reflect.TypeFor[data.ReadTx]()
	for _, name := range []string{"Exec", "CopyFrom", "BeginWrite", "AfterCommit", "AfterRollback"} {
		if _, ok := rt.MethodByName(name); ok {
			t.Errorf("data.ReadTx has a %s method", name)
		}
	}
	if rt.Implements(querier) || !reflect.TypeFor[data.WriteTx]().Implements(querier) {
		t.Error("a WriteTx must be a Querier, and a ReadTx must not")
	}
	// Every option, as the options of both forms.
	_ = []data.ReadOption{data.Join, data.Independent, data.Nested, data.Mandatory, data.Never,
		data.Source("x"), data.Isolation(data.Serializable), data.Retry(data.RetryPolicy{})}
	_ = []data.WriteOption{data.Join, data.Independent, data.Nested, data.Mandatory, data.Never,
		data.Source("x"), data.Isolation(data.Serializable), data.Retry(data.RetryPolicy{})}

	e := newEnv(t, f, Setup{})
	c := ctx(t)
	r, err := e.m.BeginRead(c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Rollback(c)
	if _, ok := r.(data.WriteTx); ok {
		t.Fatal("a read transaction can be asserted to a WriteTx")
	}
	if err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		r, err := e.m.BeginRead(ctx)
		if err != nil {
			return err
		}
		defer r.Rollback(ctx)
		if _, ok := r.(data.WriteTx); ok {
			t.Error("a read view of a write transaction can be asserted to a WriteTx")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func joinIsTheDefault(t *testing.T, f Factory) {
	e := newEnv(t, f, Setup{})
	c := ctx(t)
	err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		e.mustInsert(ctx, w, 1)
		if data.From(ctx) == nil {
			t.Error("the closure's ctx doesn't carry its transaction")
		}
		r, err := e.m.BeginRead(ctx)
		if err != nil {
			return err
		}
		if !e.sees(ctx, r, 1) {
			t.Error("BeginRead inside a write transaction doesn't see its uncommitted write")
		}
		if err := r.Commit(ctx); err != nil {
			return err
		}
		if e.committed(1) {
			t.Error("committing the joined read view committed the write transaction")
		}
		if err := e.m.Read(ctx, func(ctx context.Context, r data.ReadTx) error {
			if !e.sees(ctx, r, 1) {
				t.Error("Read inside a write transaction doesn't see its uncommitted write")
			}
			return nil
		}); err != nil {
			return err
		}
		if err := e.m.Write(ctx, func(ctx context.Context, j data.WriteTx) error { return e.insert(ctx, j, 2) }); err != nil {
			return err
		}
		if e.committed(2) {
			t.Error("a joined Write committed on its own")
		}
		return errUndo
	})
	if !errors.Is(err, errUndo) {
		t.Fatalf("got %v", err)
	}
	if e.committed(1) || e.committed(2) {
		t.Fatal("rolling back the root left joined writes behind")
	}

	// With pairs, data.With puts the enclosing transaction in ctx.
	w, err := e.m.BeginWrite(c)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Rollback(c)
	e.mustInsert(c, w, 3)
	r, err := e.m.BeginRead(data.With(c, w))
	if err != nil {
		t.Fatal(err)
	}
	if !e.sees(c, r, 3) {
		t.Fatal("BeginRead with data.With doesn't join")
	}
	_ = r.Rollback(c)
	if err := w.Commit(c); err != nil || !e.committed(3) {
		t.Fatalf("a read view's rollback doomed the write transaction: %v", err)
	}
}

func independent(t *testing.T, f Factory) {
	e := newEnv(t, f, Setup{})
	c := ctx(t)
	err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		e.mustInsert(ctx, w, 1)
		if err := e.m.Read(ctx, func(ctx context.Context, r data.ReadTx) error {
			if e.sees(ctx, r, 1) {
				t.Error("an Independent read sees the enclosing transaction's uncommitted write")
			}
			return nil
		}, data.Independent); err != nil {
			return err
		}
		r, err := e.m.BeginRead(ctx, data.Independent)
		if err != nil {
			return err
		}
		if e.sees(ctx, r, 1) {
			t.Error("an Independent BeginRead sees the enclosing transaction's uncommitted write")
		}
		_ = r.Rollback(ctx)
		if err := e.m.Write(ctx, func(ctx context.Context, w data.WriteTx) error { return e.insert(ctx, w, 2) }, data.Independent); err != nil {
			return err
		}
		if !e.committed(2) {
			t.Error("an Independent write isn't committed on its own")
		}
		return errUndo
	})
	if !errors.Is(err, errUndo) {
		t.Fatalf("got %v", err)
	}
	if e.committed(1) || !e.committed(2) {
		t.Fatal("an Independent transaction must outlive the enclosing one's rollback, and nothing else")
	}
}

func nested(t *testing.T, f Factory) {
	e := newEnv(t, f, Setup{})
	c := ctx(t)
	err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		e.mustInsert(ctx, w, 1)
		if err := e.m.Write(ctx, func(ctx context.Context, n data.WriteTx) error {
			e.mustInsert(ctx, n, 2)
			return errUndo
		}, data.Nested); !errors.Is(err, errUndo) {
			t.Errorf("got %v", err)
		}
		if err := e.m.Write(ctx, func(ctx context.Context, n data.WriteTx) error { return e.insert(ctx, n, 3) }, data.Nested); err != nil {
			return err
		}
		// A failed statement in a savepoint doesn't doom the transaction.
		if err := e.m.Write(ctx, func(ctx context.Context, n data.WriteTx) error { return e.insert(ctx, n, 1) }, data.Nested); errs.KindOf(err) != errs.Conflict {
			t.Errorf("a duplicate in a savepoint: %v", err)
		}
		n, err := w.BeginWrite(ctx)
		if err != nil {
			return err
		}
		e.mustInsert(ctx, n, 4)
		if err := n.Rollback(ctx); err != nil {
			return err
		}
		n, err = w.BeginWrite(ctx)
		if err != nil {
			return err
		}
		e.mustInsert(ctx, n, 5)
		if err := n.Commit(ctx); err != nil {
			return err
		}
		if e.committed(5) {
			t.Error("a nested Commit committed the root")
		}
		if !e.sees(ctx, w, 5) || e.sees(ctx, w, 4) || e.sees(ctx, w, 2) {
			t.Error("the root doesn't see exactly the work of released savepoints")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int]bool{1: true, 2: false, 3: true, 4: false, 5: true} {
		if e.committed(id) != want {
			t.Errorf("row %d committed: %v, want %v", id, !want, want)
		}
	}

	// Rolling back the root undoes released savepoints too.
	w, err := e.m.BeginWrite(c)
	if err != nil {
		t.Fatal(err)
	}
	n, err := w.BeginWrite(c)
	if err != nil {
		t.Fatal(err)
	}
	e.mustInsert(c, n, 6)
	if err := n.Commit(c); err != nil {
		t.Fatal(err)
	}
	if err := w.Rollback(c); err != nil || e.committed(6) {
		t.Fatalf("a released savepoint survived the root's rollback: %v", err)
	}

	// With no enclosing transaction, Nested begins one.
	if err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error { return e.insert(ctx, w, 7) }, data.Nested); err != nil || !e.committed(7) {
		t.Fatalf("Nested with no enclosing transaction: %v", err)
	}
}

func mandatoryAndNever(t *testing.T, f Factory) {
	e := newEnv(t, f, Setup{})
	c := ctx(t)
	if _, err := e.m.BeginWrite(c, data.Mandatory); err == nil {
		t.Fatal("Mandatory began a transaction")
	} else {
		wantCode(t, err, errs.Internal, data.CodePropagation)
	}
	if err := e.m.Read(c, func(context.Context, data.ReadTx) error { return nil }, data.Mandatory); err == nil {
		t.Fatal("a Mandatory Read ran without a transaction")
	}
	err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		if err := e.m.Write(ctx, func(ctx context.Context, j data.WriteTx) error { return e.insert(ctx, j, 1) }, data.Mandatory); err != nil {
			return err
		}
		if e.committed(1) {
			t.Error("Mandatory didn't join")
		}
		if _, err := e.m.BeginRead(ctx, data.Never); err == nil {
			t.Error("Never began a transaction inside one")
		} else {
			wantCode(t, err, errs.Internal, data.CodePropagation)
		}
		if err := e.m.Write(ctx, func(context.Context, data.WriteTx) error { return nil }, data.Never); err == nil {
			t.Error("a Never Write ran inside a transaction")
		}
		return nil
	})
	if err != nil || !e.committed(1) {
		t.Fatalf("got %v", err)
	}
	if err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error { return e.insert(ctx, w, 2) }, data.Never); err != nil || !e.committed(2) {
		t.Fatalf("Never without a transaction: %v", err)
	}
}

func ownership(t *testing.T, f Factory) {
	e := newEnv(t, f, Setup{})
	c := ctx(t)

	// Only the root commits.
	w, err := e.m.BeginWrite(c)
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.m.BeginWrite(data.With(c, w))
	if err != nil {
		t.Fatal(err)
	}
	e.mustInsert(c, j, 1)
	if err := j.Commit(c); err != nil {
		t.Fatal(err)
	}
	if e.committed(1) {
		t.Fatal("a joined handle's Commit committed")
	}
	if err := w.Commit(c); err != nil || !e.committed(1) {
		t.Fatalf("the root's Commit: %v", err)
	}

	// A joined handle that rolls back dooms the root.
	w, err = e.m.BeginWrite(c)
	if err != nil {
		t.Fatal(err)
	}
	e.mustInsert(c, w, 2)
	j, err = e.m.BeginWrite(data.With(c, w))
	if err != nil {
		t.Fatal(err)
	}
	e.mustInsert(c, j, 3)
	if err := j.Rollback(c); err != nil {
		t.Fatal(err)
	}
	err = w.Commit(c)
	wantCode(t, err, errs.Internal, data.CodeRollbackOnly)
	if e.committed(2) || e.committed(3) {
		t.Fatal("the root committed after a joined handle rolled back")
	}

	// A root with a savepoint still open doesn't commit.
	w, err = e.m.BeginWrite(c)
	if err != nil {
		t.Fatal(err)
	}
	n, err := w.BeginWrite(c)
	if err != nil {
		t.Fatal(err)
	}
	e.mustInsert(c, n, 4)
	wantCode(t, w.Commit(c), errs.Internal, data.CodeTxOpen)
	if e.committed(4) {
		t.Fatal("committed with a nested transaction open")
	}
	if err := n.Commit(c); err == nil {
		t.Fatal("a nested handle outlived its root")
	}
}

func endedHandles(t *testing.T, f Factory) {
	e := newEnv(t, f, Setup{})
	c := ctx(t)
	w, err := e.m.BeginWrite(c)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.m.BeginRead(data.With(c, w))
	if err != nil {
		t.Fatal(err)
	}
	n, err := w.BeginWrite(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Commit(c); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(c); err != nil {
		t.Fatal(err)
	}
	if err := w.Rollback(c); err != nil {
		t.Fatalf("Rollback after Commit: %v", err)
	}
	wantCode(t, w.Commit(c), errs.Internal, data.CodeTxEnded)
	wantCode(t, e.insert(c, w, 1), errs.Internal, data.CodeTxEnded)
	wantCode(t, e.insert(c, n, 1), errs.Internal, data.CodeTxEnded)
	var x int
	wantCode(t, r.QueryRow(c, "SELECT 1").Scan(&x), errs.Internal, data.CodeTxEnded)
	if _, err := r.Query(c, "SELECT 1"); err == nil {
		t.Fatal("a read view outlived its root")
	}
	if _, err := w.BeginWrite(c); err == nil {
		t.Fatal("began a savepoint in an ended transaction")
	}
	if err := e.m.Write(data.With(c, w), func(context.Context, data.WriteTx) error { return nil }); err == nil {
		t.Fatal("joined an ended transaction")
	}

	w, err = e.m.BeginWrite(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Rollback(c); err != nil {
		t.Fatal(err)
	}
	if err := w.Rollback(c); err != nil {
		t.Fatalf("Rollback twice: %v", err)
	}
	wantCode(t, w.Commit(c), errs.Internal, data.CodeTxEnded)
	if _, err := w.SendBatch(c, &data.Batch{}).Exec(); err == nil {
		t.Fatal("a batch ran on an ended handle")
	}
}

func commitHooks(t *testing.T, f Factory) {
	e := newEnv(t, f, Setup{})
	c := ctx(t)
	var mu sync.Mutex
	var got []string
	note := func(s string) func(context.Context) {
		return func(context.Context) { mu.Lock(); got = append(got, s); mu.Unlock() }
	}
	take := func() []string { mu.Lock(); defer mu.Unlock(); g := got; got = nil; return g }

	err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		e.mustInsert(ctx, w, 1)
		w.AfterCommit(func(ctx context.Context) {
			if !e.committed(1) {
				t.Error("AfterCommit ran before the commit")
			}
			note("a")(ctx)
		})
		w.AfterRollback(note("rolled back"))
		n, err := w.BeginWrite(ctx)
		if err != nil {
			return err
		}
		n.AfterCommit(note("nested"))
		if err := n.Commit(ctx); err != nil {
			return err
		}
		n, err = w.BeginWrite(ctx)
		if err != nil {
			return err
		}
		n.AfterCommit(note("dropped"))
		n.AfterRollback(note("savepoint rolled back"))
		if err := n.Rollback(ctx); err != nil {
			return err
		}
		w.AfterCommit(note("b"))
		if err := e.m.Write(ctx, func(ctx context.Context, j data.WriteTx) error {
			j.AfterCommit(note("joined"))
			return nil
		}); err != nil {
			return err
		}
		if g := take(); !slices.Equal(g, []string{"savepoint rolled back"}) {
			t.Errorf("before the root commits, ran %v", g)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if g := take(); !slices.Equal(g, []string{"a", "nested", "b", "joined"}) {
		t.Fatalf("after commit, ran %v", g)
	}

	err = e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		w.AfterCommit(note("never"))
		w.AfterRollback(note("r1"))
		n, err := w.BeginWrite(ctx)
		if err != nil {
			return err
		}
		n.AfterCommit(note("never either"))
		n.AfterRollback(note("r2"))
		if err := n.Commit(ctx); err != nil {
			return err
		}
		return errUndo
	})
	if !errors.Is(err, errUndo) {
		t.Fatal(err)
	}
	if g := take(); !slices.Equal(g, []string{"r1", "r2"}) {
		t.Fatalf("after rollback, ran %v", g)
	}
}

// failer creates a function that raises state the first times calls, then
// succeeds. It counts with a sequence, which rollbacks don't undo.
func (e *env) failer(times int, state string) string {
	name := fmt.Sprintf("%s_fail%d", e.table, tables.Add(1))
	e.exec("CREATE SEQUENCE " + name + "_seq")
	e.exec(fmt.Sprintf(`CREATE FUNCTION %[1]s() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
	IF nextval('%[1]s_seq') <= %[2]d THEN
		RAISE EXCEPTION 'injected' USING ERRCODE = '%[3]s';
	END IF;
END $$`, name, times, state))
	e.t.Cleanup(func() {
		e.drop("FUNCTION", name+"()")
		e.drop("SEQUENCE", name+"_seq")
	})
	return "SELECT " + name + "()"
}

func retryOnInjected(t *testing.T, f Factory) {
	for _, state := range []string{"40001", "40P01"} {
		t.Run(state, func(t *testing.T) {
			e := newEnv(t, f, Setup{})
			c := ctx(t)
			fail := e.failer(2, state)
			var runs, committedHooks, rolledBackHooks int
			err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
				runs++
				w.AfterCommit(func(context.Context) { committedHooks++ })
				w.AfterRollback(func(context.Context) { rolledBackHooks++ })
				e.mustInsert(ctx, w, 1)
				_, err := w.Exec(ctx, fail)
				return err
			})
			if err != nil || runs != 3 || !e.committed(1) {
				t.Fatalf("got %v after %d runs", err, runs)
			}
			if committedHooks != 1 || rolledBackHooks != 2 {
				t.Fatalf("AfterCommit ran %d times, AfterRollback %d; failed attempts' AfterCommit callbacks must be discarded", committedHooks, rolledBackHooks)
			}
		})
	}

	e := newEnv(t, f, Setup{})
	c := ctx(t)

	// The policy bounds the attempts.
	fail := e.failer(1000, "40001")
	runs := 0
	err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		runs++
		_, err := w.Exec(ctx, fail)
		return err
	}, data.Retry(data.RetryPolicy{Attempts: 2, Backoff: time.Millisecond}))
	if runs != 2 || !data.Retryable(err) {
		t.Fatalf("got %v after %d runs", err, runs)
	}
	wantCode(t, err, errs.Conflict, data.CodeSerializationFailure)
	if !slices.ContainsFunc(errs.From(err).Details, func(d errs.Detail) bool { _, ok := d.(errs.RetryInfo); return ok }) {
		t.Fatal("a serialization failure doesn't say it can be retried")
	}

	// A pair never retries, and refuses a retry policy.
	if _, err := e.m.BeginWrite(c, data.Retry(data.RetryPolicy{})); err == nil {
		t.Fatal("BeginWrite took a retry policy")
	}
	w, err := e.m.BeginWrite(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(c, fail); !data.Retryable(err) {
		t.Fatalf("got %v", err)
	}
	_ = w.Rollback(c)

	// data.Retry reruns code that can be rerun.
	fail = e.failer(2, "40001")
	runs = 0
	err = data.Retry(data.RetryPolicy{Backoff: time.Millisecond}).Do(c, func(ctx context.Context) error {
		runs++
		w, err := e.m.BeginWrite(ctx)
		if err != nil {
			return err
		}
		defer w.Rollback(ctx)
		if _, err := w.Exec(ctx, fail); err != nil {
			return err
		}
		return w.Commit(ctx)
	})
	if err != nil || runs != 3 {
		t.Fatalf("data.Retry: %v after %d runs", err, runs)
	}

	// A joined closure leaves retrying to the root's owner.
	fail = e.failer(1, "40001")
	outer, inner := 0, 0
	err = e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		outer++
		return e.m.Write(ctx, func(ctx context.Context, j data.WriteTx) error {
			inner++
			_, err := j.Exec(ctx, fail)
			return err
		})
	})
	if err != nil || outer != 2 || inner != 2 {
		t.Fatalf("got %v with %d outer and %d inner runs", err, outer, inner)
	}
}

// retryOnRealConflict runs two serializable transactions that each read
// what the other writes, so Postgres aborts one of them, and the closure
// form reruns it.
func retryOnRealConflict(t *testing.T, f Factory) {
	e := newEnv(t, f, Setup{})
	c := ctx(t)
	var attempts atomic.Int32
	var ready sync.WaitGroup
	ready.Add(2)
	var wg sync.WaitGroup
	errc := make(chan error, 2)
	for id := 1; id <= 2; id++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			first := true
			errc <- e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
				attempts.Add(1)
				var n int
				if err := w.QueryRow(ctx, "SELECT count(*) FROM "+e.table).Scan(&n); err != nil {
					return err
				}
				if first {
					// Both read before either writes.
					first = false
					ready.Done()
					ready.Wait()
				}
				_, err := w.Exec(ctx, "INSERT INTO "+e.table+" (id, v) VALUES ($1, $2)", id, fmt.Sprint(n))
				return err
			}, data.Isolation(data.Serializable),
				// Generous: on a slow runner the peer can still be finishing
				// while the first retries run, and those conflict too.
				data.Retry(data.RetryPolicy{Attempts: 20, Backoff: 5 * time.Millisecond, MaxBackoff: 100 * time.Millisecond}))
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Fatal(err)
		}
	}
	if attempts.Load() < 3 || !e.committed(1) || !e.committed(2) {
		t.Fatalf("%d attempts; want a serialization failure and a retry", attempts.Load())
	}
}

func readOnlyAndIsolation(t *testing.T, f Factory) {
	e := newEnv(t, f, Setup{})
	c := ctx(t)
	show := func(r data.Reader, setting string) string {
		t.Helper()
		var v string
		if err := r.QueryRow(c, "SHOW "+setting).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	r, err := e.m.BeginRead(c)
	if err != nil {
		t.Fatal(err)
	}
	if show(r, "transaction_read_only") != "on" {
		t.Fatal("a read transaction isn't READ ONLY")
	}
	rows, err := r.Query(c, "INSERT INTO "+e.table+" (id, v) VALUES (1, 'x')")
	if err == nil {
		rows.Close()
		err = rows.Err()
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "25006" {
		t.Fatalf("a write through Query in a read transaction: %v", err)
	}
	_ = r.Rollback(c)

	for _, level := range []data.IsolationLevel{data.ReadCommitted, data.RepeatableRead, data.Serializable} {
		if err := e.m.Read(c, func(ctx context.Context, r data.ReadTx) error {
			if got := show(r, "transaction_isolation"); got != string(level) {
				t.Errorf("read with %s runs %s", level, got)
			}
			return nil
		}, data.Isolation(level)); err != nil {
			t.Fatal(err)
		}
	}
	err = e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		if show(w, "transaction_isolation") != "serializable" || show(w, "transaction_read_only") != "off" {
			t.Error("a serializable write transaction runs otherwise")
		}
		// A nested read is read only, and the root writes again after it.
		r, err := w.Begin(ctx)
		if err != nil {
			return err
		}
		if show(r, "transaction_read_only") != "on" {
			t.Error("a nested read in a write transaction isn't READ ONLY")
		}
		if err := r.Commit(ctx); err != nil {
			return err
		}
		if _, err := e.m.BeginRead(ctx, data.Isolation(data.ReadCommitted)); err == nil {
			t.Error("joined a serializable transaction as read committed")
		}
		return e.insert(ctx, w, 2)
	}, data.Isolation(data.Serializable))
	if err != nil || !e.committed(2) {
		t.Fatalf("got %v", err)
	}
	// A read nested in a read view of a write transaction is read only too.
	writeThrough := func(ctx context.Context, r data.Reader, id int) error {
		rows, err := r.Query(ctx, "INSERT INTO "+e.table+" (id, v) VALUES ($1, 'x')", id)
		if err == nil {
			rows.Close()
			err = rows.Err()
		}
		return err
	}
	err = e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		return e.m.Read(ctx, func(ctx context.Context, view data.ReadTx) error {
			n, err := view.Begin(ctx)
			if err != nil {
				return err
			}
			if show(n, "transaction_read_only") != "on" {
				t.Error("a read nested in a read view isn't READ ONLY")
			}
			if err := writeThrough(ctx, n, 3); !errors.As(err, &pe) || pe.Code != "25006" {
				t.Errorf("a write through a read nested in a read view: %v", err)
			}
			if err := n.Commit(ctx); err != nil {
				return err
			}
			return e.m.Read(ctx, func(ctx context.Context, n data.ReadTx) error {
				if err := writeThrough(ctx, n, 4); !errors.As(err, &pe) || pe.Code != "25006" {
					t.Errorf("a write through a Nested read in a read view: %v", err)
				}
				return nil
			}, data.Nested)
		})
	})
	if err != nil || e.committed(3) || e.committed(4) {
		t.Fatalf("a read nested in a read view wrote: %v", err)
	}

	// Joining with an explicit isolation compares it with the level the
	// transaction actually has, even when it was begun at the default.
	var def string
	if err := e.pool.QueryRow(c, "SHOW default_transaction_isolation").Scan(&def); err != nil {
		t.Fatal(err)
	}
	other := data.Serializable
	if def == string(data.Serializable) {
		other = data.ReadCommitted
	}
	err = e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		if err := e.m.Read(ctx, func(context.Context, data.ReadTx) error { return nil }, data.Isolation(data.IsolationLevel(def))); err != nil {
			t.Errorf("joining a default (%s) transaction as %s: %v", def, def, err)
		}
		err := e.m.Read(ctx, func(context.Context, data.ReadTx) error { return nil }, data.Isolation(other))
		wantCode(t, err, errs.Internal, data.CodePropagation)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := e.m.Read(c, func(ctx context.Context, r data.ReadTx) error {
		_, err := e.m.BeginWrite(ctx)
		wantCode(t, err, errs.Internal, data.CodePropagation)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func noTwoPhaseCommit(t *testing.T, f Factory) {
	e := newEnv(t, f, Setup{})
	c := ctx(t)
	err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		_, err := e.m.BeginWrite(ctx, data.Source("other"))
		wantCode(t, err, errs.Internal, data.CodeCrossSource)
		err = e.m.Read(ctx, func(context.Context, data.ReadTx) error { return nil }, data.Source("other"), data.Nested)
		wantCode(t, err, errs.Internal, data.CodeCrossSource)
		return e.m.Read(ctx, func(context.Context, data.ReadTx) error { return nil }, data.Source("other"), data.Independent)
	})
	if err != nil {
		t.Fatalf("an Independent transaction on another source: %v", err)
	}
	if _, err := e.m.BeginRead(c, data.Source("no_such_source")); err == nil {
		t.Fatal("an unknown source was accepted")
	}
}

type Empty struct{}

func leaks(t *testing.T, f Factory, logOnly bool) {
	var m data.Manager
	var table string
	mod := galleon.ModuleFunc("datatest", func(r *galleon.Registry) error {
		r.Operation(op.Unary[Empty, Empty]("datatest.leak_write").Public(), func(ctx context.Context, _ *Empty) (*Empty, error) {
			w, err := m.BeginWrite(ctx)
			if err != nil {
				return nil, err
			}
			_, err = w.Exec(ctx, "INSERT INTO "+table+" (id, v) VALUES (1, 'leaked')")
			return &Empty{}, err
		})
		r.Operation(op.Unary[Empty, Empty]("datatest.leak_read").Public(), func(ctx context.Context, _ *Empty) (*Empty, error) {
			_, err := m.BeginRead(ctx)
			return &Empty{}, err
		})
		r.Operation(op.Unary[Empty, Empty]("datatest.tidy").Public(), func(ctx context.Context, _ *Empty) (*Empty, error) {
			w, err := m.BeginWrite(ctx)
			if err != nil {
				return nil, err
			}
			defer w.Rollback(ctx)
			if err := insertRow(ctx, w, table, 2); err != nil {
				return nil, err
			}
			return &Empty{}, w.Commit(ctx)
		})
		return nil
	})
	e := newEnv(t, f, Setup{LogLeaks: logOnly}, galleon.Modules(mod))
	m, table = e.m, e.table
	ops := testkit.Get[*galleon.Operations](e.h)
	c := ctx(t)

	out, err := ops.Call(c, "datatest.leak_write", &Empty{})
	if logOnly {
		if err != nil || out == nil {
			t.Fatalf("in production a leak is only logged, but the call returned %v, %v", out, err)
		}
	} else {
		wantCode(t, err, errs.Internal, data.CodeTxLeaked)
		if !strings.Contains(err.Error(), "datatest.go:") {
			t.Fatalf("the error doesn't say where the transaction began:\n%v", err)
		}
		if p := errs.ToProblem(err, ""); strings.Contains(p.Title, "datatest") {
			t.Fatalf("a client sees where the transaction began: %+v", p)
		}
	}
	if e.committed(1) {
		t.Fatal("a leaked transaction was committed")
	}
	if !e.h.Logs.Contains("datatest.go:") {
		t.Fatalf("the leak wasn't logged with where it began:\n%s", e.h.Logs)
	}

	// Leaks are rolled back, so connections aren't lost: many more calls
	// than any pool has connections still run.
	for range 40 {
		c, cancel := context.WithTimeout(c, 5*time.Second)
		_, err := ops.Call(c, "datatest.leak_read", &Empty{})
		cancel()
		if (err == nil) != logOnly {
			t.Fatalf("got %v", err)
		}
	}
	if _, err := ops.Call(c, "datatest.tidy", &Empty{}); err != nil || !e.committed(2) {
		t.Fatalf("a call that ends its transaction: %v", err)
	}
}

func insertRow(ctx context.Context, q data.Querier, table string, id int) error {
	_, err := q.Exec(ctx, "INSERT INTO "+table+" (id, v) VALUES ($1, 'x')", id)
	return err
}

func errorKinds(t *testing.T, f Factory) {
	e := newEnv(t, f, Setup{})
	c := ctx(t)
	p, ch := e.table+"_p", e.table+"_c"
	e.exec("CREATE TABLE " + p + " (id int PRIMARY KEY)")
	e.exec("CREATE TABLE " + ch + " (id int PRIMARY KEY, p int REFERENCES " + p + ", n int NOT NULL, pos int CHECK (pos > 0), span int4range, EXCLUDE USING gist (span WITH &&))")
	t.Cleanup(func() { e.drop("TABLE", ch+", "+p) })
	e.exec("INSERT INTO " + p + " VALUES (1)")
	e.exec("INSERT INTO " + ch + " VALUES (1, 1, 1, 1, '[1,5)')")
	fail := e.failer(1, "40001")
	for _, tc := range []struct {
		name string
		sql  string
		kind errs.Kind
		code string
	}{
		{"unique", "INSERT INTO " + ch + " VALUES (1, 1, 1, 1, NULL)", errs.Conflict, data.CodeUniqueViolation},
		{"exclusion", "INSERT INTO " + ch + " VALUES (2, 1, 1, 1, '[3,8)')", errs.Conflict, data.CodeExclusionViolation},
		{"check", "INSERT INTO " + ch + " VALUES (3, 1, 1, -1, NULL)", errs.FailedPrecondition, data.CodeCheckViolation},
		{"foreign key", "INSERT INTO " + ch + " VALUES (4, 99, 1, 1, NULL)", errs.FailedPrecondition, data.CodeForeignKeyViolation},
		{"not null", "INSERT INTO " + ch + " VALUES (5, 1, NULL, 1, NULL)", errs.FailedPrecondition, data.CodeNotNullViolation},
		{"serialization", fail, errs.Conflict, data.CodeSerializationFailure},
	} {
		err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
			_, err := w.Exec(ctx, tc.sql)
			return err
		}, data.Retry(data.NoRetry))
		var pe *pgconn.PgError
		switch {
		case errs.KindOf(err) != tc.kind || errs.From(err).Code != tc.code:
			t.Errorf("%s: got %v", tc.name, err)
		case !errors.As(err, &pe):
			t.Errorf("%s: the driver's error isn't the cause", tc.name)
		}
		if p := errs.ToProblem(err, ""); strings.Contains(fmt.Sprint(p), pe.Message) || strings.Contains(fmt.Sprint(p), "SQLSTATE") {
			t.Errorf("%s: the driver's message reaches the client: %+v", tc.name, p)
		}
	}

	canceled, cancel := context.WithCancel(c)
	cancel()
	if err := e.m.Write(canceled, func(context.Context, data.WriteTx) error { return nil }); errs.KindOf(err) != errs.Canceled {
		t.Errorf("a canceled context: %v", err)
	}
	if err := e.m.Read(c, func(ctx context.Context, r data.ReadTx) error {
		ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		var x string
		return r.QueryRow(ctx, "SELECT pg_sleep(5)::text").Scan(&x)
	}); errs.KindOf(err) != errs.DeadlineExceeded {
		t.Errorf("an expired context: %v", err)
	}

	// statement_timeout, with the caller's ctx still live.
	err := e.m.Read(c, func(ctx context.Context, r data.ReadTx) error {
		rows, err := r.Query(ctx, "SET LOCAL statement_timeout = '50ms'")
		if err != nil {
			return err
		}
		rows.Close()
		var x string
		return r.QueryRow(ctx, "SELECT pg_sleep(5)::text").Scan(&x)
	})
	wantCode(t, err, errs.DeadlineExceeded, data.CodeStatementTimeout)

	// A lock not granted within lock_timeout.
	holder, err := e.pool.Begin(c)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(c)
	if _, err := holder.Exec(c, "SELECT * FROM "+p+" WHERE id = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	err = e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		if _, err := w.Exec(ctx, "SET LOCAL lock_timeout = '50ms'"); err != nil {
			return err
		}
		_, err := w.Exec(ctx, "SELECT * FROM "+p+" WHERE id = 1 FOR UPDATE")
		return err
	})
	wantCode(t, err, errs.Conflict, data.CodeLockNotAvailable)
}

func unreachable(t *testing.T, f Factory) {
	testkit.DB(t) // skips without Postgres, like the rest
	dsn := "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2"
	app, err := galleon.New(append(f.Options(t, Setup{DSN: dsn}),
		galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode), galleon.DrainGrace(0), galleon.Logger(slog.New(slog.DiscardHandler)))...)
	if err != nil {
		t.Fatal(err)
	}
	err = app.Start(ctx(t))
	if err == nil {
		_ = app.Shutdown(context.Background())
		t.Fatal("started with an unreachable database")
	}
	if errs.KindOf(err) != errs.Unavailable || strings.Contains(err.Error(), "nobody@") {
		t.Fatalf("got %v", err)
	}
}

func spans(t *testing.T, f Factory) {
	e := newEnv(t, f, Setup{})
	c := ctx(t)
	var db string
	if err := e.pool.QueryRow(c, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	const secret = "an argument that must not be recorded"
	err := e.m.Write(c, func(ctx context.Context, w data.WriteTx) error {
		_, err := w.Exec(ctx, "INSERT INTO "+e.table+" (id, v) VALUES ($1, $2)", 1, secret)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	w, err := e.m.BeginWrite(c, data.Nested)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(c, "UPDATE "+e.table+" SET v = $1", secret); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(c); err != nil {
		t.Fatal(err)
	}

	txs := e.tp.Find(func(s *Span) bool { return s.Attr("galleon.data.source") != "" && s.Attr("db.query.text") == "" })
	if len(txs) != 2 {
		t.Fatalf("want a span per transaction, got %d among %v", len(txs), e.tp.Names())
	}
	for i, tx := range txs {
		want := map[string]string{"db.system.name": "postgresql", "db.namespace": db, "galleon.data.source": "primary",
			"galleon.data.propagation": []string{"join", "nested"}[i]}
		for k, v := range want {
			if got := tx.Attr(k); got != v {
				t.Errorf("transaction span %d: %s is %q, want %q", i, k, got, v)
			}
		}
		stmts := e.tp.Find(func(s *Span) bool { return s.Parent() == tx && s.Attr("db.query.text") != "" })
		if !slices.ContainsFunc(stmts, func(s *Span) bool {
			return s.Attr("db.system.name") == "postgresql" && s.Attr("db.namespace") == db &&
				(strings.HasPrefix(s.Attr("db.query.text"), "INSERT") || strings.HasPrefix(s.Attr("db.query.text"), "UPDATE"))
		}) {
			t.Errorf("transaction span %d has no statement span for its write", i)
		}
	}
	for _, s := range e.tp.Find(func(*Span) bool { return true }) {
		if s.Contains(secret) {
			t.Fatalf("span %s recorded a statement's argument", s.Name())
		}
	}
}

// Recorder is a trace.TracerProvider that keeps every span, for checking
// what an implementation records.
type Recorder struct {
	embedded.TracerProvider
	mu    sync.Mutex
	spans []*Span
}

func (p *Recorder) Tracer(string, ...trace.TracerOption) trace.Tracer { return recTracer{p: p} }

// Find returns the spans match accepts, in start order.
func (p *Recorder) Find(match func(*Span) bool) []*Span {
	p.mu.Lock()
	all := slices.Clone(p.spans)
	p.mu.Unlock()
	var out []*Span
	for _, s := range all {
		if match(s) {
			out = append(out, s)
		}
	}
	return out
}

// Names lists every span's name.
func (p *Recorder) Names() []string {
	var out []string
	for _, s := range p.Find(func(*Span) bool { return true }) {
		out = append(out, s.Name())
	}
	return out
}

type recTracer struct {
	embedded.Tracer
	p *Recorder
}

func (t recTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	cfg := trace.NewSpanStartConfig(opts...)
	s := &Span{name: name, attrs: map[attribute.Key]string{}}
	if parent, ok := trace.SpanFromContext(ctx).(*Span); ok && !cfg.NewRoot() {
		s.parent = parent
	}
	var id [8]byte
	t.p.mu.Lock()
	n := len(t.p.spans) + 1
	t.p.spans = append(t.p.spans, s)
	t.p.mu.Unlock()
	id[0], id[1] = byte(n>>8), byte(n)
	s.sc = trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID(id), TraceFlags: trace.FlagsSampled})
	s.SetAttributes(cfg.Attributes()...)
	return trace.ContextWithSpan(ctx, s), s
}

// Span is a recorded span.
type Span struct {
	noop.Span
	mu     sync.Mutex
	name   string
	sc     trace.SpanContext
	parent *Span
	attrs  map[attribute.Key]string
	events []string
}

func (s *Span) SpanContext() trace.SpanContext { return s.sc }
func (s *Span) IsRecording() bool              { return true }
func (s *Span) SetName(n string)               { s.mu.Lock(); s.name = n; s.mu.Unlock() }
func (s *Span) SetAttributes(kv ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range kv {
		s.attrs[a.Key] = a.Value.Emit()
	}
}
func (s *Span) AddEvent(name string, opts ...trace.EventOption) {
	cfg := trace.NewEventConfig(opts...)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, name)
	for _, a := range cfg.Attributes() {
		s.events = append(s.events, a.Value.Emit())
	}
}
func (s *Span) SetStatus(_ codes.Code, desc string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, desc)
}

// Name, Attr and Parent read what was recorded.
func (s *Span) Name() string { s.mu.Lock(); defer s.mu.Unlock(); return s.name }
func (s *Span) Attr(k string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attrs[attribute.Key(k)]
}
func (s *Span) Parent() *Span { return s.parent }

// Contains reports whether any attribute, event or status holds sub.
func (s *Span) Contains(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.attrs {
		if strings.Contains(v, sub) {
			return true
		}
	}
	return slices.ContainsFunc(s.events, func(v string) bool { return strings.Contains(v, sub) })
}
