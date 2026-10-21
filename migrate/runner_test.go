package migrate_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
	p.LockTTL = 30 * time.Millisecond
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
			Migrators: []migrate.Migrator{planner{&fake{db: d, name: "schema", steps: twoSafe}}, &fake{db: d, name: "broken", steps: twoSafe, fail: errBoom}}})
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
		if v, _ := applies[1].attr("galleon.migrate.outcome"); v != "failed" || !strings.Contains(applies[1].status, "boom") {
			t.Errorf("the failed apply's span: %q, %q", v, applies[1].status)
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
