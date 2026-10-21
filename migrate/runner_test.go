package migrate_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/migrate"
)

var twoSafe = []migrate.StepInfo{
	{ID: "1_a", Description: "CREATE TABLE a", Risk: migrate.Safe},
	{ID: "2_b", Description: "CREATE TABLE b", Risk: migrate.Safe},
}

var dev = migrate.Policy{MaxRisk: migrate.Manual, AllowUnknown: true}

func newRunner(t *testing.T, l *locker, p migrate.Policy, ms ...migrate.Migrator) *migrate.Runner {
	t.Helper()
	r, err := migrate.NewRunner(migrate.Config{Locker: l, Migrators: ms, Policy: p})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newLocker() *locker { return &locker{Locker: local.New().Locks()} }

func hasCode(err error, code string) bool {
	e := errs.From(err)
	return e != nil && e.Code == code
}

// until polls cond until it holds, failing the test at the deadline.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(time.Millisecond) // a poll interval, not synchronization
	}
}

func TestARunnerNeedsALockerAndDistinctValidNames(t *testing.T) {
	d := newDB()
	if _, err := migrate.NewRunner(migrate.Config{Migrators: []migrate.Migrator{&fake{db: d, name: "a"}}}); err == nil {
		t.Error("a Runner without a Locker was built")
	}
	if _, err := migrate.NewRunner(migrate.Config{Locker: newLocker(), Migrators: []migrate.Migrator{&fake{db: d, name: "a"}, &fake{db: d, name: "a"}}}); err == nil {
		t.Error("two migrators of one name were accepted")
	}
	if _, err := migrate.NewRunner(migrate.Config{Locker: newLocker(), Migrators: []migrate.Migrator{&fake{db: d, name: "Bad Name"}}}); err == nil {
		t.Error("a bad name was accepted")
	}
}

func TestApplyRunsUnderTheHeldLeaseAndPassesItsToken(t *testing.T) {
	d := newDB()
	l := newLocker()
	m := planner{&fake{db: d, name: "schema", steps: twoSafe}}
	reps, err := newRunner(t, l, dev, m).Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reps[0].Outcome != migrate.Applied || len(reps[0].Result.Applied) != 2 {
		t.Fatalf("%+v", reps[0])
	}
	opts := d.opts[0]
	if opts.Lock == nil || opts.Lock.Key() != migrate.LockKey("schema") || opts.Lock.Token() == 0 {
		t.Fatalf("Apply got lease %v", opts.Lock)
	}
	if opts.Lock.Token() != l.last().Token() {
		t.Errorf("Apply got token %d, the lease has %d", opts.Lock.Token(), l.last().Token())
	}
	pl, _ := migrate.NewPlan("", twoSafe), 0
	if opts.Fingerprint != pl.Fingerprint() || opts.MaxRisk != migrate.Manual || !opts.AllowUnknown {
		t.Errorf("Apply got options %+v", opts)
	}
	// The lease is released once the apply ends.
	if _, ok, _ := l.Locker.TryAcquire(context.Background(), migrate.LockKey("schema"), time.Second); !ok {
		t.Error("the lease is still held after the apply")
	}
}

func TestLosingTheLeaseCancelsTheApplyAndSaysSo(t *testing.T) {
	d := newDB()
	l := newLocker()
	m := &fake{db: d, name: "schema", steps: twoSafe, block: make(chan struct{}), started: make(chan struct{})}
	r := newRunner(t, l, dev, m)
	done := make(chan migrate.ApplyReport, 1)
	go func() {
		reps, _ := r.Apply(context.Background())
		done <- reps[0]
	}()
	<-m.started
	l.last().lose()
	rep := <-done
	if rep.Outcome != migrate.Failed || !hasCode(rep.Err, migrate.CodeLeaseLost) {
		t.Fatalf("%+v", rep)
	}
	if !errors.Is(rep.Err, context.Canceled) {
		t.Errorf("the apply's ctx wasn't cancelled: %v", rep.Err)
	}
	if st, _ := m.Status(context.Background()); st.State != migrate.Pending {
		t.Errorf("status %s", st.State)
	}
}

func TestAFailedRenewalCancelsTheApply(t *testing.T) {
	d := newDB()
	l := newLocker()
	l.renewed = errors.New("partitioned")
	m := &fake{db: d, name: "schema", steps: twoSafe, block: make(chan struct{})}
	p := dev
	p.LockTTL = migrate.MinLockTTL
	_, err := newRunner(t, l, p, m).Apply(context.Background())
	if !hasCode(err, migrate.CodeLeaseLost) {
		t.Fatalf("%v", err)
	}
}

