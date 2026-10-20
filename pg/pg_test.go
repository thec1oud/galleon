package pg_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

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

func TestSourcesHideTheirPasswords(t *testing.T) {
	for _, dsn := range []string{
		"postgres://app:hunter2@db/orders",
		"postgres://app@db/orders?password=hunter2",
		"host=db user=app password=hunter2 dbname=orders",
		"host=db password='hunter2 x' dbname=orders",
	} {
		if s := fmt.Sprint(pg.Source{DSN: dsn}); strings.Contains(s, "hunter2") || !strings.Contains(s, "db") {
			t.Errorf("%q prints as %s", dsn, s)
		}
		if s := fmt.Sprint(map[string]pg.Source{"x": {DSN: dsn}}); strings.Contains(s, "hunter2") {
			t.Errorf("in a map, %q prints as %s", dsn, s)
		}
	}
}

func TestConfigReportRedactsDSNs(t *testing.T) {
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
