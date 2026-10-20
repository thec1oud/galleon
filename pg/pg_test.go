package pg_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/pg"
)

// The tests that need a database are in testkit/conformance/datatest,
// which runs the data seam's conformance suite against this package.

func TestConfigIsValidated(t *testing.T) {
	ok := pg.Config{Primary: pg.Source{DSN: "postgres://app:hunter2@db/orders"}, Leaks: "error"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]pg.Config{
		"no dsn":         {Leaks: "error"},
		"bad leaks":      {Primary: ok.Primary, Leaks: "ignore"},
		"named primary":  {Primary: ok.Primary, Leaks: "error", Named: map[string]pg.Source{"primary": ok.Primary}},
		"bad name":       {Primary: ok.Primary, Leaks: "error", Named: map[string]pg.Source{"Big-Name": ok.Primary}},
		"named no dsn":   {Primary: ok.Primary, Leaks: "error", Named: map[string]pg.Source{"other": {}}},
		"min over max":   {Primary: pg.Source{DSN: ok.Primary.DSN, MaxConns: 2, MinConns: 3}, Leaks: "error"},
		"negative":       {Primary: pg.Source{DSN: ok.Primary.DSN, ConnectTimeout: -1}, Leaks: "error"},
		"negative retry": {Primary: ok.Primary, Leaks: "error", RetryAttempts: -1},
		"unparseable":    {Primary: pg.Source{DSN: "postgres://app:hunter2@db:notaport/x"}, Leaks: "error"},
	} {
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error shows the password: %v", name, err)
		}
	}
}

// Every DSN that leaked through masking its text: none of what pgx reads as
// the password shows, and nothing needed masking.
func TestSourcesNeverShowTheirPasswords(t *testing.T) {
	cleanPGEnv(t)
	for _, dsn := range []string{
		"postgres://app:hunter2@db/orders",
		"postgres://app@db/orders?password=hunter2",
		"host=db user=app password=hunter2 dbname=orders",
		"host=db password='hunter2 x' dbname=orders",
		"postgres://app:hunter2%zz@db/orders",
		"postgres://app:hun@ter2%zz@db/orders",
		"postgres://app:hun@ter2%zz@db",
		"host=db user=app password=hunter2 application_name=http://x",
		"postgres://app:hunter2@db/orders?sslmode=%zz&password=hunter2",
		"postgres://app:hun/ter2@db/orders",
		"postgress://app:hunter2@db",
		"POSTGRES://app:hunter2@db/orders",
		"postgres://app@db/orders?password=hunter2%zz",
		"postgres://app@db/orders?sslmode=disable&password=hunter2&password=hunter2",
		"postgres://app@db:5432/orders?password=hun@ter2",
		"postgres://app:#hunter2@db/orders",
		"postgres://app:123?ter2@db",
		"postgres://app:5432/ter2@db/orders",
		" postgres://app:hunter2@db",
		"postgres://app:hunter2@db,db2:5433/orders",
		"host=db password=hunter2%zz",
		"postgres://app:hunter2@db/orders?application_name=hunter2",
		"host=db password=hunter2 application_name=hunter2",
		"postgres://app@db/orders?pass%77ord=hunter2",
		"postgres://app@db/orders?SSLPassword=hunter2",
		"postgres://app:hun%2Fter2@db/orders",
		"postgres://app:hun/ter2@db/orders?pass%77ord=hunter2",
		`host=db password=hun\ ter2`,
		"host=db password\v=hunter2",
		"host=db password=hunter2 sslpassword=hunter2 sslmode=disable",
	} {
		s := fmt.Sprint(pg.Source{DSN: dsn})
		m := fmt.Sprint(map[string]pg.Source{"x": {DSN: dsn}})
		for _, out := range []string{s, m} {
			// "ter2" ends every password here, '@' or not.
			if strings.Contains(out, "ter2") || strings.Contains(out, "xxxxx") {
				t.Errorf("%q prints as %s", dsn, out)
			}
			if c, err := pgconn.ParseConfig(dsn); err == nil && c.Password != "" && strings.Contains(out, c.Password) {
				t.Errorf("%q prints as %s, with the password pgx reads", dsn, out)
			}
		}
	}
}

