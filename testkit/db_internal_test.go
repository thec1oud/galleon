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
