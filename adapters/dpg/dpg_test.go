package dpg_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thec1oud/dpg/pkg/dpg/engine"

	"github.com/thec1oud/galleon/adapters/dpg"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/migrate"
	"github.com/thec1oud/galleon/testkit"
	"github.com/thec1oud/galleon/testkit/conformance/migratetest"
)

// The builds are DPG projects that testdata/gen.sh writes.
var builds = map[migratetest.Build]string{
	migratetest.Base:   "base",
	migratetest.Next:   "next",
	migratetest.Risky:  "risky",
	migratetest.Edited: "edited",
	migratetest.Last:   "last",
}

func dir(build string) string { return filepath.Join("testdata", "builds", build) }

// database returns the DSN of a new, empty database, dropped when t ends:
// DPG compares a whole database with its project, so tests can't share
// one. It skips without testkit.PostgresEnv, and fails then in CI.
func database(t *testing.T) string {
	t.Helper()
	admin := strings.TrimSpace(os.Getenv(testkit.PostgresEnv))
	if admin == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not set in CI", testkit.PostgresEnv)
		}
		t.Skipf("%s is not set; skipping a test that needs Postgres", testkit.PostgresEnv)
	}
	ctx := context.Background()
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "galleon_dpg_" + hex.EncodeToString(b[:])
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Errorf("dropping %s: %v", name, err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func poolOf(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// unreachable is a pool of a server that isn't there, for what never
// connects; a pool connects only when it is used.
func unreachable(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return poolOf(t, "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
}

func newMigrator(t *testing.T, build string, pool *pgxpool.Pool) *dpg.Migrator {
	t.Helper()
	m, err := dpg.New(migratetest.Name, dir(build), pool)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestConformsOnPostgres(t *testing.T) {
	migratetest.Run(t, migratetest.Factory{
		Open: func(t *testing.T) func(migratetest.Build) migrate.Migrator {
			pool := poolOf(t, database(t))
			return func(b migratetest.Build) migrate.Migrator { return newMigrator(t, builds[b], pool) }
		},
		Ahead: true, Drift: true, Fencing: true,
	})
}

// lease is a lease of a given epoch and token.
type lease struct {
	epoch string
	token uint64
}

func (l lease) Key() string                 { return migrate.LockKey(migratetest.Name) }
func (l lease) Token() uint64               { return l.token }
func (l lease) Epoch() string               { return l.epoch }
func (lease) Renew(context.Context) error   { return nil }
func (lease) Release(context.Context) error { return nil }
func (lease) Lost() <-chan struct{}         { return nil }

func under(l lease) migrate.ApplyOptions {
	return migrate.ApplyOptions{Lock: l, MaxRisk: migrate.Manual, AllowUnknown: true}
}

func code(err error) string {
	if e := errs.From(err); e != nil {
		return e.Code
	}
	return ""
}

func exists(t *testing.T, pool *pgxpool.Pool, relation string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", relation).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestNewRefusesASourceItsLogDoesntRecord(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(dir("base"))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "c/app/schemas/public/tables/z.dpg"), []byte("TABLE z (id int PRIMARY KEY);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := dpg.New("schema", root, unreachable(t)); err == nil || !strings.Contains(err.Error(), "DPG-E056") {
		t.Fatalf("an unrecorded change: %v", err)
	}
	if _, err := dpg.New("schema", filepath.Join(t.TempDir(), "nothing"), unreachable(t)); err == nil {
		t.Fatal("a directory without a project was accepted")
	}
}

func TestNewNeedsAPool(t *testing.T) {
	if _, err := dpg.New("schema", dir("base"), nil); err == nil || !strings.Contains(err.Error(), "no pool") {
		t.Fatalf("%v", err)
	}
}

func TestANameMustBeValid(t *testing.T) {
	if _, err := dpg.New("Schema!", dir("base"), unreachable(t)); err == nil {
		t.Fatal("a bad name was accepted")
	}
}

func TestApplyRefusesToRunWithoutALease(t *testing.T) {
	// A pool of a server that isn't there: Apply refuses before it
	// connects.
	m := newMigrator(t, "base", unreachable(t))
	_, err := m.Apply(context.Background(), migrate.ApplyOptions{MaxRisk: migrate.Manual, AllowUnknown: true})
	if code(err) != dpg.CodeNoLock {
		t.Fatalf("%v", err)
	}
}

func TestTheOfflinePlanIsTheWholeLogWithoutADatabase(t *testing.T) {
	ctx := context.Background()
	pl, err := newMigrator(t, "last", unreachable(t)).PlanOffline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Steps()) != 4 || pl.MaxRisk() != migrate.Safe {
		t.Fatalf("%+v, max %s", pl.Steps(), pl.MaxRisk())
	}
	risky, err := newMigrator(t, "risky", unreachable(t)).PlanOffline(ctx)
	if err != nil || risky.MaxRisk() != migrate.Destructive {
		t.Fatalf("%v, %v", risky, err)
	}
	// The builds share their first migrations, as builds of one project do.
	if pl.Steps()[0].ID != risky.Steps()[0].ID || pl.Steps()[1].ID != risky.Steps()[1].ID {
		t.Errorf("last %v and risky %v don't share base's migrations", pl.Steps(), risky.Steps())
	}
}

func TestStatusPlanAndVerifyCreateNothing(t *testing.T) {
	ctx := context.Background()
	pool := poolOf(t, database(t))
	m := newMigrator(t, "base", pool)
	st, err := m.Status(ctx)
	if err != nil || st.State != migrate.Pending || st.Current != "none" {
		t.Fatalf("%+v, %v", st, err)
	}
	if _, err := m.Plan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	var schemas int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_namespace WHERE nspname = 'dpg'").Scan(&schemas); err != nil || schemas != 0 {
		t.Errorf("reading created DPG's schema: %d, %v", schemas, err)
	}
}

func TestAStaleLeaseIsRefusedWithTheAdaptersCodes(t *testing.T) {
	ctx := context.Background()
	pool := poolOf(t, database(t))
	if _, err := newMigrator(t, "base", pool).Apply(ctx, under(lease{"e1", 10})); err != nil {
		t.Fatal(err)
	}
	opts := under(lease{"e1", 5})
	rec := &recorder{}
	opts.Observer = rec
	_, err := newMigrator(t, "next", pool).Apply(ctx, opts)
	if code(err) != dpg.CodeStaleToken || !strings.Contains(err.Error(), "token 5 is stale") {
		t.Fatalf("a lower token: %v", err)
	}
	if len(rec.ended) != 1 || code(rec.ended[0]) != dpg.CodeStaleToken {
		t.Fatalf("observed %v", rec.ended)
	}
	// The fence refused DPG's first record, whose SQL isn't quoted, to
	// the caller or to the observer.
	for _, e := range []error{err, rec.ended[0]} {
		for _, sql := range []string{"$dpg$", `"dpg".`, "INSERT", "CREATE"} {
			if strings.Contains(e.Error(), sql) {
				t.Errorf("the stale lease's error quotes SQL (%s): %v", sql, e)
			}
		}
	}
	if exists(t, pool, "public.c") {
		t.Fatal("the stale holder created c")
	}
	if _, err := newMigrator(t, "next", pool).Apply(ctx, under(lease{"e2", 1})); err != nil {
		t.Fatalf("a new epoch: %v", err)
	}
	_, err = newMigrator(t, "last", pool).Apply(ctx, under(lease{"e1", 99}))
	if code(err) != dpg.CodeStaleEpoch || !strings.Contains(err.Error(), `epoch "e1" is stale`) {
		t.Fatalf("a superseded epoch: %v", err)
	}
	if exists(t, pool, "public.d") {
		t.Fatal("the superseded epoch created d")
	}
}

// The fence is in DPG's ledger schema, which dpg verify ignores, and moves
// with the migrations it fences.
func TestTheFenceIsKeptWhereDPGDoesntLook(t *testing.T) {
	ctx := context.Background()
	pool := poolOf(t, database(t))
	m := newMigrator(t, "base", pool)
	if _, err := m.Apply(ctx, under(lease{"e", 7})); err != nil {
		t.Fatal(err)
	}
	var (
		epoch string
		token int64
	)
	if err := pool.QueryRow(ctx, "SELECT epoch, token FROM dpg."+dpg.FenceTable+" WHERE migrator = $1", migratetest.Name).Scan(&epoch, &token); err != nil || epoch != "e" || token != 7 {
		t.Fatalf("the fence holds %q/%d: %v", epoch, token, err)
	}
	d, err := m.Verify(ctx)
	if err != nil || !d.None() {
		t.Fatalf("verify: %v, %v", d.Differences, err)
	}
}

// recorder records the steps an apply reports.
type recorder struct {
	started []migrate.StepInfo
	ended   []error
}

func (r *recorder) Step(ctx context.Context, s migrate.StepInfo) (context.Context, func(error)) {
	r.started = append(r.started, s)
	return ctx, func(err error) { r.ended = append(r.ended, err) }
}

func TestEachMigrationIsAnObservedStep(t *testing.T) {
	ctx := context.Background()
	pool := poolOf(t, database(t))
	m := newMigrator(t, "next", pool)
	pl, err := m.Plan(ctx)
	if err != nil || len(pl.Steps()) != 3 {
		t.Fatalf("%v, %v", pl, err)
	}
	opts := under(lease{"e", 1})
	rec := &recorder{}
	opts.Observer, opts.Fingerprint = rec, pl.Fingerprint()
	res, err := m.Apply(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Applied, pl.Steps()) || !slices.Equal(rec.started, pl.Steps()) {
		t.Fatalf("applied %+v, observed %+v, planned %+v", res.Applied, rec.started, pl.Steps())
	}
	if len(rec.ended) != 3 || slices.ContainsFunc(rec.ended, func(err error) bool { return err != nil }) {
		t.Errorf("outcomes %v", rec.ended)
	}
	for _, s := range pl.Steps() {
		if s.Risk != migrate.Safe || strings.Contains(s.Description, "CREATE") {
			t.Errorf("step %+v", s)
		}
	}
	st, err := m.Status(ctx)
	if err != nil || st.State != migrate.UpToDate || st.Current != pl.Steps()[2].ID || st.Expected != st.Current {
		t.Errorf("%+v, %v", st, err)
	}
}

// A statement DPG runs after a migration's transaction is fenced before it
// runs: here the transaction committed, then the statement after it
// failed, and a stale holder that tries to finish the migration is refused
// before it builds the index.
func TestAStatementAfterTheTransactionIsFencedBeforeItRuns(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	pool := poolOf(t, dsn)
	if _, err := newMigrator(t, "base", pool).Apply(ctx, under(lease{"e", 10})); err != nil {
		t.Fatal(err)
	}
	// Run the index's migration through DPG's engine, failing the first
	// statement after its transaction committed.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	e, err := engine.Open(engine.Config{Dir: dir("indexed"), Conn: conn})
	if err != nil {
		t.Fatal(err)
	}
	afterCommit := errors.New("failed after the commit")
	_, err = e.Apply(ctx, engine.ApplyOptions{MaxSafety: engine.Caution, Hook: func(ctx context.Context, tx pgx.Tx) error {
		var committed bool
		err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM dpg.migrations WHERE status = 'committed')").Scan(&committed)
		if err == nil && committed {
			return afterCommit
		}
		return err
	}})
	if !errors.Is(err, afterCommit) {
		t.Fatalf("the engine's apply: %v", err)
	}

	m := newMigrator(t, "indexed", pool)
	pl, err := m.Plan(ctx)
	if err != nil || len(pl.Steps()) != 1 || !strings.Contains(pl.Steps()[0].Description, "1 after its transaction") {
		t.Fatalf("the plan to finish: %+v, %v", pl, err)
	}
	_, err = m.Apply(ctx, under(lease{"e", 9}))
	if code(err) != dpg.CodeStaleToken {
		t.Fatalf("the stale holder: %v", err)
	}
	if exists(t, pool, "public.a_id_desc") {
		t.Fatal("the stale holder built the index")
	}
	if st, err := m.Status(ctx); err != nil || st.State != migrate.Pending {
		t.Fatalf("after the stale holder: %+v, %v", st, err)
	}
	res, err := m.Apply(ctx, under(lease{"e", 11}))
	if err != nil || len(res.Applied) != 1 {
		t.Fatalf("finishing: %+v, %v", res, err)
	}
	if !exists(t, pool, "public.a_id_desc") {
		t.Error("finishing didn't build the index")
	}
	if st, err := m.Status(ctx); err != nil || st.State != migrate.UpToDate {
		t.Errorf("after finishing: %+v, %v", st, err)
	}
}

func TestVerifyFindsAChangedObject(t *testing.T) {
	ctx := context.Background()
	pool := poolOf(t, database(t))
	m := newMigrator(t, "base", pool)
	if _, err := m.Apply(ctx, under(lease{"e", 1})); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "ALTER TABLE a ALTER id TYPE bigint"); err != nil {
		t.Fatal(err)
	}
	d, err := m.Verify(ctx)
	if err != nil || len(d.Differences) == 0 || !strings.Contains(strings.Join(d.Differences, "\n"), "public.a") {
		t.Fatalf("%v, %v", d.Differences, err)
	}
	// The ledger still says the database is up to date: Status reads
	// only where it is in the log.
	if st, err := m.Status(ctx); err != nil || st.State != migrate.UpToDate {
		t.Errorf("%+v, %v", st, err)
	}
}

