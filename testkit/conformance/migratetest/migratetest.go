// Package migratetest is the conformance suite for migrators, the
// migration seam's adapters. Every adapter runs it in its own tests:
//
//	func TestConformance(t *testing.T) {
//		migratetest.Run(t, migratetest.Factory{
//			Open: func(t *testing.T) func(migratetest.Build) migrate.Migrator {
//				db := freshDatabase(t)
//				return func(b migratetest.Build) migrate.Migrator { return mytool.New("schema", builds[b], db) }
//			},
//			Ahead: true, Drift: true, Fencing: true,
//		})
//	}
//
// It covers the core for every adapter, and each optional capability the
// adapter has: claiming one, by implementing its interface, is a suite to
// pass.
package migratetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/migrate"
)

// Build is one of the builds of the suite's schema the adapter provides.
type Build uint8

const (
	// Base has two changes, both of Safe risk where the adapter
	// classifies risk.
	Base Build = iota
	// Next is Base plus a third change, of Safe risk.
	Next
	// Risky is Base plus a third change of Destructive risk, which the
	// suite's policy refuses.
	Risky
	// Edited is Base with its first change's source edited.
	Edited
)

func (b Build) String() string {
	return [...]string{"Base", "Next", "Risky", "Edited"}[b]
}

// Factory makes the migrators under test.
type Factory struct {
	// Open returns a fresh, empty database for t, and a function that
	// returns a new migrator over it for a build. Every migrator from one
	// Open shares its database, as nodes do, and each call is a new
	// instance, as a new node or a restarted process would have. Every
	// migrator is named "suite".
	Open func(t *testing.T) func(Build) migrate.Migrator
	// Ahead and Drift claim that Status reports Ahead, for a database a
	// newer build migrated, and Drifted, for an edited change.
	Ahead, Drift bool
	// Fencing claims that Apply refuses a lease whose token is below one
	// it has already applied under.
	Fencing bool
}

// Run runs every conformance test that applies to the adapter. The numbers
// are those of the migration seam's conformance list (04, Round 3).
func Run(t *testing.T, f Factory) {
	t.Run("AnEmptyDatabaseIsPending", func(t *testing.T) { emptyIsPending(t, f) })
	t.Run("ApplyTwiceChangesNothingTheSecondTime", func(t *testing.T) { applyTwice(t, f) })
	t.Run("ConcurrentApplyUnderTheLockRunsExactlyOnce", func(t *testing.T) { concurrent(t, f) })
	t.Run("StatusIsUpToDateAfterApply", func(t *testing.T) { upToDateAfterApply(t, f) })
	t.Run("APlanAboveMaxRiskIsRefusedAndNothingApplied", func(t *testing.T) { riskRefused(t, f) })
	t.Run("ApplyNeedsALease", func(t *testing.T) { needsLease(t, f) })
	t.Run("AStaleLeaseCantApply", func(t *testing.T) {
		if !f.Fencing {
			t.Skip("the adapter doesn't claim fencing")
		}
		staleLease(t, f)
	})
	t.Run("AheadIsDetected", func(t *testing.T) {
		if !f.Ahead {
			t.Skip("the adapter doesn't claim to detect Ahead")
		}
		ahead(t, f)
	})
	t.Run("DriftIsDetected", func(t *testing.T) {
		if !f.Drift {
			t.Skip("the adapter doesn't claim to detect drift")
		}
		drift(t, f)
	})
	t.Run("Planner", func(t *testing.T) { planner(t, f) })
	t.Run("OfflinePlanner", func(t *testing.T) { offlinePlanner(t, f) })
	t.Run("Verifier", func(t *testing.T) { verifier(t, f) })
	t.Run("BackgroundTasksResumeFromASavedCursor", func(t *testing.T) { background(t, f) })
}

// Name is the name every migrator of the suite has.
const Name = "suite"

// policy is the suite's deploy policy: up to Caution, Unknown allowed so
// adapters that can't classify still apply Base.
var policy = migrate.Policy{MaxRisk: migrate.Caution, AllowUnknown: true, LockTTL: 10 * time.Second}

func open(t *testing.T, f Factory) func(Build) migrate.Migrator {
	t.Helper()
	build := f.Open(t)
	return func(b Build) migrate.Migrator {
		t.Helper()
		m := build(b)
		if m == nil {
			t.Fatalf("the factory returned no migrator for %s", b)
		}
		if m.Name() != Name {
			t.Fatalf("the migrator is named %q, not %q", m.Name(), Name)
		}
		return m
	}
}

