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
		packageDBs.mu.Lock()
		delete(packageDBs.dsns, key) // as a new run would
		packageDBs.mu.Unlock()
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
		packageDBs.mu.Lock()
		delete(packageDBs.dsns, key)
		packageDBs.mu.Unlock()
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
	return key, tpl, base, conn, run
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

// When the connection holding the marks dies, the next package database
// reopens it and marks this run's databases again, rather than failing or
// letting them go.
func TestLiveMarksSurviveTheirConnection(t *testing.T) {
	_, _, _, conn, run := fresh(t)
	first, err := run()
	if err != nil {
		t.Fatal(err)
	}
	packageDBs.mu.Lock()
	pid := packageDBs.live.PgConn().PID()
	packageDBs.mu.Unlock()
	if _, err := conn.Exec(context.Background(), "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatal(err)
	}
	// The server ends the backend a moment later.
	for range 100 {
		if !held(t, conn, first) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	second, err := run()
	if err != nil {
		t.Fatalf("after the marks' connection died: %v", err)
	}
	for _, name := range []string{first, second} {
		if !held(t, conn, name) {
			t.Errorf("%s isn't marked live", name)
		}
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
	marked := len(packageDBs.marked)
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