func TestANodeThatLosesTheRaceWaitsRereadsAndDoesntApply(t *testing.T) {
	d := newDB()
	l := newLocker()
	first := &fake{db: d, name: "schema", steps: twoSafe, block: make(chan struct{}), started: make(chan struct{})}
	second := &fake{db: d, name: "schema", steps: twoSafe}
	firstDone := make(chan error, 1)
	go func() {
		_, err := newRunner(t, l, dev, first).Apply(context.Background())
		firstDone <- err
	}()
	<-first.started
	secondDone := make(chan migrate.ApplyReport, 1)
	go func() {
		reps, _ := newRunner(t, l, dev, second).Apply(context.Background())
		secondDone <- reps[0]
	}()
	until(t, "the second node waits for the lock", func() bool { return l.waiting() == 1 })
	close(first.block)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	rep := <-secondDone
	if rep.Outcome != migrate.AlreadyUpToDate || !rep.Waited || rep.Err != nil {
		t.Fatalf("%+v", rep)
	}
	if n := d.callsOf("schema"); n != 1 {
		t.Fatalf("Apply ran %d times", n)
	}
}

func TestLockWaitBoundsTheWait(t *testing.T) {
	d := newDB()
	l := newLocker()
	held, _, _ := l.Locker.TryAcquire(context.Background(), migrate.LockKey("schema"), time.Minute)
	defer held.Release(context.Background())
	p := dev
	p.LockWait = 20 * time.Millisecond
	reps, err := newRunner(t, l, p, &fake{db: d, name: "schema", steps: twoSafe}).Apply(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || reps[0].Outcome != migrate.Failed || d.callsOf("schema") != 0 {
		t.Fatalf("%+v, %v", reps[0], err)
	}
}

func TestAPlanAboveThePolicyIsRefusedBeforeApply(t *testing.T) {
	steps := append(twoSafe[:1:1], migrate.StepInfo{ID: "2_drop", Risk: migrate.Destructive})
	for name, tc := range map[string]struct {
		steps  []migrate.StepInfo
		policy migrate.Policy
		code   string
	}{
		"above max risk":  {steps, migrate.Policy{MaxRisk: migrate.Caution, AllowUnknown: true}, migrate.CodeRiskRefused},
		"unknown refused": {[]migrate.StepInfo{{ID: "1_x"}}, migrate.Policy{MaxRisk: migrate.Manual}, migrate.CodeRiskRefused},
		"pinned elsewhere": {twoSafe, migrate.Policy{MaxRisk: migrate.Manual, Approved: map[string]string{"schema": "beef"}},
			migrate.CodeFingerprintMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDB()
			reps, err := newRunner(t, newLocker(), tc.policy, planner{&fake{db: d, name: "schema", steps: tc.steps}}).Apply(context.Background())
			if reps[0].Outcome != migrate.Refused || !hasCode(err, tc.code) {
				t.Fatalf("%+v, %v", reps[0], err)
			}
			if d.callsOf("schema") != 0 {
				t.Error("Apply ran")
			}
		})
	}
}