func ctxFor(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func runner(t *testing.T, locker coord.Locker, p migrate.Policy, ms ...migrate.Migrator) *migrate.Runner {
	t.Helper()
	r, err := migrate.NewRunner(migrate.Config{Locker: locker, Migrators: ms, Policy: p})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func state(t *testing.T, ctx context.Context, m migrate.Migrator) migrate.State {
	t.Helper()
	st, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return st.State
}

func want(t *testing.T, ctx context.Context, m migrate.Migrator, s migrate.State, when string) {
	t.Helper()
	if got := state(t, ctx, m); got != s {
		t.Fatalf("%s, the status is %s, not %s", when, got, s)
	}
}

// apply applies m through a Runner, as the kernel would.
func apply(t *testing.T, ctx context.Context, m migrate.Migrator) migrate.ApplyReport {
	t.Helper()
	reps, err := runner(t, local.New().Locks(), policy, m).Apply(ctx)
	if err != nil {
		t.Fatalf("applying: %v", err)
	}
	return reps[0]
}

// lease is a lease with a chosen token, for calling Apply directly.
type lease struct {
	token uint64
	lost  chan struct{}
}

func newLease(token uint64) *lease { return &lease{token: token, lost: make(chan struct{})} }

func (l *lease) Key() string                   { return migrate.LockKey(Name) }
func (l *lease) Token() uint64                 { return l.token }
func (l *lease) Renew(context.Context) error   { return nil }
func (l *lease) Release(context.Context) error { return nil }
func (l *lease) Lost() <-chan struct{}         { return l.lost }

func emptyIsPending(t *testing.T, f Factory) {
	ctx := ctxFor(t)
	build := open(t, f)
	want(t, ctx, build(Base), migrate.Pending, "on an empty database")
}

func applyTwice(t *testing.T, f Factory) {
	ctx := ctxFor(t)
	build := open(t, f)
	m := build(Base)
	if rep := apply(t, ctx, m); rep.Outcome != migrate.Applied || len(rep.Result.Applied) == 0 {
		t.Fatalf("the first apply applied nothing: %+v", rep)
	}
	before, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Called directly, as the Runner never would on an up-to-date
	// database: the migrator itself must find nothing to do.
	res, err := build(Base).Apply(ctx, migrate.ApplyOptions{Lock: newLease(1 << 40), MaxRisk: migrate.Caution, AllowUnknown: true})
	if err != nil {
		t.Fatalf("applying again: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Errorf("applying again applied %v", res.Applied)
	}
	after, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("applying again changed the status from %+v to %+v", before, after)
	}
	if rep := apply(t, ctx, build(Base)); rep.Outcome != migrate.AlreadyUpToDate {
		t.Errorf("the Runner applied again: %+v", rep)
	}
}

// counting counts the calls to Apply.
type counting struct {
	migrate.Migrator
	mu    *sync.Mutex
	calls *int
}

func (c counting) Apply(ctx context.Context, opts migrate.ApplyOptions) (migrate.Result, error) {
	c.mu.Lock()
	*c.calls++
	c.mu.Unlock()
	return c.Migrator.Apply(ctx, opts)
}

func concurrent(t *testing.T, f Factory) {
	ctx := ctxFor(t)
	build := open(t, f)
	locks := local.New().Locks()
	const nodes = 4
	var mu sync.Mutex
	calls := 0
	start := make(chan struct{})
	reps := make([]migrate.ApplyReport, nodes)
	fails := make([]error, nodes)
	var wg sync.WaitGroup
	for i := range nodes {
		r := runner(t, locks, policy, counting{Migrator: build(Base), mu: &mu, calls: &calls})
		wg.Go(func() {
			<-start
			var out []migrate.ApplyReport
			out, fails[i] = r.Apply(ctx)
			reps[i] = out[0]
		})
	}
	close(start)
	wg.Wait()
	applied := 0
	for i, rep := range reps {
		if fails[i] != nil {
			t.Fatalf("node %d: %v", i, fails[i])
		}
		switch rep.Outcome {
		case migrate.Applied:
			applied++
		case migrate.AlreadyUpToDate:
		default:
			t.Errorf("node %d: %s", i, rep.Outcome)
		}
	}
	if applied != 1 || calls != 1 {
		t.Fatalf("%d nodes applied, and Apply ran %d times: want exactly one", applied, calls)
	}
	want(t, ctx, build(Base), migrate.UpToDate, "after every node applied")
}

func upToDateAfterApply(t *testing.T, f Factory) {
	ctx := ctxFor(t)
	build := open(t, f)
	apply(t, ctx, build(Base))
	want(t, ctx, build(Base), migrate.UpToDate, "after applying")
	want(t, ctx, build(Next), migrate.Pending, "with a third change in the build")
	apply(t, ctx, build(Next))
	want(t, ctx, build(Next), migrate.UpToDate, "after applying the third change")
}

func riskRefused(t *testing.T, f Factory) {
	ctx := ctxFor(t)
	build := open(t, f)
	m := build(Risky)
	p := policy
	if _, ok := m.(migrate.Planner); !ok {
		// A migrator that can't plan is of Unknown risk throughout.
		p.AllowUnknown = false
	}
	reps, err := runner(t, local.New().Locks(), p, m).Apply(ctx)
	if err == nil || reps[0].Outcome != migrate.Refused {
		t.Fatalf("the Runner didn't refuse the risky build: %+v, %v", reps[0], err)
	}
	want(t, ctx, build(Base), migrate.Pending, "after the Runner refused")
	if _, ok := m.(migrate.Planner); !ok {
		return
	}
	// The migrator enforces MaxRisk itself too.
	res, err := build(Risky).Apply(ctx, migrate.ApplyOptions{Lock: newLease(1 << 40), MaxRisk: migrate.Caution, AllowUnknown: true})
	if err == nil {
		t.Fatalf("Apply ran a Destructive step under MaxRisk Caution")
	}
	if len(res.Applied) != 0 {
		t.Errorf("Apply refused, but applied %v", res.Applied)
	}
	want(t, ctx, build(Base), migrate.Pending, "after the migrator refused")
}

func needsLease(t *testing.T, f Factory) {
	ctx := ctxFor(t)
	build := open(t, f)
	if _, err := build(Base).Apply(ctx, migrate.ApplyOptions{MaxRisk: migrate.Manual, AllowUnknown: true}); err == nil {
		t.Fatal("Apply ran without a lease")
	}
	want(t, ctx, build(Base), migrate.Pending, "after Apply without a lease")
}

func staleLease(t *testing.T, f Factory) {
	ctx := ctxFor(t)
	build := open(t, f)
	all := migrate.ApplyOptions{MaxRisk: migrate.Manual, AllowUnknown: true}
	all.Lock = newLease(10)
	if _, err := build(Base).Apply(ctx, all); err != nil {
		t.Fatal(err)
	}
	// A node that held token 5 and paused past its lease resumes.
	all.Lock = newLease(5)
	if _, err := build(Next).Apply(ctx, all); err == nil {
		t.Fatal("a lease with a lower token applied")
	}
	want(t, ctx, build(Next), migrate.Pending, "after the stale holder tried")
	all.Lock = newLease(11)
	if _, err := build(Next).Apply(ctx, all); err != nil {
		t.Fatalf("the next lease can't apply: %v", err)
	}
	want(t, ctx, build(Next), migrate.UpToDate, "after the next lease applied")
}

func ahead(t *testing.T, f Factory) {
	ctx := ctxFor(t)
	build := open(t, f)
	apply(t, ctx, build(Next))
	want(t, ctx, build(Base), migrate.Ahead, "with a newer build applied")
	tolerant := policy
	tolerant.TolerateAhead = true
	reps, err := runner(t, local.New().Locks(), tolerant, build(Base)).Apply(ctx)
	if err != nil || reps[0].Outcome != migrate.AheadTolerated {
		t.Errorf("applying the older build where Ahead is tolerated: %+v, %v", reps[0], err)
	}
	if _, err := runner(t, local.New().Locks(), policy, build(Base)).Apply(ctx); err == nil {
		t.Error("applying the older build where Ahead isn't tolerated succeeded")
	}
	want(t, ctx, build(Next), migrate.UpToDate, "after the older build tried")
}

func drift(t *testing.T, f Factory) {
	ctx := ctxFor(t)
	build := open(t, f)
	apply(t, ctx, build(Base))
	want(t, ctx, build(Edited), migrate.Drifted, "with an applied change edited")
	if _, err := runner(t, local.New().Locks(), policy, build(Edited)).Apply(ctx); err == nil {
		t.Error("applying a drifted build succeeded")
	}
}

func planner(t *testing.T, f Factory) {
	ctx := ctxFor(t)
	build := open(t, f)
	m := build(Base)
	p, ok := m.(migrate.Planner)
	if !ok {
		t.Skip("the adapter isn't a Planner")
	}
	pl, err := p.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Steps()) != 2 || pl.Fingerprint() == "" {
		t.Fatalf("the plan of Base on an empty database has %d steps and fingerprint %q", len(pl.Steps()), pl.Fingerprint())
	}
	if pl.MaxRisk() != migrate.MaxRisk(pl.Steps()) {
		t.Errorf("the plan's MaxRisk %s isn't its riskiest step's", pl.MaxRisk())
	}
	again, err := build(Base).(migrate.Planner).Plan(ctx)
	if err != nil || again.Fingerprint() != pl.Fingerprint() {
		t.Fatalf("a second node's plan has another fingerprint: %v, %v", again, err)
	}
	risky, err := build(Risky).(migrate.Planner).Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if risky.MaxRisk() != migrate.Destructive || risky.Fingerprint() == pl.Fingerprint() {
		t.Errorf("Risky's plan has max risk %s and fingerprint %q", risky.MaxRisk(), risky.Fingerprint())
	}

	// A plan other than the approved one is refused, and nothing applied.
	opts := migrate.ApplyOptions{Lock: newLease(1), MaxRisk: migrate.Manual, AllowUnknown: true, Fingerprint: risky.Fingerprint()}
	if _, err := build(Base).Apply(ctx, opts); err == nil {
		t.Fatal("Apply applied a plan other than the approved one")
	}
	want(t, ctx, m, migrate.Pending, "after refusing an unapproved plan")
	pinned := policy
	pinned.Approved = map[string]string{Name: risky.Fingerprint()}
	reps, err := runner(t, local.New().Locks(), pinned, build(Base)).Apply(ctx)
	if !errors.Is(err, errs.New(errs.FailedPrecondition, migrate.CodeFingerprintMismatch, "")) || reps[0].Outcome != migrate.Refused {
		t.Fatalf("the Runner applied a plan other than the pinned one: %+v, %v", reps[0], err)
	}
	pinned.Approved[Name] = pl.Fingerprint()
	if _, err := runner(t, local.New().Locks(), pinned, build(Base)).Apply(ctx); err != nil {
		t.Fatalf("the pinned plan doesn't apply: %v", err)
	}
	done, err := m.(migrate.Planner).Plan(ctx)
	if err != nil || len(done.Steps()) != 0 {
		t.Errorf("the plan after applying has steps: %v, %v", done, err)
	}
}

func offlinePlanner(t *testing.T, f Factory) {
	ctx := ctxFor(t)
	build := open(t, f)
	p, ok := build(Base).(migrate.OfflinePlanner)
	if !ok {
		t.Skip("the adapter isn't an OfflinePlanner")
	}
	before, err := p.PlanOffline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Steps()) != 2 || before.Fingerprint() == "" {
		t.Fatalf("the offline plan of Base has %d steps and fingerprint %q", len(before.Steps()), before.Fingerprint())
	}
	apply(t, ctx, build(Base))
	after, err := build(Base).(migrate.OfflinePlanner).PlanOffline(ctx)
	if err != nil || after.Fingerprint() != before.Fingerprint() {
		t.Errorf("the offline plan depends on the database: %v, %v", after, err)
	}
}

