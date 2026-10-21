package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/errs"
)

// LockKey is the coordination lock key every node applies migrator name
// under.
func LockKey(name string) string { return "galleon/migrate/" + name }

// Config configures a Runner.
type Config struct {
	// Locker is the cluster's lock. It is required: a migrator never runs
	// without one.
	Locker coord.Locker
	// Migrators run in this order.
	Migrators []Migrator
	Policy    Policy
	// Logger, TracerProvider and MeterProvider default to discarding.
	Logger         *slog.Logger
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
}

// Runner applies migrators exactly once across the cluster, under the
// policy, and reports their status and plans. The kernel builds one at
// boot (see galleon.Migrator); tools and tests can build their own. It is
// safe for concurrent use; the lock keeps concurrent applies, in this
// process or any other, to one at a time per migrator.
type Runner struct {
	locker    coord.Locker
	migrators []Migrator
	policy    Policy
	log       *slog.Logger
	tracer    trace.Tracer
	metrics   *metrics
}

// NewRunner checks cfg and returns its Runner.
func NewRunner(cfg Config) (*Runner, error) {
	if cfg.Locker == nil {
		return nil, errors.New("migrate: a Runner needs the cluster's Locker: a migrator never runs without the lock")
	}
	seen := map[string]bool{}
	for _, m := range cfg.Migrators {
		if m == nil {
			return nil, errors.New("migrate: a migrator is nil")
		}
		if err := ValidName(m.Name()); err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
		if seen[m.Name()] {
			return nil, fmt.Errorf("migrate: two migrators are named %q", m.Name())
		}
		seen[m.Name()] = true
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.TracerProvider == nil {
		cfg.TracerProvider = noop.NewTracerProvider()
	}
	return &Runner{
		locker:    cfg.Locker,
		migrators: append([]Migrator(nil), cfg.Migrators...),
		policy:    cfg.Policy,
		log:       cfg.Logger,
		tracer:    cfg.TracerProvider.Tracer(tracerName),
		metrics:   newMetrics(cfg.MeterProvider),
	}, nil
}

// Migrators returns the migrators, in order.
func (r *Runner) Migrators() []Migrator { return append([]Migrator(nil), r.migrators...) }

// Policy returns the policy.
func (r *Runner) Policy() Policy { return r.policy }

// Migrator returns the migrator named name.
func (r *Runner) Migrator(name string) (Migrator, error) {
	for _, m := range r.migrators {
		if m.Name() == name {
			return m, nil
		}
	}
	return nil, errs.New(errs.NotFound, CodeUnknownMigrator, fmt.Sprintf("no migrator is named %q", name))
}

// Report is one migrator's status, as Runner.Status reads it.
type Report struct {
	Name         string
	Capabilities []string
	Status       Status
	// Err is why Status failed; the other fields are then unset.
	Err error
	// Ready is why the policy keeps the node out of traffic, or nil.
	Ready error
	// Drift is what Verify found, for a Verifier; DriftErr why it failed.
	Drift    *Drift
	DriftErr error
	// Background lists a Backgrounder's tasks; BackgroundErr is why they
	// couldn't be listed.
	Background    []TaskReport
	BackgroundErr error
}

// TaskReport is one background task.
type TaskReport struct {
	ID string
	// Done is what the migrator's Completion says. Known is false without
	// Completion, or when it failed (Err).
	Done, Known bool
	Err         error
}

// Status reads every migrator's status, in order, with its drift and
// background tasks where the migrator has those capabilities.
func (r *Runner) Status(ctx context.Context) []Report {
	out := make([]Report, 0, len(r.migrators))
	for _, m := range r.migrators {
		out = append(out, r.report(ctx, m))
	}
	return out
}

func (r *Runner) report(ctx context.Context, m Migrator) Report {
	rep := Report{Name: m.Name(), Capabilities: Capabilities(m)}
	st, err := m.Status(ctx)
	if err != nil {
		rep.Err = err
		rep.Ready = errs.Wrap(err, errs.Unavailable, CodeNotReady, fmt.Sprintf("migrator %q: reading its status failed", m.Name()))
		return rep
	}
	rep.Status = st
	rep.Ready = r.policy.Ready(m.Name(), st)
	if v, ok := m.(Verifier); ok {
		d, err := v.Verify(ctx)
		if err != nil {
			rep.DriftErr = err
		} else {
			rep.Drift = &d
		}
	}
	if b, ok := m.(Backgrounder); ok {
		tasks, err := b.BackgroundTasks(ctx)
		rep.BackgroundErr = err
		c, hasDone := m.(Completion)
		for _, t := range tasks {
			tr := TaskReport{ID: t.ID()}
			if hasDone {
				tr.Done, tr.Err = c.Done(ctx, t.ID())
				tr.Known = tr.Err == nil
			}
			rep.Background = append(rep.Background, tr)
		}
	}
	return rep
}

// PlanReport is one migrator's plan.
type PlanReport struct {
	Name string
	// Plan is nil when Err is set.
	Plan Plan
	// Err is why there is no plan; for a migrator without the capability,
	// an Unimplemented error saying so.
	Err error
	// Refused is why the policy would refuse the plan, or nil.
	Refused error
}

// Plan asks every migrator what Apply would do, in order. Offline asks
// OfflinePlanners, without a database. A migrator without the capability
// gets an Err saying so, and the others are still asked.
func (r *Runner) Plan(ctx context.Context, offline bool) []PlanReport {
	out := make([]PlanReport, 0, len(r.migrators))
	for _, m := range r.migrators {
		rep := PlanReport{Name: m.Name()}
		var pl Plan
		var err error
		if !offline {
			pl, err = r.plan(ctx, m)
		} else if p, ok := m.(OfflinePlanner); ok {
			pl, err = p.PlanOffline(ctx)
		} else {
			err = errs.New(errs.Unimplemented, CodeNotSupported, fmt.Sprintf("plan offline: migrator %q can't plan without a database", m.Name()))
		}
		rep.Plan, rep.Err = pl, err
		if err == nil {
			rep.Refused = r.policy.check(m.Name(), pl)
		}
		out = append(out, rep)
	}
	return out
}

func (r *Runner) plan(ctx context.Context, m Migrator) (Plan, error) {
	p, ok := m.(Planner)
	if !ok {
		return nil, errs.New(errs.Unimplemented, CodeNotSupported, fmt.Sprintf("plan: migrator %q can't plan", m.Name()))
	}
	return p.Plan(ctx)
}

// Outcome is what applying one migrator came to.
type Outcome string

const (
	// Applied: this node applied changes.
	Applied Outcome = "applied"
	// AlreadyUpToDate: there was nothing to apply. After waiting for the
	// lock, that usually means another node applied the changes.
	AlreadyUpToDate Outcome = "up_to_date"
	// AheadTolerated: the database is ahead of this build, which the
	// policy tolerates; nothing was applied.
	AheadTolerated Outcome = "ahead"
	// Refused: the policy, or the database's state, refused the apply
	// before anything ran.
	Refused Outcome = "refused"
	// Failed: the apply, or reading the status around it, failed.
	Failed Outcome = "failed"
	// NotRun: an earlier migrator failed, so this one wasn't tried.
	NotRun Outcome = "not_run"
)

// ApplyReport is what Apply did with one migrator.
type ApplyReport struct {
	Name    string
	Outcome Outcome
	Result  Result
	// Waited is set when another node held the lock first.
	Waited bool
	// Notes say what the Runner had to assume, such as that a migrator
	// that can't plan is of Unknown risk.
	Notes    []string
	Err      error
	Duration time.Duration
}

// Apply applies every migrator in order, each under its cluster lock, and
// stops at the first failure: the migrators after it are reported NotRun.
// The error is the first failure's.
func (r *Runner) Apply(ctx context.Context) ([]ApplyReport, error) {
	out := make([]ApplyReport, 0, len(r.migrators))
	var failed error
	for _, m := range r.migrators {
		if failed != nil {
			out = append(out, ApplyReport{Name: m.Name(), Outcome: NotRun})
			continue
		}
		rep := r.applyOne(ctx, m)
		out = append(out, rep)
		failed = rep.Err
	}
	return out, failed
}

// ApplyOne applies the migrator named name alone, under its lock.
func (r *Runner) ApplyOne(ctx context.Context, name string) (ApplyReport, error) {
	m, err := r.Migrator(name)
	if err != nil {
		return ApplyReport{Name: name, Outcome: Failed, Err: err}, err
	}
	rep := r.applyOne(ctx, m)
	return rep, rep.Err
}

var errLeaseLost = errs.New(errs.Unavailable, CodeLeaseLost, "the migration lease was lost")

func (r *Runner) applyOne(ctx context.Context, m Migrator) (rep ApplyReport) {
	name := m.Name()
	rep.Name = name
	start := time.Now()
	ctx, span := r.tracer.Start(ctx, "migrate.apply", trace.WithAttributes(attrMigrator.String(name)))
	log := r.log.With("migrator", name)
	defer func() {
		rep.Duration = time.Since(start)
		r.finish(ctx, span, log, &rep)
	}()

	ttl := r.policy.lockTTL()
	lease, ok, err := r.locker.TryAcquire(ctx, LockKey(name), ttl)
	if err != nil {
		rep.Outcome, rep.Err = Failed, fmt.Errorf("migrator %q: taking the migration lock: %w", name, err)
		return rep
	}
	if !ok {
		rep.Waited = true
		log.InfoContext(ctx, "another node is applying this migrator; waiting for it")
		wctx, cancel := ctx, context.CancelFunc(func() {})
		if r.policy.LockWait > 0 {
			wctx, cancel = context.WithTimeout(ctx, r.policy.LockWait)
		}
		lease, err = r.locker.Acquire(wctx, LockKey(name), ttl)
		cancel()
		if err != nil {
			rep.Outcome, rep.Err = Failed, fmt.Errorf("migrator %q: waiting for the migration lock: %w", name, err)
			return rep
		}
	}
	span.SetAttributes(attrToken.Int64(int64(lease.Token())))
	actx, stop := r.hold(ctx, lease, log)
	defer stop()

	// Read under the lock: a node that waited sees what the holder did,
	// and doesn't apply it again.
	st, err := m.Status(actx)
	if err != nil {
		rep.Outcome, rep.Err = Failed, r.lost(actx, fmt.Errorf("migrator %q: reading its status: %w", name, err))
		return rep
	}
	switch st.State {
	case UpToDate:
		rep.Outcome = AlreadyUpToDate
		return rep
	case Ahead:
		if r.policy.TolerateAhead {
			rep.Outcome = AheadTolerated
			return rep
		}
		rep.Outcome, rep.Err = Refused, r.policy.Ready(name, st)
		return rep
	case Pending:
	default:
		rep.Outcome, rep.Err = Refused, r.policy.Ready(name, st)
		return rep
	}

	opts := ApplyOptions{Lock: lease, MaxRisk: r.policy.MaxRisk, AllowUnknown: r.policy.AllowUnknown, Observer: &observer{r: r, migrator: name, log: log}}
	if p, ok := m.(Planner); ok {
		pl, err := p.Plan(actx)
		if err != nil {
			rep.Outcome, rep.Err = Failed, r.lost(actx, fmt.Errorf("migrator %q: planning: %w", name, err))
			return rep
		}
		if err := r.policy.check(name, pl); err != nil {
			rep.Outcome, rep.Err = Refused, err
			return rep
		}
		opts.Fingerprint = pl.Fingerprint()
		span.SetAttributes(attrFingerprint.String(pl.Fingerprint()), attrRisk.String(pl.MaxRisk().String()))
	} else {
		note := fmt.Sprintf("plan: migrator %q can't plan, so its changes are treated as of unknown risk", name)
		rep.Notes = append(rep.Notes, note)
		log.WarnContext(ctx, note)
		if _, pinned := r.policy.Approved[name]; pinned {
			rep.Outcome, rep.Err = Refused, errs.New(errs.FailedPrecondition, CodeNotSupported, fmt.Sprintf("migrator %q can't plan, so no plan fingerprint can be approved for it; remove its approval", name))
			return rep
		}
		if !r.policy.AllowUnknown {
			rep.Outcome, rep.Err = Refused, errs.New(errs.FailedPrecondition, CodeRiskRefused, fmt.Sprintf("migrator %q can't plan, and the policy refuses changes of unknown risk without approval (allow_unknown); nothing was applied", name))
			return rep
		}
	}

	log.InfoContext(ctx, "applying migrations", "token", lease.Token(), "waited", rep.Waited)
	res, err := m.Apply(actx, opts)
	rep.Result = res
	if err != nil {
		rep.Outcome, rep.Err = Failed, r.lost(actx, fmt.Errorf("migrator %q: applying: %w", name, err))
		return rep
	}
	if err := r.lost(actx, nil); err != nil {
		rep.Outcome, rep.Err = Failed, err
		return rep
	}
	// Recheck: an apply that returns without error must have left the
	// database as this build expects.
	after, err := m.Status(actx)
	if err != nil {
		rep.Outcome, rep.Err = Failed, r.lost(actx, fmt.Errorf("migrator %q: reading its status after applying: %w", name, err))
		return rep
	}
	if after.State != UpToDate {
		rep.Outcome, rep.Err = Failed, fmt.Errorf("migrator %q: applied, but its status is %s rather than up to date: %w", name, after.State, r.policy.Ready(name, after))
		return rep
	}
	rep.Outcome = Applied
	return rep
}

// hold keeps lease renewed until stop is called, and returns a context
// that is cancelled, with errLeaseLost as its cause, if the lease is lost.
// stop releases the lease.
func (r *Runner) hold(ctx context.Context, lease coord.Lease, log *slog.Logger) (context.Context, func()) {
	actx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	exited := make(chan struct{})
	every := r.policy.lockTTL() / 3
	go func() {
		defer close(exited)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-lease.Lost():
				log.ErrorContext(ctx, "the migration lease was lost while applying; cancelling the apply", "token", lease.Token())
				cancel(errLeaseLost)
				return
			case <-t.C:
				if err := lease.Renew(actx); err != nil {
					log.ErrorContext(ctx, "renewing the migration lease failed; cancelling the apply", "token", lease.Token(), "error", err)
					cancel(errLeaseLost)
					return
				}
			}
		}
	}()
	return actx, func() {
		close(done)
		<-exited
		cancel(nil)
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer rcancel()
		if err := lease.Release(rctx); err != nil {
			log.WarnContext(ctx, "releasing the migration lease failed; it expires on its own", "error", err)
		}
	}
}

// lost returns err, or the lease-lost error in its place when the lease was
// lost, so the report says what really happened.
func (r *Runner) lost(actx context.Context, err error) error {
	if cause := context.Cause(actx); errors.Is(cause, errLeaseLost) {
		if err == nil {
			return errLeaseLost
		}
		return errs.Wrap(err, errs.Unavailable, CodeLeaseLost, "the migration lease was lost during the apply, which was cancelled")
	}
	return err
}

// Done asks the migrator named name whether its task or change id is
// finished. It fails with Unimplemented when the migrator has no
// Completion.
func (r *Runner) Done(ctx context.Context, name, id string) (bool, error) {
	m, err := r.Migrator(name)
	if err != nil {
		return false, err
	}
	c, ok := m.(Completion)
	if !ok {
		return false, errs.New(errs.Unimplemented, CodeNotSupported, fmt.Sprintf("done: migrator %q can't say whether a task is done", name))
	}
	return c.Done(ctx, id)
}