func TestAPolicyAllowsWhatItCovers(t *testing.T) {
	for name, p := range map[string]migrate.Policy{
		"unknown allowed": {MaxRisk: migrate.Safe, AllowUnknown: true},
		"pinned":          {MaxRisk: migrate.Safe, AllowUnknown: true, Approved: map[string]string{"schema": migrate.NewPlan("", []migrate.StepInfo{{ID: "1_x"}, twoSafe[0]}).Fingerprint()}},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDB()
			_, err := newRunner(t, newLocker(), p, planner{&fake{db: d, name: "schema", steps: []migrate.StepInfo{{ID: "1_x"}, twoSafe[0]}}}).Apply(context.Background())
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAMigratorThatCantPlanIsOfUnknownRisk(t *testing.T) {
	d := newDB()
	p := migrate.Policy{MaxRisk: migrate.Manual}
	reps, err := newRunner(t, newLocker(), p, &fake{db: d, name: "schema", steps: twoSafe}).Apply(context.Background())
	if reps[0].Outcome != migrate.Refused || !hasCode(err, migrate.CodeRiskRefused) || d.callsOf("schema") != 0 {
		t.Fatalf("%+v, %v", reps[0], err)
	}
	if len(reps[0].Notes) != 1 || !strings.Contains(reps[0].Notes[0], `plan: migrator "schema" can't plan`) {
		t.Errorf("notes %v", reps[0].Notes)
	}
	p.Approved = map[string]string{"schema": "beef"}
	p.AllowUnknown = true
	if _, err := newRunner(t, newLocker(), p, &fake{db: d, name: "schema", steps: twoSafe}).Apply(context.Background()); !hasCode(err, migrate.CodeNotSupported) {
		t.Errorf("a pin on a migrator that can't plan: %v", err)
	}
	p.Approved = nil
	reps, err = newRunner(t, newLocker(), p, &fake{db: d, name: "schema", steps: twoSafe}).Apply(context.Background())
	if err != nil || reps[0].Outcome != migrate.Applied || d.opts[0].Fingerprint != "" {
		t.Fatalf("%+v, %v", reps[0], err)
	}
}

func TestDriftedAndAheadDatabasesAreNotApplied(t *testing.T) {
	for _, tc := range []struct {
		state    migrate.State
		tolerate bool
		outcome  migrate.Outcome
	}{
		{migrate.Drifted, true, migrate.Refused},
		{migrate.Ahead, false, migrate.Refused},
		{migrate.Ahead, true, migrate.AheadTolerated},
	} {
		d := newDB()
		p := dev
		p.TolerateAhead = tc.tolerate
		reps, err := newRunner(t, newLocker(), p, &fake{db: d, name: "schema", steps: twoSafe, state: tc.state}).Apply(context.Background())
		if reps[0].Outcome != tc.outcome || (err == nil) != (tc.outcome == migrate.AheadTolerated) || d.callsOf("schema") != 0 {
			t.Errorf("%s, tolerating ahead %v: %+v, %v", tc.state, tc.tolerate, reps[0], err)
		}
	}
}

func TestStatusIsReadAgainAfterApply(t *testing.T) {
	d := newDB()
	reps, err := newRunner(t, newLocker(), dev, &fake{db: d, name: "schema", steps: twoSafe, stuck: true}).Apply(context.Background())
	if reps[0].Outcome != migrate.Failed || err == nil || !strings.Contains(err.Error(), "pending rather than up to date") {
		t.Fatalf("%+v, %v", reps[0], err)
	}
}

func TestMigratorsRunInOrderAndTheFirstFailureStopsTheRest(t *testing.T) {
	d := newDB()
	ms := []migrate.Migrator{
		&fake{db: d, name: "a", steps: twoSafe},
		&fake{db: d, name: "b", steps: twoSafe, fail: errBoom},
		&fake{db: d, name: "c", steps: twoSafe},
	}
	r := newRunner(t, newLocker(), dev, ms...)
	reps, err := r.Apply(context.Background())
	if !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	var got []migrate.Outcome
	for _, rep := range reps {
		got = append(got, rep.Outcome)
	}
	if want := []migrate.Outcome{migrate.Applied, migrate.Failed, migrate.NotRun}; !equal(got, want) {
		t.Fatalf("outcomes %v, want %v", got, want)
	}
	if d.callsOf("c") != 0 {
		t.Error("c ran after b failed")
	}
	var states []migrate.State
	for _, rep := range r.Status(context.Background()) {
		states = append(states, rep.Status.State)
	}
	if want := []migrate.State{migrate.UpToDate, migrate.Pending, migrate.Pending}; !equal(states, want) {
		t.Errorf("states %v, want %v", states, want)
	}
	if rep, err := r.ApplyOne(context.Background(), "c"); err != nil || rep.Outcome != migrate.Applied {
		t.Errorf("applying c alone: %+v, %v", rep, err)
	}
	if _, err := r.ApplyOne(context.Background(), "nope"); !hasCode(err, migrate.CodeUnknownMigrator) {
		t.Errorf("applying an unknown migrator: %v", err)
	}
}

func equal[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPlanSaysWhenAMigratorCantPlan(t *testing.T) {
	d := newDB()
	r := newRunner(t, newLocker(), migrate.Policy{MaxRisk: migrate.Safe}, &fake{db: d, name: "core", steps: twoSafe},
		full{planner{&fake{db: d, name: "all", steps: []migrate.StepInfo{{ID: "1_x", Risk: migrate.Caution}}}}})
	plans := r.Plan(context.Background(), false)
	if !hasCode(plans[0].Err, migrate.CodeNotSupported) || plans[0].Err.Error() != `migrate.not_supported: plan: migrator "core" can't plan` {
		t.Errorf("core: %v", plans[0].Err)
	}
	if plans[1].Err != nil || len(plans[1].Plan.Steps()) != 1 || !hasCode(plans[1].Refused, migrate.CodeRiskRefused) {
		t.Errorf("all: %+v", plans[1])
	}
	offline := r.Plan(context.Background(), true)
	if offline[0].Err == nil || !strings.Contains(offline[0].Err.Error(), `plan offline: migrator "core" can't plan without a database`) || offline[1].Err != nil {
		t.Errorf("offline: %+v", offline)
	}
	var b strings.Builder
	if err := migrate.WritePlans(&b, plans); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`core: migrate.not_supported: plan: migrator "core" can't plan`, "all: 1 step(s), max risk caution", "1_x [caution]", "refused:"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("plan output lacks %q:\n%s", want, b.String())
		}
	}
}

func TestStatusReportsDriftBackgroundTasksAndMissingCapabilities(t *testing.T) {
	d := newDB()
	r := newRunner(t, newLocker(), migrate.ProductionPolicy(), &fake{db: d, name: "core", steps: twoSafe},
		full{planner{&fake{db: d, name: "all", steps: twoSafe}}})
	reps := r.Status(context.Background())
	if reps[0].Drift != nil || reps[0].Background != nil || len(reps[0].Capabilities) != 0 {
		t.Errorf("core: %+v", reps[0])
	}
	if !hasCode(reps[0].Ready, migrate.CodeNotReady) {
		t.Errorf("a pending migrator is ready: %v", reps[0].Ready)
	}
	all := reps[1]
	if all.Drift == nil || all.Drift.None() || len(all.Background) != 2 || !all.Background[0].Known || all.Background[0].Done || !all.Background[1].Done {
		t.Errorf("all: %+v", all)
	}
	var b strings.Builder
	if err := migrate.WriteStatus(&b, reps); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"core: pending (database none, build none), not ready",
		"capabilities: none",
		`verify: migrator "core" can't verify`,
		"capabilities: plan, plan_offline, verify, background, completion",
		"drift: x was edited",
		"background backfill: pending",
		"background reindex: done",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("status output lacks %q:\n%s", want, b.String())
		}
	}
	if done, err := r.Done(context.Background(), "all", "reindex"); err != nil || !done {
		t.Errorf("Done: %v, %v", done, err)
	}
	if _, err := r.Done(context.Background(), "core", "x"); !hasCode(err, migrate.CodeNotSupported) || errs.KindOf(err) != errs.Unimplemented {
		t.Errorf("Done without Completion: %v", err)
	}
	if _, err := r.Done(context.Background(), "nope", "x"); !hasCode(err, migrate.CodeUnknownMigrator) {
		t.Errorf("Done of an unknown migrator: %v", err)
	}
}

