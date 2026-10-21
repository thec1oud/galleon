package migratetest_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/migrate"
	"github.com/thec1oud/galleon/pg"
	"github.com/thec1oud/galleon/pg/sqlmigrate"
	"github.com/thec1oud/galleon/testkit"
	"github.com/thec1oud/galleon/testkit/conformance/datatest"
	"github.com/thec1oud/galleon/testkit/conformance/migratetest"
)

// sqlBuilds are the suite's builds as SQL files.
func sqlBuilds(b migratetest.Build) fstest.MapFS {
	files := fstest.MapFS{
		"0001_create_a.sql": {Data: []byte("-- galleon:risk safe\nCREATE TABLE a (id int);\n")},
		"0002_create_b.sql": {Data: []byte("-- galleon:risk safe\nCREATE TABLE b (id int);\n")},
	}
	switch b {
	case migratetest.Next:
		files["0003_create_c.sql"] = &fstest.MapFile{Data: []byte("-- galleon:risk safe\nCREATE TABLE c (id int);\n")}
	case migratetest.Last:
		files["0003_create_c.sql"] = &fstest.MapFile{Data: []byte("-- galleon:risk safe\nCREATE TABLE c (id int);\n")}
		files["0004_create_d.sql"] = &fstest.MapFile{Data: []byte("-- galleon:risk safe\nCREATE TABLE d (id int);\n")}
	case migratetest.Risky:
		files["0003_drop_a.sql"] = &fstest.MapFile{Data: []byte("-- galleon:risk destructive\nDROP TABLE a;\n")}
	case migratetest.Edited:
		files["0001_create_a.sql"] = &fstest.MapFile{Data: []byte("-- galleon:risk safe\nCREATE TABLE a (id int, note text);\n")}
	}
	return files
}

// schemaPool returns a pool whose search path is a new, empty schema of
// the package's database, dropped when t ends.
func schemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testkit.DB(t)
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "m_" + hex.EncodeToString(b[:])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(db.DSN())
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 8
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("dropping %s: %v", schema, err)
		}
		pool.Close()
	})
	return pool
}

