package testkit

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A run's package database survives other runs while the run lasts, even
// with nothing connected to it, and is dropped by the first run after.
func TestPackageDatabasesLiveAsLongAsTheirRun(t *testing.T) {
	f := fresh(t)
	ctx := context.Background()
	// Two runs that came before, with nothing connected to their
	// databases: one has ended, one is between tests and holds its mark.
	ended, idle := f.base+"_1_aa", f.base+"_2_bb"
	for _, name := range []string{ended, idle} {
		if _, err := f.conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	other, err := pgx.Connect(ctx, adminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	if _, err := other.Exec(ctx, "SELECT pg_advisory_lock("+liveLock+")", idle); err != nil {
		t.Fatal(err)
	}

	first := f.mustRun(t)
	if !strings.HasPrefix(first, fmt.Sprintf("%s_%d_", f.base, os.Getpid())) {
		t.Errorf("the package database %s isn't named after this process", first)
	}
	if f.exists(t, ended) {
		t.Error("an ended run's database was left behind")
	}
	if !f.exists(t, idle) {
		t.Fatal("a running test's idle database was dropped")
	}

	// The idle run ends.
	other.Close(ctx)
	second := f.mustRun(t)
	if second == first {
		t.Error("two runs got the same name")
	}
	if f.exists(t, idle) {
		t.Error("the ended run's database was left behind")
	}
	if !f.exists(t, first) {
		t.Error("this run's own idle database was dropped")
	}
}

func TestWithDatabaseTellsTheFormsApart(t *testing.T) {
	for _, tc := range []struct{ dsn, want string }{
		{"postgres://u@h:5432/postgres?sslmode=disable", "postgres://u@h:5432/x?sslmode=disable"},
		{"host=h user=u application_name=http://y", "host=h user=u application_name=http://y dbname='x'"},
	} {
		got, err := withDatabase(tc.dsn, "x")
		if err != nil || got != tc.want {
			t.Errorf("withDatabase(%q) = %q, %v; want %q", tc.dsn, got, err, tc.want)
		}
	}
	for _, dsn := range []string{"postgress://u@h/postgres", "postgres://u@h:%zz/postgres"} {
		if got, err := withDatabase(dsn, "x"); err == nil {
			t.Errorf("withDatabase(%q) = %q, want an error", dsn, got)
		}
	}
}

// fixture is a schema of its own, with its names and a connection to the
// admin database. Everything named after it is dropped when the test ends.
type fixture struct {
	schema         []string
	key, tpl, base string
	conn           *pgx.Conn
}

func fresh(t *testing.T) *fixture {
	t.Helper()
	admin := adminDSN()
	if admin == "" {
		t.Skipf("%s is not set", PostgresEnv)
	}
	ctx := context.Background()
	f := &fixture{schema: []string{fmt.Sprintf("-- %d\nCREATE TABLE t (id int)", time.Now().UnixNano())}}
	var err error
	if f.key, f.tpl, f.base, err = dbNames(f.schema); err != nil {
		t.Fatal(err)
	}
	if f.conn, err = pgx.Connect(ctx, admin); err != nil {
		t.Fatal(err)
	}
	// Each check of the live marks' connection is made, not skipped.
	every := liveCheckEvery
	liveCheckEvery = 0
	t.Cleanup(func() {
		liveCheckEvery = every
		rows, _ := f.conn.Query(ctx, "SELECT datname FROM pg_database WHERE starts_with(datname, $1) OR datname = $2", f.base, f.tpl)
		names, _ := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, name := range names {
			_, _ = f.conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		}
		f.conn.Close(ctx)
	})
	return f
}

// again gets the package database as a later test of the same run would.
func (f *fixture) again(t *testing.T) (string, error) {
	dsn, err := packageDB(adminDSN(), f.schema)
	if err != nil {
		return "", err
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Database, nil
}

// run gets the package database as a new run of the package would.
func (f *fixture) run(t *testing.T) (string, error) {
	retire(f.key)
	return f.again(t)
}

func (f *fixture) mustRun(t *testing.T) string {
	t.Helper()
	name, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func (f *fixture) exists(t *testing.T, name string) bool {
	t.Helper()
	var ok bool
	if err := f.conn.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

var retired int

// retire makes this process forget it has a database for key, while
// keeping it marked, as if another run had made it.
func retire(key string) {
	packageDBs.mu.Lock()
	defer packageDBs.mu.Unlock()
	if d, ok := packageDBs.dbs[key]; ok {
		retired++
		packageDBs.dbs[fmt.Sprintf("%s/retired/%d", key, retired)] = d
		delete(packageDBs.dbs, key)
	}
}

// held reports whether a live mark is held on name.
func held(t *testing.T, conn *pgx.Conn, name string) bool {
	t.Helper()
	ctx := context.Background()
	var won bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock("+liveLock+")", name).Scan(&won); err != nil {
		t.Fatal(err)
	}
	if won {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock("+liveLock+")", name); err != nil {
			t.Fatal(err)
		}
	}
	return !won
}

// killLive ends the backend holding the live marks, and waits for the
// server to let go of name's mark.
func killLive(t *testing.T, conn *pgx.Conn, name string) {
	t.Helper()
	packageDBs.mu.Lock()
	pid := packageDBs.live.PgConn().PID()
	packageDBs.mu.Unlock()
	if _, err := conn.Exec(context.Background(), "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if !held(t, conn, name) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the marks outlived their connection")
}

// When the connection holding the marks dies, the run's next DB reopens it
// and marks the run's databases again, rather than failing or letting
// them go.
func TestLiveMarksSurviveTheirConnection(t *testing.T) {
	f := fresh(t)
	conn := f.conn
	first, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	killLive(t, conn, first)
	got, err := f.again(t)
	if err != nil {
		t.Fatalf("after the marks' connection died: %v", err)
	}
	if got != first {
		t.Errorf("the run moved from %s to %s", first, got)
	}
	if !held(t, conn, first) {
		t.Errorf("%s isn't marked live", first)
	}
	second, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{first, second} {
		if !held(t, conn, name) {
			t.Errorf("%s isn't marked live", name)
		}
	}
}

// If another run dropped the database while its mark was lost, the run's
// next DB makes a new one rather than handing out one that is gone.
func TestADatabaseDroppedWhileUnmarkedIsMadeAgain(t *testing.T) {
	f := fresh(t)
	conn, base := f.conn, f.base
	first, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	killLive(t, conn, first)
	// Another binary's run, between this run's tests.
	if err := dropStale(context.Background(), conn, base); err != nil {
		t.Fatal(err)
	}
	got, err := f.again(t)
	if err != nil {
		t.Fatal(err)
	}
	if got == first {
		t.Fatalf("handed out %s, which was dropped", first)
	}
	dsn, err := withDatabase(adminDSN(), got)
	if err != nil {
		t.Fatal(err)
	}
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("the new database: %v", err)
	}
	c.Close(context.Background())
	if !held(t, conn, got) {
		t.Errorf("%s isn't marked live", got)
	}
}

// A package database that can't be created leaves neither a mark nor a
// database behind.
func TestAFailedCloneLeavesNothingBehind(t *testing.T) {
	f := fresh(t)
	tpl, base, conn := f.tpl, f.base, f.conn
	if _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	admin := adminDSN()
	dsn, err := withDatabase(admin, tpl)
	if err != nil {
		t.Fatal(err)
	}
	// A template can't be copied while anyone is connected to it.
	user, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer user.Close(context.Background())
	if _, err := f.run(t); err == nil {
		t.Fatal("cloning a template in use worked")
	}
	packageDBs.mu.Lock()
	marked := len(packageDBs.dbs)
	pid := packageDBs.live.PgConn().PID()
	packageDBs.mu.Unlock()
	var locks int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM pg_locks WHERE pid = $1 AND locktype = 'advisory'", pid).Scan(&locks); err != nil {
		t.Fatal(err)
	}
	if locks != marked {
		t.Errorf("%d live marks are held for %d databases", locks, marked)
	}
	var n int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM pg_database WHERE starts_with(datname, $1)", base).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d package databases, want the first run's alone", n)
	}
}