func TestEveryApplyAndStepIsASpanALogAndMetrics(t *testing.T) {
	for _, debug := range []bool{false, true} {
		d := newDB()
		tp := &recorder{}
		mt := &meter{seen: map[string]float64{}}
		p := dev
		p.Debug = debug
		r, err := migrate.NewRunner(migrate.Config{Locker: newLocker(), Policy: p, TracerProvider: tp, MeterProvider: meterProvider{m: mt},
			Migrators: []migrate.Migrator{planner{&fake{db: d, name: "schema", steps: twoSafe}}, &fake{db: d, name: "broken", steps: twoSafe, fail: fmt.Errorf("applying 2_b: %w", sqlError{})}}})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = r.Apply(context.Background())
		applies := tp.named("migrate.apply")
		if len(applies) != 2 {
			t.Fatalf("%d apply spans", len(applies))
		}
		if v, _ := applies[0].attr("galleon.migrate.migrator"); v != "schema" {
			t.Errorf("the apply span names %q", v)
		}
		v, _ := applies[1].attr("galleon.migrate.outcome")
		quotes := strings.Contains(applies[1].status, "DROP TABLE payroll")
		if v != "failed" || quotes != debug || (!debug && !strings.Contains(applies[1].status, "SQLSTATE 42601")) {
			t.Errorf("with debug %v, the failed apply's span: %q, %q", debug, v, applies[1].status)
		}
		steps := tp.named("migrate.step")
		if len(steps) != 2 {
			t.Fatalf("%d step spans", len(steps))
		}
		for i, s := range steps {
			id, _ := s.attr("galleon.migrate.step")
			risk, _ := s.attr("galleon.migrate.risk")
			if s.parent != applies[0] || id != twoSafe[i].ID || risk != "safe" || !s.ended {
				t.Errorf("step span %d: %s, %s, parent %v", i, id, risk, s.parent)
			}
			if desc, ok := s.attr("galleon.migrate.step.description"); ok != debug || (debug && desc != twoSafe[i].Description) {
				t.Errorf("with debug %v, the description is %q (%v)", debug, desc, ok)
			}
		}
		for key, want := range map[string]float64{
			"galleon.migrate.applies galleon.migrate.migrator=schema galleon.migrate.outcome=applied":                                             1,
			"galleon.migrate.applies galleon.migrate.migrator=broken galleon.migrate.outcome=failed":                                              1,
			"galleon.migrate.failures galleon.migrate.error.code=galleon.internal galleon.migrate.migrator=broken galleon.migrate.outcome=failed": 1,
			"galleon.migrate.apply.duration galleon.migrate.migrator=schema galleon.migrate.outcome=applied":                                      1,
			"galleon.migrate.steps galleon.migrate.migrator=schema galleon.migrate.risk=safe galleon.migrate.step.result=applied":                 2,
		} {
			if got := mt.get(key); got != want {
				t.Errorf("%s = %v, want %v; have %v", key, got, want, mt.seen)
			}
		}
	}
}

func TestAZeroMaxRiskMeansSafe(t *testing.T) {
	if migrate.Allowed(migrate.Caution, migrate.Unknown, true) || !migrate.Allowed(migrate.Safe, migrate.Unknown, false) {
		t.Fatal("a zero MaxRisk isn't Safe")
	}
	d := newDB()
	drop := []migrate.StepInfo{{ID: "1_drop", Risk: migrate.Destructive}}
	reps, err := newRunner(t, newLocker(), migrate.Policy{Mode: migrate.ModeApply}, planner{&fake{db: d, name: "schema", steps: drop}}).Apply(context.Background())
	if reps[0].Outcome != migrate.Refused || !hasCode(err, migrate.CodeRiskRefused) || !strings.Contains(err.Error(), "the maximum risk is safe") {
		t.Fatalf("%+v, %v", reps[0], err)
	}
}

func TestAPinnedMigratorWithNothingPendingHasNoMismatch(t *testing.T) {
	d := newDB()
	m := planner{&fake{db: d, name: "schema", steps: twoSafe}}
	d.applied["schema"] = len(twoSafe)
	p := dev
	p.Approved = map[string]string{"schema": "an-old-plan"}
	plans := newRunner(t, newLocker(), p, m).Plan(context.Background(), false)
	if plans[0].Err != nil || plans[0].Refused != nil {
		t.Fatalf("%+v", plans[0])
	}
}

// emptyPlanner plans nothing, whatever its status says.
type emptyPlanner struct{ *fake }