func newSQL(t *testing.T, name string, files fstest.MapFS, pool *pgxpool.Pool, opts ...sqlmigrate.Option) *sqlmigrate.Migrator {
	t.Helper()
	m, err := sqlmigrate.New(name, files, pool, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSQLMigrateConformsOnPostgres(t *testing.T) {
	migratetest.Run(t, migratetest.Factory{
		Open: func(t *testing.T) func(migratetest.Build) migrate.Migrator {
			pool := schemaPool(t)
			return func(b migratetest.Build) migrate.Migrator {
				return newSQL(t, migratetest.Name, sqlBuilds(b), pool)
			}
		},
		Ahead: true, Drift: true, Fencing: true,
	})
}

// held takes the migration lock, as the Runner does, for calling Apply
// directly.
func held(t *testing.T, locks *local.Backend, name string) migrate.ApplyOptions {
	t.Helper()
	l, err := locks.Locks().Acquire(context.Background(), migrate.LockKey(name), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release(context.Background()) })
	return migrate.ApplyOptions{Lock: l, MaxRisk: migrate.Manual, AllowUnknown: true}
}

func exists(t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", table).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestAFailingFileLeavesNothingOfItselfAndStopsTheRest(t *testing.T) {
	ctx := context.Background()
	pool := schemaPool(t)
	files := fstest.MapFS{
		"1_ok.sql":     {Data: []byte("CREATE TABLE ok (id int);")},
		"2_broken.sql": {Data: []byte("CREATE TABLE half (id int);\nSELECT no_such_function();")},
		"3_later.sql":  {Data: []byte("CREATE TABLE later (id int);")},
	}
	m := newSQL(t, "schema", files, pool)
	res, applyErr := m.Apply(ctx, held(t, local.New(), "schema"))
	if applyErr == nil {
		t.Fatal("a broken file applied")
	}
	if len(res.Applied) != 1 || res.Applied[0].ID != "1_ok" {
		t.Errorf("applied %v, want only 1_ok", res.Applied)
	}
	if !exists(t, pool, "ok") || exists(t, pool, "half") || exists(t, pool, "later") {
		t.Error("the broken file's transaction didn't roll back, or a later file ran")
	}
	st, err := m.Status(ctx)
	if err != nil || st.State != migrate.Pending || st.Current != "1" || !strings.Contains(st.Detail, "2_broken.sql") {
		t.Errorf("status %+v, %v", st, err)
	}
	if strings.Contains(applyErr.Error(), "SELECT") {
		t.Errorf("the error holds SQL text: %v", applyErr)
	}
}

func TestANoTransactionFileRunsOutsideATransaction(t *testing.T) {
	ctx := context.Background()
	pool := schemaPool(t)
	files := fstest.MapFS{
		"1_table.sql": {Data: []byte("-- galleon:risk safe\nCREATE TABLE t (id int);")},
		"2_index.sql": {Data: []byte("-- galleon:risk caution\n-- galleon:no-transaction\nCREATE INDEX CONCURRENTLY t_id ON t (id);")},
	}
	m := newSQL(t, "schema", files, pool)
	res, err := m.Apply(ctx, held(t, local.New(), "schema"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 2 || !exists(t, pool, "t_id") {
		t.Fatalf("applied %v", res.Applied)
	}
	pl, err := m.Plan(ctx)
	if err != nil || len(pl.Steps()) != 0 {
		t.Errorf("plan after applying: %v, %v", pl, err)
	}

	// Without the directive, the same statement fails in its transaction.
	files["3_index.sql"] = &fstest.MapFile{Data: []byte("CREATE INDEX CONCURRENTLY t_id2 ON t (id);")}
	if _, err := newSQL(t, "schema", files, pool).Apply(ctx, held(t, local.New(), "schema")); err == nil {
		t.Fatal("CREATE INDEX CONCURRENTLY ran in a transaction")
	}
}

func TestPlansReportDeclaredRisksAndUnknownOtherwise(t *testing.T) {
	ctx := context.Background()
	pool := schemaPool(t)
	files := fstest.MapFS{
		"1_a.sql": {Data: []byte("-- galleon:risk safe\nCREATE TABLE a (id int);")},
		"2_b.sql": {Data: []byte("-- a comment first\n-- galleon:risk destructive\nDROP TABLE a;")},
		"3_c.sql": {Data: []byte("CREATE TABLE c (id int);\n-- galleon:risk safe")},
	}
	pl, err := newSQL(t, "schema", files, pool).Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var risks []migrate.Risk
	for _, s := range pl.Steps() {
		risks = append(risks, s.Risk)
	}
	if want := []migrate.Risk{migrate.Safe, migrate.Destructive, migrate.Unknown}; !equalRisks(risks, want) {
		t.Errorf("risks %v, want %v: a directive after the SQL begins doesn't count", risks, want)
	}
	if pl.MaxRisk() != migrate.Unknown {
		t.Errorf("max risk %s", pl.MaxRisk())
	}
	// A plan is refused as a whole: with MaxRisk Caution not even 1_a runs.
	opts := held(t, local.New(), "schema")
	opts.MaxRisk = migrate.Caution
	if _, err := newSQL(t, "schema", files, pool).Apply(ctx, opts); !hasCode(err, migrate.CodeRiskRefused) {
		t.Fatalf("applying above the maximum risk: %v", err)
	}
	if exists(t, pool, "a") {
		t.Error("a refused plan applied its first file")
	}
}

func equalRisks(a, b []migrate.Risk) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasCode(err error, code string) bool {
	e := errs.From(err)
	return e != nil && e.Code == code
}

func TestTheFenceIsRecordedInTheDatabase(t *testing.T) {
	ctx := context.Background()
	pool := schemaPool(t)
	locks := local.New()
	opts := held(t, locks, "schema")
	m := newSQL(t, "schema", sqlBuilds(migratetest.Base), pool, sqlmigrate.Table("ops_migrations"))
	if _, err := m.Apply(ctx, opts); err != nil {
		t.Fatal(err)
	}
	var token int64
	if err := pool.QueryRow(ctx, "SELECT token FROM ops_migrations_fence WHERE migrator = 'schema'").Scan(&token); err != nil {
		t.Fatal(err)
	}
	if uint64(token) != opts.Lock.Token() {
		t.Errorf("the fence holds %d, not the lease's token %d", token, opts.Lock.Token())
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM ops_migrations WHERE migrator = 'schema' AND token = $1", token).Scan(&n); err != nil || n != 2 {
		t.Errorf("%d records carry the token: %v", n, err)
	}
	// Another migrator in the same table has its own versions and fence.
	other := newSQL(t, "other", fstest.MapFS{"1_x.sql": {Data: []byte("CREATE TABLE x (id int);")}}, pool, sqlmigrate.Table("ops_migrations"))
	if st, err := other.Status(ctx); err != nil || st.State != migrate.Pending {
		t.Fatalf("status of another migrator in the table: %+v, %v", st, err)
	}
	stale := migrate.ApplyOptions{Lock: pastLease, MaxRisk: migrate.Manual, AllowUnknown: true}
	if _, err := other.Apply(ctx, stale); err != nil {
		t.Fatalf("another migrator's fence isn't its own: %v", err)
	}
}

// fixedLease is a lease of a given epoch and token.
type fixedLease struct {
	epoch string
	token uint64
}

// pastLease is a lease with the lowest token.
var pastLease = fixedLease{epoch: "past", token: 1}

func (fixedLease) Key() string                   { return "" }
func (l fixedLease) Token() uint64               { return l.token }
func (l fixedLease) Epoch() string               { return l.epoch }
func (fixedLease) Renew(context.Context) error   { return nil }
func (fixedLease) Release(context.Context) error { return nil }
func (fixedLease) Lost() <-chan struct{}         { return nil }

func TestStatusNamesWhatDriftedAndWhatIsAhead(t *testing.T) {
	ctx := context.Background()
	pool := schemaPool(t)
	if _, err := newSQL(t, "schema", sqlBuilds(migratetest.Next), pool).Apply(ctx, held(t, local.New(), "schema")); err != nil {
		t.Fatal(err)
	}
	st, err := newSQL(t, "schema", sqlBuilds(migratetest.Base), pool).Status(ctx)
	if err != nil || st.State != migrate.Ahead || st.Current != "3" || st.Expected != "2" || !strings.Contains(st.Detail, "0003_create_c.sql") {
		t.Errorf("ahead: %+v, %v", st, err)
	}
	edited := sqlBuilds(migratetest.Next)
	edited["0002_create_b.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE b (id bigint);")}
	delete(edited, "0001_create_a.sql")
	m := newSQL(t, "schema", edited, pool)
	st, err = m.Status(ctx)
	if err != nil || st.State != migrate.Drifted || !strings.Contains(st.Detail, "0002_create_b.sql was edited") || !strings.Contains(st.Detail, "0001_create_a.sql is applied, but this build doesn't have it") {
		t.Errorf("drifted: %+v, %v", st, err)
	}
	d, err := m.Verify(ctx)
	if err != nil || len(d.Differences) != 2 {
		t.Errorf("verify: %v, %v", d.Differences, err)
	}
	if _, err := m.Apply(ctx, held(t, local.New(), "schema")); !hasCode(err, sqlmigrate.CodeNotPending) {
		t.Errorf("applying a drifted build: %v", err)
	}
}

func TestStatusOnAnEmptyDatabaseCreatesNothing(t *testing.T) {
	ctx := context.Background()
	pool := schemaPool(t)
	m := newSQL(t, "schema", sqlBuilds(migratetest.Base), pool)
	st, err := m.Status(ctx)
	if err != nil || st.State != migrate.Pending || st.Current != "none" || st.Expected != "2" {
		t.Fatalf("%+v, %v", st, err)
	}
	if _, err := m.Plan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	if exists(t, pool, "galleon_migrations") {
		t.Error("reading the status created the table")
	}
}

func TestACancelledApplyStopsAndRollsBack(t *testing.T) {
	pool := schemaPool(t)
	files := fstest.MapFS{"1_slow.sql": {Data: []byte("CREATE TABLE s (id int); SELECT pg_sleep(30);")}}
	m := newSQL(t, "schema", files, pool)
	opts := held(t, local.New(), "schema")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Apply(ctx, opts)
		done <- err
	}()
	// Cancel once the statement is running.
	deadline := time.Now().Add(10 * time.Second)
	for !sleeping(t, pool) {
		if time.Now().After(deadline) {
			t.Fatal("the apply never started its statement")
		}
		time.Sleep(time.Millisecond) // a poll interval, not synchronization
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled apply succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled apply didn't stop")
	}
	if exists(t, pool, "s") {
		t.Error("the cancelled file's table stayed")
	}
}

func sleeping(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT 1 FROM pg_stat_activity WHERE query LIKE '%pg_sleep(30)%' AND state = 'active' AND pid <> pg_backend_pid()")
	if err != nil {
		t.Fatal(err)
	}
	n, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatal(err)
	}
	return len(n) > 0
}