// A name another run already took, marked or not, is left alone, and a
// new one is tried.
func TestACloneNamedLikeAnotherTriesAgain(t *testing.T) {
	f := fresh(t)
	base, conn := f.base, f.conn
	if _, err := f.run(t); err != nil { // builds the template
		t.Fatal(err)
	}
	ctx := context.Background()
	name := func(suffix string) string { return fmt.Sprintf("%s_%d_%s", base, os.Getpid(), suffix) }
	// One run holds a mark on its name; another creates a database just
	// after this run looked for stale ones.
	other, err := pgx.Connect(ctx, adminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	if _, err := other.Exec(ctx, "SELECT pg_advisory_lock("+liveLock+")", name("marked")); err != nil {
		t.Fatal(err)
	}
	suffixes := []string{"marked", "created", "free"}
	orig := randomSuffix
	randomSuffix = func() (string, error) {
		s := suffixes[0]
		suffixes = suffixes[1:]
		if s == "created" {
			if _, err := other.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name(s)}.Sanitize()); err != nil {
				return "", err
			}
		}
		return s, nil
	}
	defer func() { randomSuffix = orig }()
	got, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if got != name("free") {
		t.Errorf("got %s, want %s", got, name("free"))
	}
	if !f.exists(t, name("created")) {
		t.Error("the other run's database was dropped")
	}
	if held(t, conn, name("created")) {
		t.Error("the other run's name is still marked by this one")
	}
}

// Within liveCheckEvery of the last check, a package database handed out
// again doesn't check the live marks' connection.
func TestLiveMarksAreCheckedAtMostSoOften(t *testing.T) {
	f := fresh(t)
	first := f.mustRun(t)
	liveCheckEvery = time.Hour
	packageDBs.mu.Lock()
	before := packageDBs.live
	packageDBs.mu.Unlock()
	killLive(t, f.conn, first)
	if got, err := f.again(t); err != nil || got != first {
		t.Fatalf("got %s, %v; want %s", got, err, first)
	}
	packageDBs.mu.Lock()
	after := packageDBs.live
	packageDBs.mu.Unlock()
	if after != before {
		t.Error("the connection was checked and replaced within the interval")
	}
	// Once the interval is over, the next one checks it.
	liveCheckEvery = 0
	if _, err := f.again(t); err != nil {
		t.Fatal(err)
	}
	if !held(t, f.conn, first) {
		t.Errorf("%s isn't marked live", first)
	}
}

// With no live marks' connection at all, as after a failed reconnect, a
// recent check doesn't excuse a package database from making one.
func TestNoLiveMarksConnectionIsNeverTakenAsChecked(t *testing.T) {
	f := fresh(t)
	first := f.mustRun(t)
	liveCheckEvery = time.Hour
	killLive(t, f.conn, first)
	packageDBs.mu.Lock()
	packageDBs.live.Close(context.Background())
	packageDBs.live = nil
	packageDBs.checked = time.Now()
	packageDBs.mu.Unlock()
	if _, err := f.again(t); err != nil {
		t.Fatal(err)
	}
	if !held(t, f.conn, first) {
		t.Errorf("%s isn't marked live", first)
	}
}
