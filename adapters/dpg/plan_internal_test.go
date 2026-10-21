package dpg

import (
	"strings"
	"testing"

	"github.com/thec1oud/dpg/pkg/dpg/engine"

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
		// A safety this adapter doesn't know.
		{engine.Step{Safety: engine.Destructive + 1}, migrate.Unknown},
		{engine.Step{Safety: -1}, migrate.Unknown},
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
		{Migration: "m3", Safety: engine.Safe, Mode: engine.Instruction, SQL: "-- rebuild the partitions"},
		{Migration: "m4", Safety: engine.Destructive + 1, SQL: "SELECT 1"},
	})
	want := []migrate.StepInfo{
		{ID: "m1", Risk: migrate.Caution, Description: "dpg migration m1 (statements: 2 in its transaction)"},
		{ID: "m2", Risk: migrate.Safe, Description: "dpg migration m2 (statements: 1 before its transaction, 1 after its transaction)"},
		{ID: "m3", Risk: migrate.Manual, Description: "dpg migration m3 (statements: 1 in its transaction, 1 for a person to do)"},
		{ID: "m4", Risk: migrate.Unknown, Description: "dpg migration m4 (statements: 1 in its transaction)"},
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
