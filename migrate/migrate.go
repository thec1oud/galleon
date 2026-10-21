// Package migrate is Galleon's migration seam: what Galleon expects from any
// migration tool (dpg, goose, golang-migrate, Atlas, the built-in
// pg/sqlmigrate, or one of your own).
//
// The seam is a small required core, Migrator, plus optional capabilities a
// tool implements only if it has them, discovered with a type assertion the
// way http.Flusher is: Planner, OfflinePlanner, Verifier, Backgrounder and
// Completion. A tool never fakes an ability it lacks.
//
// Around any migrator, the Runner provides the same behaviour: exactly-once
// apply across the cluster under the coordination lock, a deploy policy (the
// highest risk allowed, what Unknown risk means, and plan fingerprints CI
// approved), several migrators in a declared order, and a span, a log record
// and metrics for every apply and step. The kernel runs it at boot and turns
// each migrator's status into a readiness gate (see galleon.Migrator).
//
// The package doesn't depend on any database driver.
package migrate

import (
	"context"
	"fmt"

	"github.com/thec1oud/galleon/coord"
)

// Migrator is the required core every migration tool implements.
type Migrator interface {
	// Name identifies the migrator in logs, status output and the lock key.
	// It is unique per app, [a-z0-9_.-]+.
	Name() string
	// Status compares the database with what this build expects.
	Status(ctx context.Context) (Status, error)
	// Apply brings the database to what this build expects. It MUST NOT run
	// without opts.Lock, MUST stop when ctx ends, and SHOULD use the lease's
	// fencing token so that a holder whose lease has passed to someone else
	// can't apply anything.
	Apply(ctx context.Context, opts ApplyOptions) (Result, error)
}

// State is how the database compares with what this build expects.
type State uint8

const (
	// UpToDate: the database is exactly what this build expects.
	UpToDate State = iota + 1
	// Pending: the build has changes the database doesn't have yet.
	Pending
	// Drifted: the database differs from anything this build can produce,
	// such as an applied change whose source has since been edited.
	Drifted
	// Ahead: the database is newer than this build, such as during a
	// rolling deploy, when a newer build has already migrated it.
	Ahead
)

func (s State) String() string {
	switch s {
	case UpToDate:
		return "up_to_date"
	case Pending:
		return "pending"
	case Drifted:
		return "drifted"
	case Ahead:
		return "ahead"
	}
	return "unknown"
}

// Status is the result of Migrator.Status.
type Status struct {
	State State
	// Expected is the version or fingerprint this build expects.
	Expected string
	// Current is the version or fingerprint recorded in the database.
	Current string
	// Detail says more, for people: which changes are pending, what drifted.
	Detail string
}

// ApplyOptions is what the Runner passes to Apply.
type ApplyOptions struct {
	// Lock is the held cluster lease the apply runs under. Its Token is the
	// fencing token: a migrator that records the highest token it has seen
	// in the database, and refuses lower ones, can't be applied twice by a
	// node that paused past its lease. Lost closes if the lease ends; the
	// Runner then also cancels ctx.
	//
	// Part 4 sketched a Locker here. The Runner takes the lock itself, so the
	// lock is held before any migrator code runs, and passes the lease.
	Lock coord.Lease
	// MaxRisk is the riskiest step allowed. A migrator that can classify its
	// steps MUST refuse, before changing anything, a set of changes with a
	// step above it. Unknown steps are governed by AllowUnknown instead.
	// The zero value, Unknown, means Safe: options nobody filled in are the
	// most restrictive, never the most permissive.
	MaxRisk Risk
	// AllowUnknown says whether steps of Unknown risk may run.
	AllowUnknown bool
	// Fingerprint, when set, is the plan the Runner checked against the
	// policy, or that CI approved. A Planner MUST refuse to apply a plan
	// with another fingerprint, so Apply does exactly what was checked.
	Fingerprint string
	// Observer receives step events. It may be nil; use Step, which
	// handles that.
	Observer Observer
}

// Step reports the start of step s to the Observer, and returns the context
// the step runs in and the function to call with its outcome when it ends.
func (o ApplyOptions) Step(ctx context.Context, s StepInfo) (context.Context, func(err error)) {
	if o.Observer == nil {
		return ctx, func(error) {}
	}
	return o.Observer.Step(ctx, s)
}

// Allows reports whether these options let a step of risk r run.
func (o ApplyOptions) Allows(r Risk) bool { return Allowed(r, o.MaxRisk, o.AllowUnknown) }

// Allowed reports whether a step of risk r runs under max: Unknown only when
// allowUnknown is set, any other risk when it isn't above max. A max of
// Unknown, the zero value, means Safe.
func Allowed(r, max Risk, allowUnknown bool) bool {
	if r == Unknown {
		return allowUnknown
	}
	return !r.Above(Ceiling(max))
}

// Ceiling is the riskiest classified step max allows: max itself, or Safe
// for the zero value, Unknown.
func Ceiling(max Risk) Risk {
	if max == Unknown {
		return Safe
	}
	return max
}