func TestTwoAppsMigrateOnePostgresDatabaseOnce(t *testing.T) {
	pool := schemaPool(t)
	schema := pool.Config().ConnConfig.RuntimeParams["search_path"]
	db := testkit.DB(t)
	dsn := db.DSN() + "&search_path=" + schema
	if !strings.Contains(db.DSN(), "?") {
		dsn = db.DSN() + "?search_path=" + schema
	}
	cfg, err := json.Marshal(map[string]any{"pg": map[string]any{"primary": map[string]any{"dsn": dsn, "max_conns": 4}}})
	if err != nil {
		t.Fatal(err)
	}
	backend := local.New()
	tp := &datatest.Recorder{}
	newApp := func() *galleon.App {
		mod := galleon.ModuleFunc("schema", func(r *galleon.Registry) error {
			galleon.Migrator(r, "schema", func(m *pg.Manager) (migrate.Migrator, error) {
				return sqlmigrate.New("schema", sqlBuilds(migratetest.Next), m.Pool(""))
			})
			return nil
		})
		app, err := galleon.New(galleon.Name("orders"), galleon.Coordination(backend), galleon.Deploy(galleon.SingleNode),
			galleon.DrainGrace(0), galleon.Environment(galleon.Development), galleon.TracerProvider(tp),
			galleon.Logger(slog.New(slog.DiscardHandler)), galleon.ConfigJSON(cfg), galleon.Modules(pg.Module(), mod))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
		return app
	}
	apps := []*galleon.App{newApp(), newApp()}
	errs := make([]error, len(apps))
	var wg sync.WaitGroup
	for i, app := range apps {
		wg.Go(func() { errs[i] = app.Start(context.Background()) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("app %d: %v", i, err)
		}
		if err := apps[i].Ready(context.Background()); err != nil {
			t.Fatalf("app %d isn't ready: %v", i, err)
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM galleon_migrations").Scan(&n); err != nil || n != 3 {
		t.Fatalf("%d versions recorded: %v", n, err)
	}
	if steps := tp.Find(func(s *datatest.Span) bool { return s.Name() == "migrate.step" }); len(steps) != 3 {
		t.Errorf("%d step spans, want 3: one node applied", len(steps))
	}
	for _, s := range tp.Find(func(s *datatest.Span) bool { return strings.HasPrefix(s.Name(), "migrate.") }) {
		if s.Contains("CREATE") {
			t.Errorf("span %s holds SQL text", s.Name())
		}
	}
}

func TestALeaseOfASupersededEpochIsRefusedWhateverItsToken(t *testing.T) {
	ctx := context.Background()
	pool := schemaPool(t)
	files := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (id int);")}}
	apply := func(epoch string, token uint64, version, table string) error {
		files[version] = &fstest.MapFile{Data: []byte("CREATE TABLE " + table + " (id int);")}
		_, err := newSQL(t, "schema", files, pool).Apply(ctx, migrate.ApplyOptions{Lock: fixedLease{epoch, token}, MaxRisk: migrate.Manual, AllowUnknown: true})
		return err
	}
	if err := apply("e1", 5, "0001_a.sql", "a"); err != nil {
		t.Fatalf("e1/5: %v", err)
	}
	if err := apply("e2", 1, "0002_b.sql", "b"); err != nil {
		t.Fatalf("e2/1 of a new epoch: %v", err)
	}
	err := apply("e1", 3, "0003_c.sql", "c")
	if !hasCode(err, sqlmigrate.CodeStaleEpoch) || !strings.Contains(err.Error(), `epoch "e1" is stale`) {
		t.Fatalf("e1/3 after e2 applied: got %v, want %s", err, sqlmigrate.CodeStaleEpoch)
	}
	if exists(t, pool, "c") {
		t.Fatal("the superseded epoch applied 0003")
	}
	if err := apply("e2", 2, "0003_c.sql", "c"); err != nil {
		t.Fatalf("e2/2: %v", err)
	}
	if !exists(t, pool, "c") {
		t.Error("e2/2 didn't apply 0003")
	}
	// Within an epoch, tokens still compare.
	err = apply("e2", 1, "0004_d.sql", "d")
	if !hasCode(err, sqlmigrate.CodeStaleToken) || exists(t, pool, "d") {
		t.Fatalf("e2/1 after e2/2: got %v, want %s", err, sqlmigrate.CodeStaleToken)
	}
}

func TestAFencingTableWithoutEpochsGainsThem(t *testing.T) {
	ctx := context.Background()
	pool := schemaPool(t)
	// The fencing table as a build before epochs made it.
	if _, err := pool.Exec(ctx, `CREATE TABLE galleon_migrations_fence (migrator text PRIMARY KEY, token bigint NOT NULL);
INSERT INTO galleon_migrations_fence VALUES ('schema', 9)`); err != nil {
		t.Fatal(err)
	}
	m := newSQL(t, "schema", sqlBuilds(migratetest.Base), pool)
	if _, err := m.Apply(ctx, migrate.ApplyOptions{Lock: fixedLease{"e1", 1}, MaxRisk: migrate.Manual, AllowUnknown: true}); err != nil {
		t.Fatalf("applying over the old fencing table: %v", err)
	}
	var (
		epoch      string
		token      int64
		superseded []string
	)
	if err := pool.QueryRow(ctx, "SELECT epoch, token, superseded FROM galleon_migrations_fence WHERE migrator = 'schema'").Scan(&epoch, &token, &superseded); err != nil {
		t.Fatal(err)
	}
	if epoch != "e1" || token != 1 || len(superseded) != 1 || superseded[0] != "" {
		t.Errorf("the fence holds %q/%d, superseding %q", epoch, token, superseded)
	}
}

// The fence outlives the in-process backend's token counter: a new
// process's backend starts at token 1 again, under a new epoch.
func TestARestartedProcessAppliesAfterAHigherTokenWasRecorded(t *testing.T) {
	ctx := context.Background()
	pool := schemaPool(t)
	files := fstest.MapFS{
		"0001_a.sql": {Data: []byte("-- galleon:risk safe\nCREATE TABLE a (id int);")},
		"0002_b.sql": {Data: []byte("-- galleon:risk safe\nCREATE TABLE b (id int); SELECT no_such_function();")},
	}
	b1 := local.New().Locks()
	apply := func(locks interface {
		Acquire(context.Context, string, time.Duration) (coord.Lease, error)
	}, files fstest.MapFS) error {
		l, err := locks.Acquire(ctx, migrate.LockKey("schema"), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Release(ctx)
		_, err = newSQL(t, "schema", files, pool).Apply(ctx, migrate.ApplyOptions{Lock: l, MaxRisk: migrate.Manual})
		return err
	}
	if err := apply(b1, files); err == nil {
		t.Fatal("the broken 0002 applied")
	}
	files["0002_b.sql"] = &fstest.MapFile{Data: []byte("-- galleon:risk safe\nCREATE TABLE b (id int);")}
	if err := apply(b1, files); err != nil {
		t.Fatalf("the retry under token 2: %v", err)
	}
	files["0003_c.sql"] = &fstest.MapFile{Data: []byte("-- galleon:risk safe\nCREATE TABLE c (id int);")}
	if err := apply(local.New().Locks(), files); err != nil {
		t.Fatalf("the next process, under token 1 of a new epoch: %v", err)
	}
	if !exists(t, pool, "c") {
		t.Error("0003 wasn't applied")
	}
}

func TestAGapBelowTheLatestAppliedVersionIsDriftNotPending(t *testing.T) {
	ctx := context.Background()
	pool := schemaPool(t)
	files := fstest.MapFS{
		"0001_a.sql": {Data: []byte("CREATE TABLE a (id int);")},
		"0003_c.sql": {Data: []byte("CREATE TABLE c (id int);")},
	}
	if _, err := newSQL(t, "schema", files, pool).Apply(ctx, held(t, local.New(), "schema")); err != nil {
		t.Fatal(err)
	}
	// A branch merged later brings 0002.
	files["0002_b.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE b (id int);")}
	m := newSQL(t, "schema", files, pool)
	st, err := m.Status(ctx)
	if err != nil || st.State != migrate.Drifted || !strings.Contains(st.Detail, "0002_b.sql is older than applied 0003_c") {
		t.Fatalf("%+v, %v", st, err)
	}
	if _, err := m.Apply(ctx, held(t, local.New(), "schema")); !hasCode(err, sqlmigrate.CodeNotPending) || exists(t, pool, "b") {
		t.Fatalf("the gap was applied: %v", err)
	}
	ooo := newSQL(t, "schema", files, pool, sqlmigrate.AllowOutOfOrder())
	if st, err := ooo.Status(ctx); err != nil || st.State != migrate.Pending {
		t.Fatalf("with AllowOutOfOrder: %+v, %v", st, err)
	}
	if _, err := ooo.Apply(ctx, held(t, local.New(), "schema")); err != nil || !exists(t, pool, "b") {
		t.Fatalf("with AllowOutOfOrder the gap isn't applied: %v", err)
	}
}

func TestADirectApplyWithNoMaxRiskRefusesAManualFileAndCreatesNothing(t *testing.T) {
	ctx := context.Background()
	pool := schemaPool(t)
	files := fstest.MapFS{
		"1_a.sql":      {Data: []byte("-- galleon:risk safe\nCREATE TABLE a (id int);")},
		"2_manual.sql": {Data: []byte("-- galleon:risk manual\nCREATE TABLE m (id int);")},
	}
	opts := held(t, local.New(), "schema")
	opts.MaxRisk = 0
	_, err := newSQL(t, "schema", files, pool).Apply(ctx, opts)
	if !hasCode(err, migrate.CodeRiskRefused) || !strings.Contains(err.Error(), "max safe") {
		t.Fatalf("%v", err)
	}
	if exists(t, pool, "a") || exists(t, pool, "galleon_migrations") {
		t.Error("a refused apply created something")
	}
	opts.Fingerprint = "not-the-plan"
	opts.MaxRisk = migrate.Manual
	if _, err := newSQL(t, "schema", files, pool).Apply(ctx, opts); !hasCode(err, migrate.CodeFingerprintMismatch) || exists(t, pool, "galleon_migrations") {
		t.Fatalf("a plan other than the approved one: %v", err)
	}
}

func TestANodeThatWaitsLongerThanTheLeaseTTLSeesTheFirstNodesWork(t *testing.T) {
	ctx := context.Background()
	pool := schemaPool(t)
	files := fstest.MapFS{"0001_slow.sql": {Data: []byte("-- galleon:risk safe\nSELECT pg_sleep(1.2); CREATE TABLE x (id int);")}}
	locks := local.New().Locks()
	// A TTL of 1s gives a safety deadline of 750ms: the second node waits
	// longer than that.
	p := migrate.Policy{MaxRisk: migrate.Manual, LockTTL: time.Second}
	apply := func() ([]migrate.ApplyReport, error) {
		r, err := migrate.NewRunner(migrate.Config{Locker: locks, Migrators: []migrate.Migrator{newSQL(t, "schema", files, pool)}, Policy: p})
		if err != nil {
			t.Fatal(err)
		}
		return r.Apply(ctx)
	}
	first := make(chan error, 1)
	go func() {
		_, err := apply()
		first <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE query LIKE '%pg_sleep(1.2)%' AND pid <> pg_backend_pid()").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first node never started its file")
		}
		time.Sleep(5 * time.Millisecond) // a poll interval, not synchronization
	}
	reps, err := apply()
	if err != nil || reps[0].Outcome != migrate.AlreadyUpToDate || !reps[0].Waited {
		t.Fatalf("the node that waited: %+v, %v", reps, err)
	}
	if err := <-first; err != nil {
		t.Fatalf("the first node: %v", err)
	}
}
