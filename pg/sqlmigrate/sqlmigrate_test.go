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
		`2_risk.sql: unknown risk "huge"`,
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
	if got := strings.Join(migrate.Capabilities(m), ","); got != "plan,verify" {
		t.Errorf("capabilities %s", got)
	}
}