// Observer receives the steps of an apply. The Runner's maps each step to a
// span, a log record and metrics.
type Observer interface {
	// Step is called as step s starts. The step runs with the returned ctx,
	// and calls done with its outcome when it ends.
	Step(ctx context.Context, s StepInfo) (_ context.Context, done func(err error))
}

// Result is what Apply did.
type Result struct {
	// Applied lists the steps applied, in order; it is empty when there was
	// nothing to do.
	Applied []StepInfo
	// Detail says more, for people.
	Detail string
}

// Planner is a migrator that can say what Apply would do, without doing it.
type Planner interface {
	Plan(ctx context.Context) (Plan, error)
}

// Plan is what Apply would do.
type Plan interface {
	Steps() []StepInfo
	// MaxRisk is the riskiest of Steps (Safe when there are none).
	MaxRisk() Risk
	// Fingerprint identifies exactly this plan, so CI can approve it and a
	// deploy can check that it applies what was approved.
	Fingerprint() string
}

// StepInfo describes one step of a plan.
type StepInfo struct {
	ID          string
	Description string
	Risk        Risk
}

// OfflinePlanner is a migrator that can plan without a database connection,
// such as in CI.
type OfflinePlanner interface {
	PlanOffline(ctx context.Context) (Plan, error)
}

// Verifier is a migrator that can detect drift beyond which version was
// applied.
type Verifier interface {
	Verify(ctx context.Context) (Drift, error)
}

// Drift is what Verify found.
type Drift struct {
	// Differences lists each way the database differs from what this
	// build expects; none means no drift.
	Differences []string
}

// None reports whether nothing drifted.
func (d Drift) None() bool { return len(d.Differences) == 0 }

// Backgrounder is a migrator with work that must continue after Apply
// returns, such as a backfill. Galleon schedules it; the tool decides what
// the work is.
type Backgrounder interface {
	BackgroundTasks(ctx context.Context) ([]BackgroundTask, error)
}

// BackgroundTask is one piece of background work.
type BackgroundTask interface {
	// ID is stable across builds and nodes.
	ID() string
	// Step does one bounded unit of work from cursor (nil at first) and
	// returns the cursor to continue from, and whether the task is
	// finished. Galleon saves the cursor, so the task can resume on any
	// node; a Step interrupted before its cursor was saved runs again from
	// the previous one, so it MUST be safe to repeat.
	Step(ctx context.Context, cursor []byte) (next []byte, done bool, err error)
}

// Completion is a migrator that can say whether a named task or change is
// finished, so code can wait for a backfill before relying on it.
type Completion interface {
	Done(ctx context.Context, id string) (bool, error)
}

// Capabilities lists the optional capabilities m has.
func Capabilities(m Migrator) []string {
	var out []string
	if _, ok := m.(Planner); ok {
		out = append(out, "plan")
	}
	if _, ok := m.(OfflinePlanner); ok {
		out = append(out, "plan_offline")
	}
	if _, ok := m.(Verifier); ok {
		out = append(out, "verify")
	}
	if _, ok := m.(Backgrounder); ok {
		out = append(out, "background")
	}
	if _, ok := m.(Completion); ok {
		out = append(out, "completion")
	}
	return out
}

// NewPlan returns a Plan of steps. With fingerprint empty, the fingerprint
// is computed from the steps' IDs, descriptions and risks; a tool whose
// steps have content beyond those, such as SQL text, passes its own.
func NewPlan(fingerprint string, steps []StepInfo) Plan {
	if fingerprint == "" {
		fingerprint = Fingerprint(steps, nil)
	}
	return plan{steps: append([]StepInfo(nil), steps...), fingerprint: fingerprint}
}

type plan struct {
	steps       []StepInfo
	fingerprint string
}

func (p plan) Steps() []StepInfo   { return append([]StepInfo(nil), p.steps...) }
func (p plan) Fingerprint() string { return p.fingerprint }
func (p plan) MaxRisk() Risk       { return MaxRisk(p.steps) }

// MaxRisk returns the riskiest of steps, Safe when there are none.
func MaxRisk(steps []StepInfo) Risk {
	max := Safe
	for _, s := range steps {
		if s.Risk.Above(max) {
			max = s.Risk
		}
	}
	return max
}

// Error codes of the errors the Runner returns.
const (
	CodeRiskRefused         = "migrate.risk_refused"
	CodeFingerprintMismatch = "migrate.fingerprint_mismatch"
	CodeLeaseLost           = "migrate.lease_lost"
	CodeNotSupported        = "migrate.not_supported"
	CodeNotReady            = "migrate.not_ready"
	CodeUnknownMigrator     = "migrate.unknown_migrator"
)

// ValidName reports whether name can name a migrator.
func ValidName(name string) error {
	if name == "" {
		return fmt.Errorf("a migrator's name must not be empty")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-') {
			return fmt.Errorf("migrator name %q must match [a-z0-9_.-]+", name)
		}
	}
	return nil
}
