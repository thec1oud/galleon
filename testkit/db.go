package testkit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/internal/dsn"
	"github.com/thec1oud/galleon/pg"
)

// PostgresEnv names the environment variable holding the DSN of a Postgres
// server that tests may create databases on, such as
// postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable. When
// it is unset, tests that need Postgres skip; in CI (where CI is set) they
// fail instead, so a misconfigured pipeline can't pass by skipping them.
//
// Runs mark the databases they use with advisory locks, which Postgres keeps
// per database, so runs that share a server must name the same database in
// it, or they may drop each other's.
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
// Each run of a package gets a database of its own, named after its process
// and a random suffix, and marks it live with an advisory lock held as long
// as the process runs. Nothing runs when a test binary exits, so the
// database is left in place then, and a later run of the package drops it
// once its mark is gone. Templates of schemas no longer
// in use stay, since nothing ties them to a package: drop the databases
// named galleon_tpl_* to reclaim them; the next run rebuilds what it needs.
func DB(t testing.TB, opts ...DBOption) *Database {
	t.Helper()
	admin := adminDSN()
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
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("testkit.DB: %v", err)
	}
	// The Manager is built on the connection, so its statement tracer is
	// plugged in once both exist.
	fwd := &forwardTracer{}
	cc.Tracer = fwd
	conn, err := pgx.ConnectConfig(ctx, cc)
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
	m := pg.Within(tx)
	fwd.set(m.QueryTracer())
	return &Database{dsn: dsn, manager: m}
}

// forwardTracer passes pgx's tracing calls to a tracer set after the
// connection was opened.
type forwardTracer struct {
	t atomic.Pointer[pgx.QueryTracer]
}

func (f *forwardTracer) set(t pgx.QueryTracer) { f.t.Store(&t) }

func (f *forwardTracer) get() pgx.QueryTracer {
	if t := f.t.Load(); t != nil {
		return *t
	}
	return nil
}

func (f *forwardTracer) TraceQueryStart(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if t := f.get(); t != nil {
		return t.TraceQueryStart(ctx, c, d)
	}
	return ctx
}

func (f *forwardTracer) TraceQueryEnd(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryEndData) {
	if t := f.get(); t != nil {
		t.TraceQueryEnd(ctx, c, d)
	}
}

func (f *forwardTracer) TraceBatchStart(ctx context.Context, c *pgx.Conn, d pgx.TraceBatchStartData) context.Context {
	if t, ok := f.get().(pgx.BatchTracer); ok {
		return t.TraceBatchStart(ctx, c, d)
	}
	return ctx
}

func (f *forwardTracer) TraceBatchQuery(ctx context.Context, c *pgx.Conn, d pgx.TraceBatchQueryData) {
	if t, ok := f.get().(pgx.BatchTracer); ok {
		t.TraceBatchQuery(ctx, c, d)
	}
}

func (f *forwardTracer) TraceBatchEnd(ctx context.Context, c *pgx.Conn, d pgx.TraceBatchEndData) {
	if t, ok := f.get().(pgx.BatchTracer); ok {
		t.TraceBatchEnd(ctx, c, d)
	}
}

func (f *forwardTracer) TraceCopyFromStart(ctx context.Context, c *pgx.Conn, d pgx.TraceCopyFromStartData) context.Context {
	if t, ok := f.get().(pgx.CopyFromTracer); ok {
		return t.TraceCopyFromStart(ctx, c, d)
	}
	return ctx
}

func (f *forwardTracer) TraceCopyFromEnd(ctx context.Context, c *pgx.Conn, d pgx.TraceCopyFromEndData) {
	if t, ok := f.get().(pgx.CopyFromTracer); ok {
		t.TraceCopyFromEnd(ctx, c, d)
	}
}

// Manager runs every transaction inside the test's transaction; see
// pg.Within for what that changes.
func (d *Database) Manager() *pg.Manager { return d.manager }

// Module is the pg module, providing Manager, for an app built with New. The
// Manager then sends its spans and logs to the app's tracer provider and
// logger.
func (d *Database) Module() galleon.Module { return pg.Module(pg.Use(d.manager)) }

// DSN is the package's database itself, outside the test's transaction.
// What is committed through it stays for the rest of the package's tests,
// so a test that uses it cleans up after itself.
func (d *Database) DSN() string { return d.dsn }

var packageDBs struct {
	mu  sync.Mutex
	dbs map[string]packageDatabase // by schema key
	// live holds, for as long as the process runs, a session-level
	// advisory lock on each package database in dbs: the mark that the
	// database is in use, even while nothing is connected to it.
	live *pgx.Conn
	// checked is when live was last found working.
	checked time.Time
}

