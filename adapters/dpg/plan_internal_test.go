package dpg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thec1oud/dpg/pkg/dpg/engine"

	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/migrate"
)

func TestRisksMapDPGsSafetyAndNothingElseIsSafe(t *testing.T) {
	for _, c := range []struct {
		step engine.Step
		want migrate.Risk
	}{
		{engine.Step{Safety: engine.Safe}, migrate.Safe},
		{engine.Step{Safety: engine.Caution}, migrate.Caution},
		{engine.Step{Safety: engine.Destructive}, migrate.Destructive},
		{engine.Step{Safety: engine.Safe, Mode: engine.AfterTransaction}, migrate.Safe},
		// A step DPG leaves for a person is Manual, whatever its safety.
		{engine.Step{Safety: engine.Safe, Mode: engine.Instruction}, migrate.Manual},
		{engine.Step{Safety: engine.Destructive, Mode: engine.Instruction}, migrate.Manual},
		// A safety or a mode this adapter doesn't know.
		{engine.Step{Safety: engine.UnknownSafety}, migrate.Unknown},
		{engine.Step{Safety: engine.UnknownSafety + 1}, migrate.Unknown},
		{engine.Step{Safety: -1}, migrate.Unknown},
		{engine.Step{Safety: engine.Safe, Mode: engine.UnknownMode}, migrate.Unknown},
		{engine.Step{Safety: engine.Safe, Mode: engine.UnknownMode + 1}, migrate.Unknown},
	} {
		if got := riskOf(c.step); got != c.want {
			t.Errorf("%s/%s: %s, want %s", c.step.Mode, c.step.Safety, got, c.want)
		}
	}
}

func TestAPlanHasAStepPerMigrationAtItsRiskiestStatement(t *testing.T) {
	pl := planOf([]engine.Step{
		{Migration: "m1", Safety: engine.Safe, SQL: "CREATE TABLE a (id int)"},
		{Migration: "m1", Safety: engine.Caution, SQL: "ALTER TABLE a ALTER id TYPE bigint"},
		{Migration: "m2", Safety: engine.Safe, Mode: engine.BeforeTransaction, SQL: "ALTER TYPE e ADD VALUE IF NOT EXISTS 'x'"},
		{Migration: "m2", Safety: engine.Safe, Mode: engine.AfterTransaction, SQL: "CREATE INDEX CONCURRENTLY a_id ON a (id)"},
		{Migration: "m3", Safety: engine.Destructive, SQL: "DROP TABLE a"},
		{Migration: "m3", Safety: engine.Safe, Mode: engine.Instruction, SQL: "-- rebuild the partitions\n--   of p by hand"},
		{Migration: "m4", Safety: engine.UnknownSafety, SQL: "SELECT 1"},
		{Migration: "m5", Safety: engine.Safe, Mode: engine.UnknownMode, SQL: "SELECT 2"},
	})
	want := []migrate.StepInfo{
		{ID: "m1", Risk: migrate.Caution, Description: "dpg migration m1 (statements: 2 in its transaction)"},
		{ID: "m2", Risk: migrate.Safe, Description: "dpg migration m2 (statements: 1 before its transaction, 1 after its transaction)"},
		{ID: "m3", Risk: migrate.Manual, Description: "dpg migration m3 (statements: 1 in its transaction, 1 for a person to do); for a person to do: rebuild the partitions of p by hand"},
		{ID: "m4", Risk: migrate.Unknown, Description: "dpg migration m4 (statements: 1 in its transaction)"},
		{ID: "m5", Risk: migrate.Unknown, Description: "dpg migration m5 (statements: 1 of an unknown stage)"},
	}
	got := pl.Steps()
	if len(got) != len(want) {
		t.Fatalf("steps %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d: %+v, want %+v", i, got[i], want[i])
		}
		for _, sql := range []string{"CREATE", "ALTER", "DROP", "SELECT"} {
			if strings.Contains(got[i].Description, sql) {
				t.Errorf("step %d's description holds SQL: %q", i, got[i].Description)
			}
		}
	}
	if pl.MaxRisk() != migrate.Unknown {
		t.Errorf("max risk %s", pl.MaxRisk())
	}
	if empty := planOf(nil); len(empty.Steps()) != 0 {
		t.Errorf("a plan of nothing has steps: %+v", empty.Steps())
	}
}

func TestAFingerprintCoversEveryStatement(t *testing.T) {
	base := []engine.Step{
		{Migration: "m1", Safety: engine.Safe, SQL: "CREATE TABLE a (id int)"},
		{Migration: "m1", Safety: engine.Safe, Mode: engine.AfterTransaction, SQL: "CREATE INDEX CONCURRENTLY a_id ON a (id)"},
	}
	fp := planOf(base).Fingerprint()
	if fp == "" || planOf(append([]engine.Step(nil), base...)).Fingerprint() != fp {
		t.Fatalf("the fingerprint %q isn't stable", fp)
	}
	for name, change := range map[string]func(s []engine.Step){
		"the SQL":       func(s []engine.Step) { s[1].SQL = "CREATE INDEX CONCURRENTLY a_id ON a (id DESC)" },
		"a safety":      func(s []engine.Step) { s[0].Safety = engine.Caution },
		"a stage":       func(s []engine.Step) { s[1].Mode = engine.InTransaction },
		"the migration": func(s []engine.Step) { s[0].Migration, s[1].Migration = "m2", "m2" },
	} {
		s := append([]engine.Step(nil), base...)
		change(s)
		if planOf(s).Fingerprint() == fp {
			t.Errorf("changing %s keeps the fingerprint", name)
		}
	}
}

