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
	if _, err := dpg.New("schema", root, nil); err == nil || !strings.Contains(err.Error(), "DPG-E056") {
		t.Fatalf("an unrecorded change: %v", err)
	}
	if _, err := dpg.New("schema", filepath.Join(t.TempDir(), "nothing"), nil); err == nil {
		t.Fatal("a directory without a project was accepted")
	}
}

func TestANameMustBeValid(t *testing.T) {
	if _, err := dpg.New("Schema!", dir("base"), nil); err == nil {
		t.Fatal("a bad name was accepted")
	}
}

func TestApplyRefusesToRunWithoutALease(t *testing.T) {
	// No pool: Apply refuses before it connects.
	m := newMigrator(t, "base", nil)
	_, err := m.Apply(context.Background(), migrate.ApplyOptions{MaxRisk: migrate.Manual, AllowUnknown: true})
	if code(err) != dpg.CodeNoLock {
		t.Fatalf("%v", err)
	}
}

func TestTheOfflinePlanIsTheWholeLogWithoutADatabase(t *testing.T) {
	ctx := context.Background()
	pl, err := newMigrator(t, "last", nil).PlanOffline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Steps()) != 4 || pl.MaxRisk() != migrate.Safe {
		t.Fatalf("%+v, max %s", pl.Steps(), pl.MaxRisk())
	}
	risky, err := newMigrator(t, "risky", nil).PlanOffline(ctx)
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
	_, err := newMigrator(t, "next", pool).Apply(ctx, under(lease{"e1", 5}))
	if code(err) != dpg.CodeStaleToken || !strings.Contains(err.Error(), "token 5 is stale") {
		t.Fatalf("a lower token: %v", err)
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
