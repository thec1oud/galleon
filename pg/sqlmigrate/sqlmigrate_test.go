package sqlmigrate_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/migrate"
	"github.com/thec1oud/galleon/pg/sqlmigrate"
)

// The tests that need a database are in testkit/conformance/migratetest,
// which runs the migration seam's conformance suite against this package.

func TestBadFilesFailNewTogether(t *testing.T) {
	_, err := sqlmigrate.New("schema", fstest.MapFS{
		"0001_a.sql":       {Data: []byte("SELECT 1;")},
		"001_again.sql":    {Data: []byte("SELECT 1;")},
		"create_users.sql": {Data: []byte("SELECT 1;")},
		"0_zero.sql":       {Data: []byte("SELECT 1;")},
		"2_risk.sql":       {Data: []byte("-- galleon:risk huge\nSELECT 1;")},
		"3_directive.sql":  {Data: []byte("-- galleon:transaction-please\nSELECT 1;")},
		"README.md":        {Data: []byte("not a migration")},
	}, nil)
	if err == nil {
		t.Fatal("bad files were accepted")
	}
	for _, want := range []string{
		"version 1 is also 0001_a.sql",
		"create_users.sql: a migration must be named <version>_<name>.sql",
		"0_zero.sql: the version must be a positive number",
		"2_risk.sql: directive galleon:risk huge: want safe, caution, destructive or manual",
		"3_directive.sql: unknown directive galleon:transaction-please",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error lacks %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "README") {
		t.Errorf("a file that isn't .sql was read: %v", err)
	}
}

func TestANameMustBeValid(t *testing.T) {
	if _, err := sqlmigrate.New("Schema!", fstest.MapFS{}, nil); err == nil {
		t.Fatal("a bad name was accepted")
	}
}

func TestApplyRefusesToRunWithoutALease(t *testing.T) {
	m, err := sqlmigrate.New("schema", fstest.MapFS{"1_a.sql": {Data: []byte("SELECT 1;")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Apply(context.Background(), migrate.ApplyOptions{MaxRisk: migrate.Manual, AllowUnknown: true})
	if e := errs.From(err); e == nil || e.Code != sqlmigrate.CodeNoLock {
		t.Fatalf("%v", err)
	}
}

func TestTheMigratorHasThePlannerAndVerifierCapabilities(t *testing.T) {
	m, err := sqlmigrate.New("schema", fstest.MapFS{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(migrate.Capabilities(m), ","); got != "plan,plan_offline,verify" {
		t.Errorf("capabilities %s", got)
	}
}

func TestDirectivesStopAtTheFirstLineOfSQLHoweverLong(t *testing.T) {
	long := "INSERT INTO t VALUES ('" + strings.Repeat("x", 200_000) + "');\n-- galleon:risk huge"
	m, err := sqlmigrate.New("schema", fstest.MapFS{
		"1_long.sql":  {Data: []byte(long)},
		"2_risky.sql": {Data: []byte("-- Galleons sail; this comment isn't a directive\n-- galleon:risk caution\n" + long)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := m.PlanOffline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s := pl.Steps(); s[0].Risk != migrate.Unknown || s[1].Risk != migrate.Caution {
		t.Errorf("risks %s and %s", s[0].Risk, s[1].Risk)
	}
}

func TestBadDirectivesAreErrorsNotIgnored(t *testing.T) {
	for body, says := range map[string]string{
		"-- galleon:risk safe\n-- galleon:risk caution\nSELECT 1;": "directive galleon:risk is given twice",
		"-- galleon:no-transaction false\nSELECT 1;":               `galleon:no-transaction takes no value, not "false"`,
		"-- Galleon:risk safe\nSELECT 1;":                          "directives are written",
		"-- galleon :risk safe\nSELECT 1;":                         "directives are written",
		"-- galleon:risk\tsafe\nSELECT 1;":                         "one space, no tabs",
		"-- galleon: risk safe\nSELECT 1;":                         "one space, no tabs",
		"-- galleon:risk  safe\nSELECT 1;":                         "one space, no tabs",
		"-- galleon:risk\nSELECT 1;":                               "galleon:risk needs a value",
		"-- galleon:risk Safe\nSELECT 1;":                          "in lower case",
		"-- galleon:no-transactions\nSELECT 1;":                    "unknown directive galleon:no-transactions",
	} {
		_, err := sqlmigrate.New("schema", fstest.MapFS{"1_a.sql": {Data: []byte(body)}}, nil)
		if err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%q: want %q, got %v", body, says, err)
		}
	}
}

func TestTheTableNameMustFit(t *testing.T) {
	for name, ok := range map[string]bool{
		"galleon_migrations":           true,
		"ops.migrations":               true,
		strings.Repeat("t", 57):        true,
		strings.Repeat("t", 58):        false,
		"":                             false,
		"ops.":                         false,
		"a.b.c":                        false,
		strings.Repeat("s", 64) + ".t": false,
	} {
		_, err := sqlmigrate.New("schema", fstest.MapFS{}, nil, sqlmigrate.Table(name))
		if (err == nil) != ok {
			t.Errorf("table %q: %v", name, err)
		}
	}
}
