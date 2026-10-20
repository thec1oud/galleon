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

func TestPackageDatabasesArePerProcess(t *testing.T) {
	admin := os.Getenv(PostgresEnv)
	if admin == "" {
		t.Skipf("%s is not set", PostgresEnv)
	}
	ctx := context.Background()
	schema := []string{fmt.Sprintf("-- %d\nCREATE TABLE t (id int)", time.Now().UnixNano())}
	_, tpl, base, err := dbNames(schema)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	exists := func(name string) bool {
		var ok bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	// Two runs that came before: one has ended, one is still connected.
	gone, running := base+"_1", base+"_2"
	for _, name := range []string{gone, running} {
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	dsn, err := withDatabase(admin, running)
	if err != nil {
		t.Fatal(err)
	}
	other, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	mine := fmt.Sprintf("%s_%d", base, os.Getpid())
	t.Cleanup(func() {
		other.Close(ctx)
		for _, name := range []string{gone, running, mine, tpl} {
			_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		}
	})

	got, err := packageDB(admin, schema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, mine) {
		t.Fatalf("the package database %s isn't named after this process (%s)", got, mine)
	}
	if exists(gone) {
		t.Error("an ended run's database was left behind")
	}
	if !exists(running) {
		t.Error("a running test's database was dropped")
	}
}