// A source prints as a summary of what pgx reads from its DSN, never the
// DSN's text.
// cleanPGEnv clears the PG* variables pgconn reads, so what a DSN parses
// to doesn't depend on where the test runs.
func cleanPGEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{"PGAPPNAME", "PGHOST", "PGPORT", "PGUSER", "PGDATABASE", "PGSSLMODE",
		"PGCONNECT_TIMEOUT", "PGPASSWORD", "PGPASSFILE", "PGSERVICE", "PGSERVICEFILE", "PGTARGETSESSIONATTRS",
		"PGSSLCERT", "PGSSLKEY", "PGSSLROOTCERT", "PGSSLPASSWORD", "PGSSLSNI", "PGSSLNEGOTIATION", "PGOPTIONS"} {
		t.Setenv(v, "")
	}
	// Nor on a password file in the home directory.
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "none"))
}

// rootCert writes a self-signed CA certificate and returns its path.
func rootCert(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "root.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSourcesPrintASummaryOfTheirDSN(t *testing.T) {
	cleanPGEnv(t)
	root := rootCert(t)
	for dsn, want := range map[string]string{
		"postgres://app@db/orders?sslmode=verify-ca&sslrootcert=" + root:                                      "postgres://app@db:5432/orders?tls=verified-ca",
		"postgres://app@db/orders?sslmode=require&sslrootcert=" + root:                                        "postgres://app@db:5432/orders?tls=verified-ca",
		"postgres://app@db/orders?sslmode=verify-full&sslrootcert=" + root:                                    "postgres://app@db:5432/orders?tls=verified",
		"postgres://app@db/orders?sslmode=disable&sslpassword=s3cret&application_name=s3cret-api":             "postgres://app@db:5432/orders?tls=off",
		"host=db user=app dbname=orders sslmode=disable sslpassword='s3 cret' application_name='s3 cret-api'": "postgres://app@db:5432/orders?tls=off",
		"host=db user=app dbname=orders sslmode=disable sslpassword=other application_name=api":               "postgres://app@db:5432/orders?application_name=api&tls=off",
		"postgres://app:hunter2@db/orders":                                                                    "postgres://app@db:5432/orders?tls=preferred",
		"host=db port=5433 user=app dbname=orders sslmode=disable application_name=api connect_timeout=5":     "postgres://app@db:5433/orders?application_name=api&connect_timeout=5s&tls=off",
		// The password is also the user's and the database's name, which
		// show as they would with any other password.
		"postgres://postgres:postgres@localhost/postgres?sslmode=disable":            "postgres://postgres@localhost:5432/postgres?tls=off",
		"postgres://app:hunter2@db,db2:5433,db3/orders?sslmode=require":              "postgres://app@db:5432/orders?tls=required +2 hosts",
		"postgres://app@db,db2/orders?sslmode=prefer":                                "postgres://app@db:5432/orders?tls=preferred +1 host",
		"postgres://app@db/orders?sslmode=allow":                                     "postgres://app@db:5432/orders?tls=allowed",
		"postgres://app@db/orders?sslmode=verify-full":                               "postgres://app@db:5432/orders?tls=verified",
		"postgres://app@[::1]:6432/orders?sslmode=disable":                           "postgres://app@[::1]:6432/orders?tls=off",
		"host=/run/postgresql user=app dbname=orders":                                "postgres://app@/run/postgresql:5432/orders?tls=off",
		"postgres://app:hunter2@db/orders?sslmode=disable&application_name=hunter2x": "postgres://app@db:5432/orders?tls=off",
		"postgres://app:hun@ter2%zz@db":                                              "<invalid DSN>",
		"postgres://app:5432/ter2@db/orders":                                         "<ambiguous DSN>",
		"host=db port=notaport":                                                      "<invalid DSN>",
		"postgres://app@db/orders?sslmode=%zz":                                       "<invalid DSN>",
		"postgres://app@db/orders?sslmode=sometimes":                                 "<invalid DSN>",
	} {
		if s := fmt.Sprint(pg.Source{DSN: dsn}); !strings.HasPrefix(s, "{dsn:"+want+" max_conns") {
			t.Errorf("%q prints as %s, want %s", dsn, s, want)
		}
	}
}