func (emptyPlanner) Plan(context.Context) (migrate.Plan, error) { return migrate.NewPlan("", nil), nil }

func TestAPinnedMigratorThatIsPendingWithAnEmptyPlanIsRefused(t *testing.T) {
	d := newDB()
	m := &fake{db: d, name: "schema", steps: twoSafe}
	p := dev
	p.Approved = map[string]string{"schema": "a-reviewed-plan"}
	reps, err := newRunner(t, newLocker(), p, emptyPlanner{m}).Apply(context.Background())
	if reps[0].Outcome != migrate.Refused || !hasCode(err, migrate.CodeFingerprintMismatch) {
		t.Fatalf("%+v, %v", reps[0], err)
	}
	if d.applied["schema"] != 0 {
		t.Error("the unplanned changes were applied")
	}
}

func TestALockTTLTooShortToRenewIsRejected(t *testing.T) {
	for _, ttl := range []time.Duration{-time.Second, time.Millisecond, migrate.MinLockTTL - 1} {
		p := dev
		p.LockTTL = ttl
		_, err := migrate.NewRunner(migrate.Config{Locker: newLocker(), Policy: p})
		if want := fmt.Sprintf("must be 0 (%s) or at least %s", migrate.DefaultLockTTL, migrate.MinLockTTL); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LockTTL %s: %v", ttl, err)
		}
	}
	p := dev
	p.LockTTL = migrate.MinLockTTL
	if _, err := migrate.NewRunner(migrate.Config{Locker: newLocker(), Policy: p}); err != nil {
		t.Errorf("LockTTL %s: %v", p.LockTTL, err)
	}
}

func TestThePlanOfAPendingMigratorWithAnEmptyPlanIsOfUnknownRisk(t *testing.T) {
	d := newDB()
	m := emptyPlanner{&fake{db: d, name: "schema", steps: twoSafe}}
	p := dev
	p.AllowUnknown = false
	plans := newRunner(t, newLocker(), p, m).Plan(context.Background(), false)
	if plans[0].Err != nil || plans[0].MaxRisk() != migrate.Unknown || !hasCode(plans[0].Refused, migrate.CodeRiskRefused) {
		t.Fatalf("without allow_unknown: %+v", plans[0])
	}
	var b strings.Builder
	if err := migrate.WritePlans(&b, plans); err != nil || !strings.Contains(b.String(), "max risk unknown") {
		t.Errorf("%q, %v", b.String(), err)
	}
	p.AllowUnknown = true
	if plans := newRunner(t, newLocker(), p, m).Plan(context.Background(), false); plans[0].Refused != nil {
		t.Errorf("with allow_unknown: %+v", plans[0])
	}
	// Up to date, an empty plan is just nothing to do.
	d.applied["schema"] = len(twoSafe)
	p.AllowUnknown = false
	if plans := newRunner(t, newLocker(), p, m).Plan(context.Background(), false); plans[0].Refused != nil || plans[0].MaxRisk() != migrate.Safe {
		t.Errorf("up to date: %+v", plans[0])
	}
}

func TestAPendingMigratorWithAnEmptyPlanIsOfUnknownRisk(t *testing.T) {
	d := newDB()
	m := &fake{db: d, name: "schema", steps: twoSafe}
	p := dev
	p.AllowUnknown = false
	reps, err := newRunner(t, newLocker(), p, emptyPlanner{m}).Apply(context.Background())
	if reps[0].Outcome != migrate.Refused || !hasCode(err, migrate.CodeRiskRefused) || d.applied["schema"] != 0 {
		t.Fatalf("without allow_unknown: %+v, %v", reps[0], err)
	}
	p.AllowUnknown = true
	tp := &recorder{}
	r, err := migrate.NewRunner(migrate.Config{Locker: newLocker(), Policy: p, TracerProvider: tp, Migrators: []migrate.Migrator{emptyPlanner{m}}})
	if err != nil {
		t.Fatal(err)
	}
	reps, err = r.Apply(context.Background())
	if v, _ := tp.named("migrate.apply")[0].attr("galleon.migrate.risk"); v != "unknown" {
		t.Errorf("the apply span's risk is %q", v)
	}
	if err != nil || reps[0].Outcome != migrate.Applied {
		t.Fatalf("with allow_unknown: %+v, %v", reps[0], err)
	}
	if len(reps[0].Notes) != 1 || !strings.Contains(reps[0].Notes[0], "unknown risk") {
		t.Errorf("notes %v", reps[0].Notes)
	}
}

type applied struct {
	reps []migrate.ApplyReport
	err  error
}

// waiting starts an apply of m that waits for another node's lock, on a
// fake clock with a LockTTL of 2s (renewals every 500ms, a deadline 1.5s
// after one starts). The other node holds the lock for longer than that
// deadline, records m's changes, and releases it.
func waiting(t *testing.T, l *locker, m *fake) (*clock.Fake, <-chan error, <-chan applied) {
	t.Helper()
	return waitingFor(t, l, m, 0, 5*time.Second, nil)
}

