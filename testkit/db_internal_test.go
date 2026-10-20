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
	admin := os.Getenv(PostgresEnv)
	if admin == "" {
		t.Skipf("%s is not set", PostgresEnv)
	}
	ctx := context.Background()
	schema := []string{fmt.Sprintf("-- %d\nCREATE TABLE t (id int)", time.Now().UnixNano())}
	key, tpl, base, err := dbNames(schema)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	exists := func(name string) bool {
		t.Helper()
		var ok bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	// Two runs that came before, with nothing connected to their
	// databases: one has ended, one is between tests and holds its mark.
	ended, idle := base+"_1_aa", base+"_2_bb"
	for _, name := range []string{ended, idle} {
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	other, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	if _, err := other.Exec(ctx, "SELECT pg_advisory_lock("+liveLock+")", idle); err != nil {
		t.Fatal(err)
	}
	var mine []string
	t.Cleanup(func() {
		for _, name := range append([]string{ended, idle, tpl}, mine...) {
			_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		}
	})
	run := func() string {
		t.Helper()
		retire(key) // as a new run would
		dsn, err := packageDB(admin, schema)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		mine = append(mine, cfg.Database)
		return cfg.Database
	}

	first := run()
	if !strings.HasPrefix(first, fmt.Sprintf("%s_%d_", base, os.Getpid())) {
		t.Errorf("the package database %s isn't named after this process", first)
	}
	if exists(ended) {
		t.Error("an ended run's database was left behind")
	}
	if !exists(idle) {
		t.Fatal("a running test's idle database was dropped")
	}

	// The idle run ends.
	other.Close(ctx)
	second := run()
	if second == first {
		t.Error("two runs got the same name")
	}
	if exists(idle) {
		t.Error("the ended run's database was left behind")
	}
	if !exists(first) {
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

// fresh returns a schema of its own, its names, a connection to the admin
// database, and a function that gets the package database as a new run of
// the package would, recording what to drop at the end.
func fresh(t *testing.T) (key, tpl, base string, conn *pgx.Conn, run func() (string, error)) {
	key, tpl, base, conn, run, _ = freshAgain(t)
	return key, tpl, base, conn, run
}

// freshAgain is fresh, plus a function that gets the package database as a
// later test of the same run would.
func freshAgain(t *testing.T) (key, tpl, base string, conn *pgx.Conn, run, again func() (string, error)) {
	t.Helper()
	admin := os.Getenv(PostgresEnv)
	if admin == "" {
		t.Skipf("%s is not set", PostgresEnv)
	}
	ctx := context.Background()
	schema := []string{fmt.Sprintf("-- %d\nCREATE TABLE t (id int)", time.Now().UnixNano())}
	key, tpl, base, err := dbNames(schema)
	if err != nil {
		t.Fatal(err)
	}
	conn, err = pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		rows, _ := conn.Query(ctx, "SELECT datname FROM pg_database WHERE starts_with(datname, $1) OR datname = $2", base, tpl)
		names, _ := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, name := range names {
			_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		}
		conn.Close(ctx)
	})
	run = func() (string, error) {
		retire(key)
		dsn, err := packageDB(admin, schema)
		if err != nil {
			return "", err
		}
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Database, nil
	}
	again = func() (string, error) {
		dsn, err := packageDB(admin, schema)
		if err != nil {
			return "", err
		}
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Database, nil
	}
	return key, tpl, base, conn, run, again
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
	_, _, _, conn, run, again := freshAgain(t)
	first, err := run()
	if err != nil {
		t.Fatal(err)
	}
	killLive(t, conn, first)
	got, err := again()
	if err != nil {
		t.Fatalf("after the marks' connection died: %v", err)
	}
	if got != first {
		t.Errorf("the run moved from %s to %s", first, got)
	}
	if !held(t, conn, first) {
		t.Errorf("%s isn't marked live", first)
	}
	second, err := run()
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
	_, _, base, conn, run, again := freshAgain(t)
	first, err := run()
	if err != nil {
		t.Fatal(err)
	}
	killLive(t, conn, first)
	// Another binary's run, between this run's tests.
	if err := dropStale(context.Background(), conn, base); err != nil {
		t.Fatal(err)
	}
	got, err := again()
	if err != nil {
		t.Fatal(err)
	}
	if got == first {
		t.Fatalf("handed out %s, which was dropped", first)
	}
	dsn, err := withDatabase(os.Getenv(PostgresEnv), got)
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
	_, tpl, base, conn, run := fresh(t)
	if _, err := run(); err != nil {
		t.Fatal(err)
	}
	admin := os.Getenv(PostgresEnv)
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
	if _, err := run(); err == nil {
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
	_, _, base, conn, run := fresh(t)
	if _, err := run(); err != nil { // builds the template
		t.Fatal(err)
	}
	ctx := context.Background()
	name := func(suffix string) string { return fmt.Sprintf("%s_%d_%s", base, os.Getpid(), suffix) }
	exists := func(name string) bool {
		t.Helper()
		var ok bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	// One run holds a mark on its name; another creates a database just
	// after this run looked for stale ones.
	other, err := pgx.Connect(ctx, os.Getenv(PostgresEnv))
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
	got, err := run()
	if err != nil {
		t.Fatal(err)
	}
	if got != name("free") {
		t.Errorf("got %s, want %s", got, name("free"))
	}
	if !exists(name("created")) {
		t.Error("the other run's database was dropped")
	}
	if held(t, conn, name("created")) {
		t.Error("the other run's name is still marked by this one")
	}
}