func TestACancelledApplyStops(t *testing.T) {
	pool := poolOf(t, database(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newMigrator(t, "base", pool).Apply(ctx, under(lease{"e", 1})); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if exists(t, pool, "public.a") {
		t.Error("a cancelled apply created a")
	}
}

// tracer records the statements traced on a connection.
type tracer struct {
	mu  sync.Mutex
	sql []string
}

func (r *tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	r.sql = append(r.sql, d.SQL)
	r.mu.Unlock()
	return ctx
}

func (r *tracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// The adapter's connections run the pool's BeforeConnect hook, which may
// rotate credentials or move the server, but not its AfterConnect hook,
// which prepares a connection for the app, nor its tracer, which may
// record statements' text, and DPG's statements can hold secrets.
func TestConnectionsRunBeforeConnectButNotAfterConnectOrTheTracer(t *testing.T) {
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(database(t))
	if err != nil {
		t.Fatal(err)
	}
	// The configured port is wrong, on every host pgx would try (sslmode
	// prefer tries each twice); BeforeConnect puts the right one in, as a
	// hook that looks the server up would.
	port := cfg.ConnConfig.Port
	setPort := func(cc *pgx.ConnConfig, p uint16) {
		cc.Port = p
		for _, f := range cc.Fallbacks {
			f.Port = p
		}
	}
	setPort(cfg.ConnConfig, 1)
	rec := &tracer{}
	cfg.ConnConfig.Tracer = rec
	var before, after atomic.Int32
	cfg.BeforeConnect = func(_ context.Context, cc *pgx.ConnConfig) error {
		before.Add(1)
		if cc.Tracer != nil {
			t.Error("BeforeConnect was given a configuration with the tracer")
		}
		setPort(cc, port)
		return nil
	}
	cfg.AfterConnect = func(context.Context, *pgx.Conn) error {
		after.Add(1)
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	m := newMigrator(t, "base", pool)
	if _, err := m.Apply(ctx, under(lease{"e", 1})); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if before.Load() < 2 || after.Load() != 0 {
		t.Errorf("BeforeConnect ran %d times, AfterConnect %d", before.Load(), after.Load())
	}
	rec.mu.Lock()
	if len(rec.sql) != 0 {
		t.Errorf("the pool's tracer saw %q", rec.sql)
	}
	rec.mu.Unlock()
	// A hook that fails stops the call.
	cfg.BeforeConnect = func(context.Context, *pgx.ConnConfig) error { return errors.New("no credentials") }
	failing, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(failing.Close)
	if _, err := newMigrator(t, "base", failing).Status(ctx); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("status with a failing BeforeConnect: %v", err)
	}
}

// What DPG warns of, here a snapshot of another format version, is in the
// status's detail.
func TestDPGsWarningsAreInTheStatus(t *testing.T) {
	// A project whose path has a space, opened from its database's
	// directory: the warning's path is relative to its root.
	root := filepath.Join(t.TempDir(), "my project")
	if err := os.CopyFS(root, os.DirFS(dir("base"))); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, ".dpg", "snapshots", "c", "app.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), `"dpg_version": "0.2.0"`, `"dpg_version": "9.0.0"`, 1))
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := dpg.New("schema", filepath.Join(root, "c", "app"), poolOf(t, database(t)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := m.Status(ctx)
	if err != nil || st.State != migrate.Pending || !strings.Contains(st.Detail, "warning: ") || !strings.Contains(st.Detail, "format 9.0.0") {
		t.Fatalf("%+v, %v", st, err)
	}
	// Its path is the project's own, not where the app runs from.
	rel := filepath.Join(".dpg", "snapshots", "c", "app.json")
	if !strings.Contains(st.Detail, "warning: "+rel+": ") || strings.Contains(st.Detail, "my project") {
		t.Errorf("the warning's path: %s", st.Detail)
	}
	// In a result too: of a refusal, of an apply, of one with nothing to do.
	refused := under(lease{"e", 1})
	refused.Fingerprint = "sha256:none"
	for i, opts := range []migrate.ApplyOptions{refused, under(lease{"e", 1}), under(lease{"e", 2})} {
		res, err := m.Apply(ctx, opts)
		if (err != nil) != (i == 0) || !strings.Contains(res.Detail, "warning: "+rel+": ") || strings.Contains(res.Detail, "my project") {
			t.Errorf("apply %d: %+v, %v", i, res, err)
		}
	}
}

// An apply cancelled while CREATE INDEX CONCURRENTLY waits (a lease lost,
// a shutdown) leaves the index behind invalid; the next apply, on any
// node, rebuilds it, and the error quotes no SQL.
func TestAnIndexBuildCancelledPartWayIsRebuiltByTheNextApply(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	pool := poolOf(t, dsn)
	if _, err := newMigrator(t, "base", pool).Apply(ctx, under(lease{"e", 1})); err != nil {
		t.Fatal(err)
	}
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, "LOCK TABLE a IN ROW EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	m := newMigrator(t, "indexed", pool)
	go func() {
		_, err := m.Apply(actx, under(lease{"e", 2}))
		done <- err
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND query LIKE 'CREATE INDEX CONCURRENTLY%' AND wait_event_type = 'Lock'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the index build never waited for the lock")
		}
		time.Sleep(5 * time.Millisecond) // a poll interval, not synchronization
	}
	cancel()
	err = <-done
	if err == nil || strings.Contains(err.Error(), "CREATE") || strings.Contains(err.Error(), "--resume") {
		t.Fatalf("the cancelled apply: %v", err)
	}
	// The cancel only stops the client waiting: pgx closes the connection
	// rather than send the server a cancel request, so the build may go on
	// (as the package's docs say). Stop it on the server too, as a
	// statement timeout or a person would, to leave it cancelled part way.
	const builds = "FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND query LIKE 'CREATE INDEX CONCURRENTLY%' AND state = 'active'"
	if _, err := pool.Exec(ctx, "SELECT pg_cancel_backend(pid) "+builds); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) "+builds).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the index build didn't stop")
		}
		time.Sleep(5 * time.Millisecond) // a poll interval, not synchronization
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := pool.QueryRow(ctx, "SELECT indisvalid FROM pg_index WHERE indexrelid = 'public.a_id_desc'::regclass").Scan(&valid); err != nil || valid {
		t.Fatalf("after the cancel, the index is valid=%v: %v", valid, err)
	}
	if st, err := m.Status(ctx); err != nil || st.State != migrate.Pending {
		t.Fatalf("status: %+v, %v", st, err)
	}
	if _, err := newMigrator(t, "indexed", pool).Apply(ctx, under(lease{"e", 3})); err != nil {
		t.Fatalf("the next apply: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT indisvalid FROM pg_index WHERE indexrelid = 'public.a_id_desc'::regclass").Scan(&valid); err != nil || !valid {
		t.Fatalf("after the next apply, the index is valid=%v: %v", valid, err)
	}
	if st, err := m.Status(ctx); err != nil || st.State != migrate.UpToDate {
		t.Errorf("status: %+v, %v", st, err)
	}
}

// The adapter needs no superuser: the database's owner, with no other
// privilege, applies, plans and verifies.
func TestTheDatabasesOwnerIsEnough(t *testing.T) {
	ctx := context.Background()
	role := newRole(t) // before the database, so it is dropped after it
	dsn := database(t)
	admin := poolOf(t, dsn)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+strings.TrimPrefix(u.Path, "/")+" OWNER TO "+role+"; ALTER SCHEMA public OWNER TO "+role); err != nil {
		t.Fatal(err)
	}
	u.User = url.User(role)
	m := newMigrator(t, "next", poolOf(t, u.String()))
	if _, err := m.Apply(ctx, under(lease{"e", 1})); err != nil {
		t.Fatalf("apply as the owner: %v", err)
	}
	if st, err := m.Status(ctx); err != nil || st.State != migrate.UpToDate {
		t.Fatalf("%+v, %v", st, err)
	}
	if d, err := m.Verify(ctx); err != nil || !d.None() {
		t.Fatalf("verify as the owner: %v, %v", d.Differences, err)
	}
}

// newRole creates a login role with no privileges, dropped when t ends.
func newRole(t *testing.T) string {
	t.Helper()
	admin := strings.TrimSpace(os.Getenv(testkit.PostgresEnv))
	if admin == "" {
		database(t) // skips, or fails in CI
	}
	ctx := context.Background()
	var b [6]byte
	_, _ = rand.Read(b[:])
	role := "galleon_dpg_owner_" + hex.EncodeToString(b[:])
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			t.Errorf("dropping %s: %v", role, err)
			return
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(context.Background(), "DROP ROLE IF EXISTS "+role); err != nil {
			t.Errorf("dropping %s: %v", role, err)
		}
	})
	return role
}