// waitingFor is waiting with a LockWait, where the other node holds the
// lock for hold on the fake clock.
// When sleeping is set, it gets each pause the Runner arms a timer for.
func waitingFor(t *testing.T, l *locker, m *fake, lockWait, hold time.Duration, sleeping chan<- time.Duration) (*clock.Fake, <-chan error, <-chan applied) {
	t.Helper()
	l.realTTL = time.Hour
	other, err := l.Locker.Acquire(context.Background(), migrate.LockKey(m.name), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fc := clock.NewFake(time.Unix(0, 0))
	p := dev
	p.LockTTL = 2 * time.Second
	p.LockWait = lockWait
	r, err := migrate.NewRunner(migrate.Config{Locker: l, Migrators: []migrate.Migrator{m}, Policy: p, Clock: fc})
	if err != nil {
		t.Fatal(err)
	}
	renewed := make(chan error, 16)
	migrate.OnRenewed(r, func(err error) { renewed <- err })
	if sleeping != nil {
		migrate.OnSleeping(r, func(d time.Duration) { sleeping <- d })
	}
	done := make(chan applied, 1)
	go func() {
		reps, err := r.Apply(context.Background())
		done <- applied{reps, err}
	}()
	until(t, "the runner waits for the lock", func() bool { return l.waiting() == 1 })
	fc.Advance(hold)
	m.db.mu.Lock()
	m.db.applied[m.name] = len(m.steps)
	m.db.mu.Unlock()
	if err := other.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	return fc, renewed, done
}

func TestAWaitLongerThanTheSafetyDeadlineStillSeesTheOtherNodesWork(t *testing.T) {
	l := newLocker()
	_, _, done := waiting(t, l, &fake{db: newDB(), name: "schema", steps: twoSafe})
	got := within(t, "the apply", done)
	if got.err != nil || got.reps[0].Outcome != migrate.AlreadyUpToDate {
		t.Fatalf("%+v, %v", got.reps, got.err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.renewals != 1 {
		t.Errorf("renewed %d times after the wait, not once", l.renewals)
	}
}

// blocked waits until fc has n timers, failing the test after a long
// deadline rather than hanging.
func blocked(t *testing.T, fc *clock.Fake, n int) {
	t.Helper()
	ok := make(chan struct{})
	go func() { fc.BlockUntil(n); close(ok) }()
	within(t, fmt.Sprintf("%d timers", n), ok)
}

func TestALeaseLostAsSoonAsTheWaitEndsIsAskedForAgainAfterAnInterval(t *testing.T) {
	l := newLocker()
	l.renew = func(context.Context) error {
		if l.renewals == 1 { // this is the first renewal
			return coord.ErrLeaseLost
		}
		return nil
	}
	sleeping := make(chan time.Duration, 4)
	fc, _, done := waitingFor(t, l, &fake{db: newDB(), name: "schema", steps: twoSafe}, 0, 5*time.Second, sleeping)
	if d := within(t, "the pause before asking again", sleeping); d != 500*time.Millisecond {
		t.Fatalf("paused %s, not one renewal interval", d)
	}
	// The pause's timer is armed: until it is due, the lock isn't asked
	// for again.
	fc.Advance(499 * time.Millisecond)
	select {
	case got := <-done:
		t.Fatalf("finished during the pause: %+v, %v", got.reps, got.err)
	default:
	}
	if n := l.waiting(); n != 1 {
		t.Fatalf("asked for the lock again during the pause (%d asks)", n)
	}
	fc.Advance(time.Millisecond)
	got := within(t, "the apply", done)
	if got.err != nil || got.reps[0].Outcome != migrate.AlreadyUpToDate {
		t.Fatalf("%+v, %v", got.reps, got.err)
	}
	if n := l.waiting(); n != 2 {
		t.Errorf("asked for the lock %d times, not twice", n)
	}
}

func TestRetryingTheRenewalAfterTheWaitStopsAtTheLockWait(t *testing.T) {
	l := newLocker()
	l.renewed = errors.New("timeout")
	// LockWait is 1s, and the safety deadline 1.5s after the renewal.
	fc, renewed, done := waitingFor(t, l, &fake{db: newDB(), name: "schema", steps: twoSafe}, time.Second, 0, nil)
	within(t, "the first renewal", renewed)
	blocked(t, fc, 3) // LockWait, the safety deadline and the retry
	fc.Advance(500 * time.Millisecond)
	within(t, "the second renewal", renewed)
	blocked(t, fc, 3)
	fc.Advance(500 * time.Millisecond)
	got := within(t, "the apply", done)
	if got.reps[0].Outcome != migrate.Failed || !strings.Contains(got.err.Error(), "renewing the lease it was granted after waiting: lock_wait (1s) passed") || !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("%+v, %v", got.reps, got.err)
	}
}

func TestAFailedRenewalAfterTheWaitIsRetriedUntilTheDeadline(t *testing.T) {
	l := newLocker()
	l.flaky = 1
	fc, renewed, done := waiting(t, l, &fake{db: newDB(), name: "schema", steps: twoSafe})
	if err := within(t, "the first renewal", renewed); err == nil {
		t.Fatal("the flaky renewal succeeded")
	}
	fc.BlockUntil(2) // the deadline and the retry's timer
	fc.Advance(500 * time.Millisecond)
	got := within(t, "the apply", done)
	if got.err != nil || got.reps[0].Outcome != migrate.AlreadyUpToDate {
		t.Fatalf("%+v, %v", got.reps, got.err)
	}
}

func TestRenewalsAfterTheWaitThatKeepFailingGiveUpAtTheDeadline(t *testing.T) {
	l := newLocker()
	l.renewed = errors.New("timeout")
	fc, renewed, done := waiting(t, l, &fake{db: newDB(), name: "schema", steps: twoSafe})
	within(t, "the first renewal", renewed)
	fc.BlockUntil(2)
	fc.Advance(1500 * time.Millisecond)
	got := within(t, "the apply", done)
	if got.reps[0].Outcome != migrate.Failed || !hasCode(got.err, migrate.CodeLeaseLost) || !strings.Contains(got.err.Error(), "timeout") {
		t.Fatalf("%+v, %v", got.reps, got.err)
	}
}

func TestStoppingAfterAnApplyDoesntWaitForARenewalThatHangs(t *testing.T) {
	l := newLocker()
	hanging := make(chan struct{})
	l.renew = func(ctx context.Context) error {
		close(hanging)
		<-ctx.Done()
		return ctx.Err()
	}
	m := &fake{db: newDB(), name: "schema", steps: twoSafe, block: make(chan struct{}), started: make(chan struct{})}
	fc, _, done := held(t, l, m)
	fc.Advance(500 * time.Millisecond)
	within(t, "the renewal to start", hanging)
	close(m.block) // the apply succeeds while the renewal hangs
	if err := within(t, "the apply", done); err != nil {
		t.Fatalf("%v", err)
	}
}

// held starts an apply of m that holds its lease on a fake clock, with a
// LockTTL of 2s: the Runner renews every 500ms and gives up 1.5s after the
// last renewal began. It returns the clock, the renewals the Runner has
// handled, and the apply's result.
func held(t *testing.T, l *locker, m *fake) (*clock.Fake, <-chan error, <-chan error) {
	t.Helper()
	l.realTTL = time.Hour
	fc := clock.NewFake(time.Unix(0, 0))
	p := dev
	p.LockTTL = 2 * time.Second
	r, err := migrate.NewRunner(migrate.Config{Locker: l, Migrators: []migrate.Migrator{m}, Policy: p, Clock: fc})
	if err != nil {
		t.Fatal(err)
	}
	renewed := make(chan error, 16)
	migrate.OnRenewed(r, func(err error) { renewed <- err })
	done := make(chan error, 1)
	go func() {
		_, err := r.Apply(context.Background())
		done <- err
	}()
	select {
	case <-m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the apply never started")
	}
	return fc, renewed, done
}

// within waits for a value from c, failing the test after a long deadline.
func within[T any](t *testing.T, what string, c <-chan T) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func TestOneFailedRenewalIsRetriedUntilTheDeadline(t *testing.T) {
	l := newLocker()
	l.flaky = 1
	m := &fake{db: newDB(), name: "schema", steps: twoSafe, block: make(chan struct{}), started: make(chan struct{})}
	fc, renewed, done := held(t, l, m)
	fc.Advance(500 * time.Millisecond)
	if err := within(t, "the first renewal", renewed); err == nil {
		t.Fatal("the flaky renewal succeeded")
	}
	fc.Advance(500 * time.Millisecond)
	if err := within(t, "the second renewal", renewed); err != nil {
		t.Fatalf("the second renewal failed: %v", err)
	}
	// Past the deadline the grant set, but well inside the one the good
	// renewal set.
	fc.Advance(600 * time.Millisecond)
	close(m.block)
	if err := within(t, "the apply", done); err != nil {
		t.Fatalf("one failed renewal cancelled the apply: %v", err)
	}
}

func TestARenewalThatSaysTheLeaseIsLostCancelsTheApplyAtOnce(t *testing.T) {
	l := newLocker()
	// The backend says the lease is gone, but Lost never closes.
	l.renew = func(context.Context) error { return coord.ErrLeaseLost }
	m := &fake{db: newDB(), name: "schema", steps: twoSafe, block: make(chan struct{}), started: make(chan struct{})}
	fc, _, done := held(t, l, m)
	fc.Advance(500 * time.Millisecond) // one renewal, far from the deadline
	if err := within(t, "the apply", done); !hasCode(err, migrate.CodeLeaseLost) {
		t.Fatalf("got %v, want %s", err, migrate.CodeLeaseLost)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.renewals != 1 {
		t.Errorf("renewed %d times after the lease was lost", l.renewals)
	}
}

func TestRenewalsThatKeepFailingCancelTheApplyBeforeTheLeaseExpires(t *testing.T) {
	l := newLocker()
	l.renewed = errors.New("timeout")
	m := &fake{db: newDB(), name: "schema", steps: twoSafe, block: make(chan struct{}), started: make(chan struct{})}
	fc, renewed, done := held(t, l, m)
	for range 2 {
		fc.Advance(500 * time.Millisecond)
		within(t, "a failed renewal", renewed)
	}
	fc.Advance(499 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("cancelled before the deadline: %v", err)
	default:
	}
	// 1.5s after the grant: a renewal interval short of the 2s TTL.
	fc.Advance(time.Millisecond)
	if err := within(t, "the apply", done); !hasCode(err, migrate.CodeLeaseLost) {
		t.Fatalf("got %v, want %s", err, migrate.CodeLeaseLost)
	}
}

func TestARenewalThatHangsIsCutOffAtTheDeadline(t *testing.T) {
	l := newLocker()
	hanging := make(chan struct{})
	returned := make(chan error, 1)
	l.renew = func(ctx context.Context) error {
		close(hanging)
		<-ctx.Done()
		returned <- ctx.Err()
		return ctx.Err()
	}
	m := &fake{db: newDB(), name: "schema", steps: twoSafe, block: make(chan struct{}), started: make(chan struct{})}
	fc, _, done := held(t, l, m)
	fc.Advance(500 * time.Millisecond)
	within(t, "the renewal to start", hanging)
	fc.Advance(time.Second) // the deadline, 1.5s after the grant
	if err := within(t, "the apply", done); !hasCode(err, migrate.CodeLeaseLost) {
		t.Fatalf("got %v, want %s", err, migrate.CodeLeaseLost)
	}
	if err := within(t, "the renewal to return", returned); err == nil {
		t.Fatal("the renewal returned without its context ending")
	}
}

func TestLosingTheLeaseAfterApplyCommittedReportsApplied(t *testing.T) {
	d := newDB()
	l := newLocker()
	m := &fake{db: d, name: "schema", steps: twoSafe}
	// The lease is lost once the changes are in: Apply sees its ctx end
	// only after it has nothing left to undo.
	m.afterApply = func(ctx context.Context) {
		l.last().lose()
		<-ctx.Done()
	}
	reps, err := newRunner(t, l, dev, planner{m}).Apply(context.Background())
	if err != nil || reps[0].Outcome != migrate.Applied {
		t.Fatalf("%+v, %v", reps[0], err)
	}
	if len(reps[0].Notes) != 1 || !strings.Contains(reps[0].Notes[0], "lost before Apply returned") {
		t.Errorf("notes %v", reps[0].Notes)
	}
}

func TestAPlanIsRefusedWhereApplyWouldRefuseTheDatabasesState(t *testing.T) {
	for state, refused := range map[migrate.State]bool{migrate.Drifted: true, migrate.Ahead: true, migrate.Pending: false} {
		m := planner{&fake{db: newDB(), name: "schema", steps: twoSafe, state: state}}
		plans := newRunner(t, newLocker(), dev, m).Plan(context.Background(), false)
		if got := plans[0].Refused != nil; got != refused || plans[0].Err != nil {
			t.Errorf("%s: %+v", state, plans[0])
		}
		if refused && !hasCode(plans[0].Refused, migrate.CodeNotReady) {
			t.Errorf("%s: refused with %v", state, plans[0].Refused)
		}
	}
	tolerant := dev
	tolerant.TolerateAhead = true
	m := planner{&fake{db: newDB(), name: "schema", steps: twoSafe, state: migrate.Ahead}}
	if plans := newRunner(t, newLocker(), tolerant, m).Plan(context.Background(), false); plans[0].Refused != nil {
		t.Errorf("Ahead where it is tolerated: %+v", plans[0])
	}
}

func TestAPlanWhoseStatusCantBeReadIsKeptWithTheError(t *testing.T) {
	m := planner{&fake{db: newDB(), name: "schema", steps: twoSafe, statErr: errBoom}}
	plans := newRunner(t, newLocker(), dev, m).Plan(context.Background(), false)
	if plans[0].Err != nil || plans[0].Plan == nil || len(plans[0].Plan.Steps()) != 2 || !errors.Is(plans[0].StatusErr, errBoom) {
		t.Fatalf("%+v", plans[0])
	}
	var b strings.Builder
	if err := migrate.WritePlans(&b, plans); err != nil || !strings.Contains(b.String(), "2 step(s)") || !strings.Contains(b.String(), "status unknown") {
		t.Errorf("%q, %v", b.String(), err)
	}
}

func TestWritePlansTakesAHandBuiltReportsRiskFromItsPlan(t *testing.T) {
	plan := migrate.NewPlan("fp", []migrate.StepInfo{{ID: "1", Risk: migrate.Caution}})
	var b strings.Builder
	if err := migrate.WritePlans(&b, []migrate.PlanReport{{Name: "schema", Plan: plan}}); err != nil || !strings.Contains(b.String(), "max risk caution") {
		t.Errorf("%q, %v", b.String(), err)
	}
}

func TestAnEmptyPlanWhoseStatusCantBeReadIsUnplanned(t *testing.T) {
	m := emptyPlanner{&fake{db: newDB(), name: "schema", steps: twoSafe, statErr: errBoom}}
	p := dev
	p.AllowUnknown = false
	plans := newRunner(t, newLocker(), p, m).Plan(context.Background(), false)
	if !plans[0].Unplanned || plans[0].MaxRisk() != migrate.Unknown || !hasCode(plans[0].Refused, migrate.CodeRiskRefused) || plans[0].StatusErr == nil {
		t.Fatalf("%+v", plans[0])
	}
}
