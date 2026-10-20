package testkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/pg"
)

// PostgresEnv names the environment variable holding the DSN of a Postgres
// server that tests may create databases on, such as
// postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable. When
// it is unset, tests that need Postgres skip; in CI (where CI is set) they
// fail instead, so a misconfigured pipeline can't pass by skipping them.
const PostgresEnv = "GALLEON_TEST_POSTGRES"

// DBOption configures DB.
type DBOption func(*dbSettings)

type dbSettings struct {
	schema []string
}

// Schema is the SQL the template database is built with, such as the
// output of a migration tool. Each statement string may hold several
// statements.
func Schema(sql ...string) DBOption {
	return func(s *dbSettings) { s.schema = append(s.schema, sql...) }
}

// Database is a test's view of a Postgres database: its own transaction in
// the package's database.
type Database struct {
	dsn     string
	manager *pg.Manager
}

// DB returns a database for the test, or skips it when PostgresEnv is unset.
//
// The schema is applied once, to a template database named after it, which
// is reused across runs. Each test binary (one per package) clones the
// template into a database of its own, once, so packages tested in
// parallel don't share data. Each test then runs in a transaction of that
// database, which is rolled back when the test ends: Manager runs every
// transaction inside it, so one test never sees another's writes.
//
// The package's database is left in place when the binary exits, and
// replaced the next time the package is tested.
func DB(t testing.TB, opts ...DBOption) *Database {
	t.Helper()
	admin := os.Getenv(PostgresEnv)
	if admin == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s must be set in CI, so the tests that need Postgres run", PostgresEnv)
		}
		t.Skipf("%s is not set; skipping a test that needs Postgres", PostgresEnv)
	}
	var s dbSettings
	for _, o := range opts {
		o(&s)
	}
	dsn, err := packageDB(admin, s.schema)
	if err != nil {
		t.Fatalf("testkit.DB: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("testkit.DB: connecting: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Close(ctx)
		t.Fatalf("testkit.DB: beginning the test's transaction: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := tx.Rollback(ctx); err != nil {
			t.Errorf("testkit.DB: rolling back the test's transaction: %v", err)
		}
		conn.Close(ctx)
	})
	return &Database{dsn: dsn, manager: pg.Within(tx)}
}

// Manager runs every transaction inside the test's transaction; see
// pg.Within for what that changes.
func (d *Database) Manager() *pg.Manager { return d.manager }

// Module is the pg module, providing Manager, for an app built with New.
func (d *Database) Module() galleon.Module { return pg.Module(pg.Use(d.manager)) }

// DSN is the package's database itself, outside the test's transaction.
// What is committed through it stays for the rest of the package's tests,
// so a test that uses it cleans up after itself.
func (d *Database) DSN() string { return d.dsn }

var packageDBs struct {
	mu   sync.Mutex
	dsns map[string]string // by schema hash
}

// packageDB returns the DSN of this binary's database for schema, creating
// the template and the clone the first time.
func packageDB(admin string, schema []string) (string, error) {
	h := sha256.Sum256([]byte(strings.Join(schema, "\x00")))
	key := hex.EncodeToString(h[:6])
	packageDBs.mu.Lock()
	defer packageDBs.mu.Unlock()
	if dsn, ok := packageDBs.dsns[key]; ok {
		return dsn, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	w := sha256.Sum256([]byte(wd))
	tpl := "galleon_tpl_" + key
	name := "galleon_pkg_" + hex.EncodeToString(w[:6]) + "_" + key

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return "", fmt.Errorf("connecting to %s: %w", PostgresEnv, err)
	}
	defer conn.Close(ctx)
	// Packages tested in parallel take turns creating databases: a
	// template can't be copied while anyone is connected to it.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext('galleon.testkit'))"); err != nil {
		return "", err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(hashtext('galleon.testkit'))")

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", tpl).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		if err := createTemplate(ctx, conn, admin, tpl, schema); err != nil {
			return "", err
		}
	}
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		return "", err
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident+" TEMPLATE "+pgx.Identifier{tpl}.Sanitize()); err != nil {
		return "", fmt.Errorf("cloning the template: %w", err)
	}
	dsn, err := withDatabase(admin, name)
	if err != nil {
		return "", err
	}
	if packageDBs.dsns == nil {
		packageDBs.dsns = map[string]string{}
	}
	packageDBs.dsns[key] = dsn
	return dsn, nil
}

func createTemplate(ctx context.Context, conn *pgx.Conn, admin, tpl string, schema []string) error {
	ident := pgx.Identifier{tpl}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		return fmt.Errorf("creating the template: %w", err)
	}
	apply := func() error {
		dsn, err := withDatabase(admin, tpl)
		if err != nil {
			return err
		}
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return err
		}
		defer c.Close(ctx)
		for _, sql := range schema {
			if _, err := c.Exec(ctx, sql); err != nil {
				return fmt.Errorf("applying the schema: %w", err)
			}
		}
		return nil
	}
	if err := apply(); err != nil {
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)")
		return err
	}
	return nil
}

// withDatabase returns dsn with its database replaced, in either DSN form.
func withDatabase(dsn, db string) (string, error) {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + db
		u.RawPath = ""
		return u.String(), nil
	}
	if strings.Contains(dsn, "://") {
		return "", fmt.Errorf("%s is not a postgres:// URL or a keyword/value string", PostgresEnv)
	}
	// In the keyword/value form, a later keyword wins.
	return dsn + " dbname='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(db) + "'", nil
}