type packageDatabase struct{ name, dsn string }

// adminDSN is the value of PostgresEnv, without the whitespace around it,
// so that everything reading it reads the same DSN.
func adminDSN() string { return strings.TrimSpace(os.Getenv(PostgresEnv)) }

// liveCheckEvery is how often, at most, a package database handed out
// again checks that the connection holding the live marks still works.
// Tests set it to zero.
var liveCheckEvery = time.Second

// liveLock is the advisory lock key, in SQL, that marks database $1 live.
const liveLock = "hashtext('galleon.testkit.live'), hashtext($1)"

// packageDB returns the DSN of this binary's database for schema, creating
// the template and the clone the first time, and again if the database
// went away while its mark was lost.
func packageDB(admin string, schema []string) (string, error) {
	key, tpl, base, err := dbNames(schema)
	if err != nil {
		return "", err
	}
	packageDBs.mu.Lock()
	defer packageDBs.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if d, ok := packageDBs.dbs[key]; ok {
		if time.Since(packageDBs.checked) < liveCheckEvery || liveAlive(ctx) {
			return d.dsn, nil
		}
	}

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

	// Before dropStale, which would otherwise win this run's own marks if
	// their connection had died.
	live, err := liveConn(ctx, conn, admin)
	if err != nil {
		return "", err
	}
	if d, ok := packageDBs.dbs[key]; ok {
		return d.dsn, nil
	}
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", tpl).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		if err := createTemplate(ctx, conn, admin, tpl, schema); err != nil {
			return "", err
		}
	}
	if err := dropStale(ctx, conn, base); err != nil {
		return "", err
	}
	for attempt := 1; ; attempt++ {
		d, err := clone(ctx, conn, live, admin, tpl, base)
		var pe *pgconn.PgError
		if (errors.Is(err, errNameTaken) || errors.As(err, &pe) && pe.Code == "42P04") && attempt < 3 { // duplicate_database
			continue
		}
		if err != nil {
			return "", err
		}
		if packageDBs.dbs == nil {
			packageDBs.dbs = map[string]packageDatabase{}
		}
		packageDBs.dbs[key] = d
		return d.dsn, nil
	}
}

// errNameTaken is a package database name another run has marked.
var errNameTaken = errors.New("the name is another run's")