// A validated source shows the DSN as validation parsed it; one never
// validated parses it when printed.
func TestValidatedSourcesKeepTheirSummary(t *testing.T) {
	cleanPGEnv(t)
	t.Setenv("PGHOST", "first")
	c := pg.Config{Primary: pg.Source{DSN: "postgres://app@/orders?sslmode=disable"},
		Named: map[string]pg.Source{"other": {DSN: "postgres://app@/other?sslmode=disable"}}, Leaks: "error"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGHOST", "second")
	if s := c.Primary.String(); !strings.Contains(s, "@first:5432/") {
		t.Errorf("the validated source prints as %s", s)
	}
	if s := c.Named["other"].String(); !strings.Contains(s, "@first:5432/") {
		t.Errorf("the validated named source prints as %s", s)
	}
	if s := (pg.Source{DSN: c.Primary.DSN}).String(); !strings.Contains(s, "@second:5432/") {
		t.Errorf("a source never validated prints as %s", s)
	}
}

func TestConfigReportHidesPasswords(t *testing.T) {
	app, err := galleon.New(galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode),
		galleon.Modules(pg.Module()),
		galleon.ConfigJSON([]byte(`{"pg": {"primary": {"dsn": "postgres://app:hunter2@db/orders"}, "named": {"other": {"dsn": "postgres://app:hunter2@db/other"}}}}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	for _, v := range app.ConfigReport() {
		if strings.Contains(v.Value, "hunter2") {
			t.Fatalf("the config report shows a password: %+v", v)
		}
	}
}

func TestBootFailsOnABadSection(t *testing.T) {
	_, err := galleon.New(galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode),
		galleon.Modules(pg.Module()), galleon.ConfigJSON([]byte(`{"pg": {"primary": {}}}`)))
	if err == nil || !strings.Contains(err.Error(), "dsn must be set") {
		t.Fatalf("got %v", err)
	}
}

func TestNewDoesNotConnect(t *testing.T) {
	m, err := pg.New(pg.Config{Primary: pg.Source{DSN: "postgres://nobody@127.0.0.1:1/x"}})
	if err != nil {
		t.Fatal(err)
	}
	if m.Pool("") != nil {
		t.Fatal("a pool exists before Start")
	}
	_, err = m.BeginWrite(context.Background())
	if errs.KindOf(err) != errs.Unavailable {
		t.Fatalf("a transaction before Start: %v", err)
	}
	if _, err := m.BeginRead(context.Background(), data.Source("nope")); err == nil {
		t.Fatal("an unknown source was accepted")
	}
}

func TestRetryableErrors(t *testing.T) {
	for err, want := range map[error]bool{
		&pgconn.PgError{Code: "40001"}:                             true,
		&pgconn.PgError{Code: "40P01"}:                             true,
		fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "40001"}):  true,
		&pgconn.PgError{Code: "23505"}:                             false,
		errs.New(errs.Conflict, data.CodeSerializationFailure, ""): true,
		errs.New(errs.Conflict, data.CodeUniqueViolation, ""):      false,
		io.EOF: false,
		errors.Join(errors.New("x"), &pgconn.PgError{Code: "40P01"}): true,
	} {
		if data.Retryable(err) != want {
			t.Errorf("Retryable(%v) = %v", err, !want)
		}
	}
}

func TestRetryPolicyIsBounded(t *testing.T) {
	runs := 0
	err := data.RetryPolicy{Attempts: 4, Backoff: 1}.Run(context.Background(), func(context.Context) error {
		runs++
		return &pgconn.PgError{Code: "40001"}
	})
	if runs != 4 || !data.Retryable(err) {
		t.Fatalf("%d runs, %v", runs, err)
	}
	runs = 0
	_ = data.RetryPolicy{}.Run(context.Background(), func(context.Context) error { runs++; return errors.New("not retryable") })
	if runs != 1 {
		t.Fatalf("a plain error was retried %d times", runs-1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runs = 0
	_ = data.RetryPolicy{Attempts: 10, Backoff: 1 << 40}.Run(ctx, func(context.Context) error { runs++; return &pgconn.PgError{Code: "40001"} })
	if runs != 1 {
		t.Fatalf("retried %d times after ctx ended", runs-1)
	}
}