// A migration whose index wasn't built yet is finished, then a later
// migration on the same table runs: the index still to be built isn't
// drift on the table.
func TestFinishingAMigrationThenTheNextOnTheSameTable(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	pool := poolOf(t, dsn)
	committedOnly(t, dsn, pool)
	m := newMigrator(t, "noted", pool)
	if st, err := m.Status(ctx); err != nil || st.State != migrate.Pending {
		t.Fatalf("%+v, %v", st, err)
	}
	res, err := m.Apply(ctx, under(lease{"e", 2}))
	if err != nil || len(res.Applied) != 2 {
		t.Fatalf("finishing, then the next migration: %+v, %v", res, err)
	}
	if st, err := m.Status(ctx); err != nil || st.State != migrate.UpToDate {
		t.Errorf("%+v, %v", st, err)
	}
	if !exists(t, pool, "public.a_id_desc") {
		t.Error("the index wasn't built")
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_name = 'a' AND column_name = 'note'").Scan(&n); err != nil || n != 1 {
		t.Errorf("column note: %d, %v", n, err)
	}
}

// committedOnly applies the base build, then the indexed build's migration
// up to its transaction's commit: its index is still to be built.
func committedOnly(t *testing.T, dsn string, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := newMigrator(t, "base", pool).Apply(ctx, under(lease{"e", 1})); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	e, err := engine.Open(engine.Config{Dir: dir("indexed"), Conn: conn})
	if err != nil {
		t.Fatal(err)
	}
	afterCommit := errors.New("failed after the commit")
	_, err = e.Apply(ctx, engine.ApplyOptions{MaxSafety: engine.Caution, Hook: func(ctx context.Context, tx pgx.Tx) error {
		var committed bool
		err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM dpg.migrations WHERE status = 'committed')").Scan(&committed)
		if err == nil && committed {
			return afterCommit
		}
		return err
	}})
	if !errors.Is(err, afterCommit) {
		t.Fatalf("the engine's apply: %v", err)
	}
}

// A valid index of the name a migration being finished builds, but made by
// hand with another definition, is neither taken as built nor dropped.
func TestAnIndexDefinedOtherwiseIsntTakenAsBuilt(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	pool := poolOf(t, dsn)
	committedOnly(t, dsn, pool)
	if _, err := pool.Exec(ctx, "CREATE INDEX a_id_desc ON a (id)"); err != nil {
		t.Fatal(err)
	}
	m := newMigrator(t, "noted", pool)
	_, err := m.Apply(ctx, under(lease{"e", 2}))
	if err == nil || !strings.Contains(err.Error(), "DPG-E058: ") || strings.Contains(err.Error(), "CREATE") {
		t.Fatalf("finishing: %v", err)
	}
	var def string
	if err := pool.QueryRow(ctx, "SELECT pg_get_indexdef('public.a_id_desc'::regclass)").Scan(&def); err != nil || !strings.HasSuffix(def, "(id)") {
		t.Errorf("a_id_desc is %s, %v", def, err)
	}
	if st, err := m.Status(ctx); err != nil || st.State != migrate.Pending {
		t.Errorf("%+v, %v", st, err)
	}
}