// clone creates a package database from the template under a new name,
// marked live before it exists, and under the turn-taking lock, so no
// other run sees it unmarked. If the name is another's, marked or created,
// it is left alone; otherwise, if it fails, the mark and the database go.
func clone(ctx context.Context, conn, live *pgx.Conn, admin, tpl, base string) (packageDatabase, error) {
	suffix, err := randomSuffix()
	if err != nil {
		return packageDatabase{}, err
	}
	// The pid helps tell runs apart; the random part keeps names apart
	// across hosts sharing a server.
	name := fmt.Sprintf("%s_%d_%s", base, os.Getpid(), suffix)
	dsn, err := withDatabase(admin, name)
	if err != nil {
		return packageDatabase{}, err
	}
	var won bool
	if err := live.QueryRow(ctx, "SELECT pg_try_advisory_lock("+liveLock+")", name).Scan(&won); err != nil {
		return packageDatabase{}, fmt.Errorf("marking the package database live: %w", err)
	}
	if !won {
		return packageDatabase{}, errNameTaken
	}
	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE "+pgx.Identifier{tpl}.Sanitize())
	if err == nil {
		return packageDatabase{name: name, dsn: dsn}, nil
	}
	err = fmt.Errorf("cloning the template: %w", err)
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var derr error
	if pe := (*pgconn.PgError)(nil); !errors.As(err, &pe) || pe.Code != "42P04" {
		_, derr = conn.Exec(dctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	}
	_, uerr := live.Exec(dctx, "SELECT pg_advisory_unlock("+liveLock+")", name)
	return packageDatabase{}, errors.Join(err, derr, uerr)
}

// randomSuffix ends a package database's name. Tests replace it.
var randomSuffix = func() (string, error) {
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(rnd[:]), nil
}

// liveAlive reports whether the connection holding the live marks works.
func liveAlive(ctx context.Context) bool {
	l := packageDBs.live
	if l == nil {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if l.Ping(pctx) != nil {
		return false
	}
	packageDBs.checked = time.Now()
	return true
}

// liveConn returns the connection that holds the live marks, opening it
// the first time, and again if it has died. Then it marks anew the
// databases that are still there, and forgets the ones that are gone, so
// they are created again. conn holds the turn-taking lock, so no other
// run drops a database meanwhile.
//
// It is its own connection, which nothing else uses: a session's advisory
// locks are reentrant, so one that tried the locks of others' databases
// would win its own too.
func liveConn(ctx context.Context, conn *pgx.Conn, admin string) (*pgx.Conn, error) {
	if liveAlive(ctx) {
		return packageDBs.live, nil
	}
	if l := packageDBs.live; l != nil {
		l.Close(context.WithoutCancel(ctx))
		packageDBs.live = nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	l, err := pgx.Connect(cctx, admin)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", PostgresEnv, err)
	}
	for key, d := range packageDBs.dbs {
		var exists bool
		err := conn.QueryRow(cctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", d.name).Scan(&exists)
		if err == nil && !exists {
			delete(packageDBs.dbs, key)
			continue
		}
		if err == nil {
			_, err = l.Exec(cctx, "SELECT pg_advisory_lock("+liveLock+")", d.name)
		}
		if err != nil {
			l.Close(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("marking the package databases live again: %w", err)
		}
	}
	packageDBs.live = l
	packageDBs.checked = time.Now()
	return l, nil
}

// dbNames returns the schema's key, its template's name, and the prefix of
// this package's databases for it.
func dbNames(schema []string) (key, tpl, base string, err error) {
	h := sha256.Sum256([]byte(strings.Join(schema, "\x00")))
	key = hex.EncodeToString(h[:6])
	wd, err := os.Getwd()
	if err != nil {
		return "", "", "", err
	}
	w := sha256.Sum256([]byte(wd))
	return key, "galleon_tpl_" + key, "galleon_pkg_" + hex.EncodeToString(w[:6]) + "_" + key, nil
}

// dropStale drops this package's databases for the schema that earlier
// runs left. A run still going holds the advisory lock that marks its
// database live, connected to it or not, so only a database whose lock
// conn wins is dropped.
func dropStale(ctx context.Context, conn *pgx.Conn, base string) error {
	rows, err := conn.Query(ctx, "SELECT datname FROM pg_database WHERE starts_with(datname, $1)", base)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, name := range names {
		var won bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock("+liveLock+")", name).Scan(&won); err != nil {
			return err
		}
		if !won {
			continue
		}
		_, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		_, uerr := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock("+liveLock+")", name)
		if err := errors.Join(err, uerr); err != nil {
			return err
		}
	}
	return nil
}

// createTemplate builds the template under a temporary name and renames
// it once the schema is in, so a run killed halfway never leaves a template
// that later runs would clone. A leftover from such a run is dropped first.
func createTemplate(ctx context.Context, conn *pgx.Conn, admin, tpl string, schema []string) error {
	building := tpl + "_building"
	ident := pgx.Identifier{building}.Sanitize()
	drop := func() {
		// ctx may be what ran out.
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = conn.Exec(dctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)")
	}
	drop()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		return fmt.Errorf("creating the template: %w", err)
	}
	apply := func() error {
		dsn, err := withDatabase(admin, building)
		if err != nil {
			return err
		}
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return err
		}
		defer c.Close(context.WithoutCancel(ctx))
		for _, sql := range schema {
			if _, err := c.Exec(ctx, sql); err != nil {
				return fmt.Errorf("applying the schema: %w", err)
			}
		}
		return nil
	}
	if err := apply(); err != nil {
		drop()
		return err
	}
	// The schema's backend can outlive its connection by a moment, and a
	// database can't be renamed while anyone is connected.
	var err error
	for range 50 {
		if _, err = conn.Exec(ctx, "ALTER DATABASE "+ident+" RENAME TO "+pgx.Identifier{tpl}.Sanitize()); err == nil {
			return nil
		}
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "55006" { // object_in_use
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	drop()
	return fmt.Errorf("naming the template: %w", err)
}

// withDatabase returns s with its database replaced, in either DSN form.
func withDatabase(s, db string) (string, error) {
	if dsn.IsURL(s) {
		u, err := url.Parse(strings.TrimSpace(s))
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
			return "", fmt.Errorf("%s is not a postgres:// URL or a keyword/value string", PostgresEnv)
		}
		u.Path = "/" + db
		u.RawPath = ""
		return u.String(), nil
	}
	// In the keyword/value form, a later keyword wins.
	return s + " dbname='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(db) + "'", nil
}