func verifier(t *testing.T, f Factory) {
	ctx := ctxFor(t)
	build := open(t, f)
	v, ok := build(Base).(migrate.Verifier)
	if !ok {
		t.Skip("the adapter isn't a Verifier")
	}
	apply(t, ctx, build(Base))
	d, err := v.Verify(ctx)
	if err != nil || !d.None() {
		t.Fatalf("Verify after applying the build found %v, %v", d.Differences, err)
	}
	if !f.Drift {
		return
	}
	d, err = build(Edited).(migrate.Verifier).Verify(ctx)
	if err != nil || d.None() {
		t.Fatalf("Verify of an edited build found no drift: %v", err)
	}
}

func background(t *testing.T, f Factory) {
	ctx := ctxFor(t)
	build := open(t, f)
	if _, ok := build(Base).(migrate.Backgrounder); !ok {
		t.Skip("the adapter isn't a Backgrounder")
	}
	apply(t, ctx, build(Base))
	tasks, err := build(Base).(migrate.Backgrounder).BackgroundTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) == 0 {
		t.Skip("Base has no background tasks")
	}
	const most = 10_000
	for _, task := range tasks {
		// One step, then the node is interrupted: its cursor was saved.
		saved, done, err := task.Step(ctx, nil)
		if err != nil {
			t.Fatalf("task %s: %v", task.ID(), err)
		}
		// Another node finds the task by ID, repeats the step the first
		// node may not have finished saving, then carries on.
		var resumed migrate.BackgroundTask
		others, err := build(Base).(migrate.Backgrounder).BackgroundTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range others {
			if o.ID() == task.ID() {
				resumed = o
			}
		}
		if resumed == nil {
			t.Fatalf("task %s isn't listed on another node", task.ID())
		}
		if _, _, err := resumed.Step(ctx, nil); err != nil {
			t.Fatalf("task %s: repeating a step: %v", task.ID(), err)
		}
		cursor := saved
		for n := 0; !done; n++ {
			if n == most {
				t.Fatalf("task %s isn't done after %d steps", task.ID(), most)
			}
			if cursor, done, err = resumed.Step(ctx, cursor); err != nil {
				t.Fatalf("task %s: %v", task.ID(), err)
			}
		}
		if c, ok := build(Base).(migrate.Completion); ok {
			finished, err := c.Done(ctx, task.ID())
			if err != nil || !finished {
				t.Errorf("Done(%q) after the task finished: %v, %v", task.ID(), finished, err)
			}
		}
	}
}