func TestAnUnknownStateIsNeverOneToApplyOver(t *testing.T) {
	for in, want := range map[engine.State]migrate.State{
		engine.UpToDate:  migrate.UpToDate,
		engine.Pending:   migrate.Pending,
		engine.Drifted:   migrate.Drifted,
		engine.Ahead:     migrate.Ahead,
		0:                migrate.Drifted,
		engine.Ahead + 1: migrate.Drifted,
	} {
		if got := statusOf(engine.Status{State: in}); got.State != want {
			t.Errorf("%s: %s, want %s", in, got.State, want)
		}
	}
	st := statusOf(engine.Status{State: engine.Pending, Expected: "m2", Detail: "1 migration(s) to run: m2"})
	if st.Expected != "m2" || st.Current != "none" || st.Detail != "1 migration(s) to run: m2" {
		t.Errorf("%+v", st)
	}
}

func TestMaxSafetyIsTheMostTheOptionsAllow(t *testing.T) {
	for max, want := range map[migrate.Risk]engine.Safety{
		migrate.Unknown:     engine.Safe, // the zero value means Safe
		migrate.Safe:        engine.Safe,
		migrate.Caution:     engine.Caution,
		migrate.Destructive: engine.Destructive,
		migrate.Manual:      engine.Destructive,
	} {
		if got := maxSafety(migrate.ApplyOptions{MaxRisk: max}); got != want {
			t.Errorf("MaxRisk %s: %s, want %s", max, got, want)
		}
	}
}

func TestErrorsNameStatementsByTheirStepIDs(t *testing.T) {
	create := `CREATE INDEX CONCURRENTLY "a_x" ON "public"."a" ("x");`
	m := &Migrator{name: "schema"}
	m.statements = []engine.Step{{ID: "m1/2", SQL: create}, {ID: "m1/1", SQL: `CREATE TABLE "a" ("x" int);`}, {ID: "m1/3", SQL: `CREATE TABLE "a" ("x" int);` + " -- longer"}}
	m.byLength = []engine.Step{m.statements[2], m.statements[0], m.statements[1]}
	pgErr := &pgconn.PgError{Severity: "ERROR", Code: "42P07", Message: `relation "a_x" already exists`}
	stale := errs.New(errs.FailedPrecondition, CodeStaleToken, `dpg: migrator "schema": the lease's fencing token 1 is stale`)
	for _, c := range []struct {
		err  error
		want string
	}{
		{
			fmt.Errorf("engine: c/app: finishing migration m1: %w", &engine.StatementError{SQL: create, Err: pgErr}),
			`dpg: migrator "schema": engine: c/app: finishing migration m1: executor: statement m1/2: ERROR: relation "a_x" already exists (SQLSTATE 42P07)`,
		},
		{
			// The fence refused the lease before DPG's own record.
			fmt.Errorf("engine: c/app: migration m1: %w", &engine.StatementError{SQL: "DO $dpg$ BEGIN INSERT INTO \"dpg\".migrations VALUES ('m1'); END $dpg$;", Err: stale}),
			`dpg: migrator "schema": engine: c/app: migration m1: executor: a statement of DPG's: dpg.stale_token: dpg: migrator "schema": the lease's fencing token 1 is stale`,
		},
		{
			// The drop of an invalid index before building it again.
			fmt.Errorf("engine: c/app: finishing migration m1: %w", &engine.StatementError{SQL: `DROP INDEX CONCURRENTLY IF EXISTS "public"."a_x";`, Err: context.Canceled}),
			`dpg: migrator "schema": engine: c/app: finishing migration m1: executor: a statement of DPG's: context canceled`,
		},
		{
			// Quoted outside a StatementError: the longer statement is
			// replaced whole.
			fmt.Errorf(`engine: c/app: DPG-E053: CREATE TABLE "a" ("x" int); -- longer`),
			`dpg: migrator "schema": engine: c/app: DPG-E053: statement m1/3`,
		},
	} {
		got := m.wrap(c.err)
		if got.Error() != c.want {
			t.Errorf("%v\n got  %s\n want %s", c.err, got, c.want)
		}
		if !errors.Is(got, c.err) {
			t.Errorf("%v doesn't wrap the original", got)
		}
	}
	// The adapter's own refusal is returned as it is; one the engine
	// wrapped keeps the engine's context and its code.
	if m.wrap(stale) != error(stale) {
		t.Error("the adapter's own error was wrapped")
	}
	got := m.wrap(fmt.Errorf("engine: c/app: migration m1: %w", stale))
	if e := errs.From(got); e == nil || e.Code != CodeStaleToken || !strings.Contains(got.Error(), "engine: c/app: migration m1: ") {
		t.Errorf("%v", got)
	}
}

func TestWarningsAreAddedToTheDetail(t *testing.T) {
	if got := withWarnings("1 migration(s) to run: m2", []string{"a.json: format 9"}); got != "1 migration(s) to run: m2; warning: a.json: format 9" {
		t.Errorf("%q", got)
	}
	if got := withWarnings("", []string{"x", "y"}); got != "warning: x; warning: y" {
		t.Errorf("%q", got)
	}
	if got := withWarnings("d", nil); got != "d" {
		t.Errorf("%q", got)
	}
}
