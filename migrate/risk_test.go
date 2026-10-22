package migrate_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/migrate"
)

func TestRisksAreOrderedWithUnknownAboveThemAll(t *testing.T) {
	order := []migrate.Risk{migrate.Safe, migrate.Caution, migrate.Destructive, migrate.Manual, migrate.Unknown}
	for i, lo := range order {
		for _, hi := range order[i+1:] {
			if !hi.Above(lo) || lo.Above(hi) {
				t.Errorf("%s isn't above %s", hi, lo)
			}
		}
		if lo.Above(lo) {
			t.Errorf("%s is above itself", lo)
		}
	}
	var zero migrate.Risk
	if zero != migrate.Unknown {
		t.Error("the zero risk isn't Unknown")
	}
	if got := migrate.MaxRisk(nil); got != migrate.Safe {
		t.Errorf("no steps are %s", got)
	}
	if got := migrate.MaxRisk([]migrate.StepInfo{{Risk: migrate.Caution}, {Risk: migrate.Safe}}); got != migrate.Caution {
		t.Errorf("max %s", got)
	}
}

func TestRisksParseAndPrintByName(t *testing.T) {
	for _, r := range []migrate.Risk{migrate.Safe, migrate.Caution, migrate.Destructive, migrate.Manual, migrate.Unknown} {
		got, err := migrate.ParseRisk(strings.ToUpper(r.String()))
		if err != nil || got != r {
			t.Errorf("%s: %v, %v", r, got, err)
		}
		b, _ := json.Marshal(struct{ R migrate.Risk }{r})
		var back struct{ R migrate.Risk }
		if err := json.Unmarshal(b, &back); err != nil || back.R != r {
			t.Errorf("%s through JSON (%s): %v, %v", r, b, back.R, err)
		}
	}
	if _, err := migrate.ParseRisk("risky"); err == nil {
		t.Error("an unknown name parsed")
	}
}

func TestModesParseByName(t *testing.T) {
	for _, m := range []migrate.Mode{migrate.ModeOff, migrate.ModeVerify, migrate.ModeApply} {
		if got, err := migrate.ParseMode(m.String()); err != nil || got != m {
			t.Errorf("%s: %v, %v", m, got, err)
		}
	}
	if _, err := migrate.ParseMode("unset"); err == nil {
		t.Error("unset parsed")
	}
}

func TestUnknownIsAllowedOnlyByItsOwnSetting(t *testing.T) {
	if migrate.Allowed(migrate.Unknown, migrate.Manual, false) || !migrate.Allowed(migrate.Unknown, migrate.Safe, true) {
		t.Error("Unknown follows MaxRisk")
	}
	if migrate.Allowed(migrate.Destructive, migrate.Caution, true) || !migrate.Allowed(migrate.Caution, migrate.Caution, false) {
		t.Error("MaxRisk isn't applied")
	}
}

func TestPolicyDecidesWhichStatesAreReady(t *testing.T) {
	p := migrate.ProductionPolicy()
	if p.Mode != migrate.ModeVerify || p.MaxRisk != migrate.Caution || p.AllowUnknown {
		t.Errorf("production %+v", p)
	}
	if d := migrate.DevelopmentPolicy(); d.Mode != migrate.ModeApply || !d.AllowUnknown {
		t.Errorf("development %+v", d)
	}
	for _, tc := range []struct {
		s        migrate.State
		tolerate bool
		ready    bool
		says     string
	}{
		{migrate.UpToDate, false, true, ""},
		{migrate.Pending, false, false, "changes are pending (database 1, build 2): 1 pending; apply them"},
		{migrate.Drifted, false, false, "has drifted"},
		{migrate.Ahead, false, false, "tolerate_ahead"},
		{migrate.Ahead, true, true, ""},
		{0, false, false, "no state"},
	} {
		p.TolerateAhead = tc.tolerate
		err := p.Ready("schema", migrate.Status{State: tc.s, Current: "1", Expected: "2", Detail: "1 pending"})
		if (err == nil) != tc.ready || (err != nil && (!strings.Contains(err.Error(), tc.says) || errs.KindOf(err) != errs.Unavailable)) {
			t.Errorf("%s, tolerating %v: %v", tc.s, tc.tolerate, err)
		}
	}
}

func TestFingerprintsCoverEveryFieldAndContent(t *testing.T) {
	base := []migrate.StepInfo{{ID: "1", Description: "a", Risk: migrate.Safe}}
	fp := migrate.Fingerprint(base, [][]byte{[]byte("x")})
	for name, other := range map[string]string{
		"id":      migrate.Fingerprint([]migrate.StepInfo{{ID: "2", Description: "a", Risk: migrate.Safe}}, [][]byte{[]byte("x")}),
		"desc":    migrate.Fingerprint([]migrate.StepInfo{{ID: "1", Description: "b", Risk: migrate.Safe}}, [][]byte{[]byte("x")}),
		"risk":    migrate.Fingerprint([]migrate.StepInfo{{ID: "1", Description: "a", Risk: migrate.Caution}}, [][]byte{[]byte("x")}),
		"content": migrate.Fingerprint(base, [][]byte{[]byte("y")}),
		"split":   migrate.Fingerprint([]migrate.StepInfo{{ID: "1a", Description: "", Risk: migrate.Safe}}, [][]byte{[]byte("x")}),
	} {
		if other == fp {
			t.Errorf("changing the %s keeps the fingerprint", name)
		}
	}
	if migrate.NewPlan("", base).Fingerprint() != migrate.Fingerprint(base, nil) || migrate.NewPlan("mine", base).Fingerprint() != "mine" {
		t.Error("NewPlan's fingerprint")
	}
}
