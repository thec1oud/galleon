package testkit

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/migrate"
)

// MigrationStore is the fake database fake migrators apply to. Every
// Migrator made on one store shares what it has applied, as nodes share a
// database, so a test can run several nodes' migrators against it.
type MigrationStore struct {
	mu       sync.Mutex
	applied  map[string][]FakeStep // per migrator, in the order applied
	fence    map[string]fence
	progress map[string]int // per migrator/task, units done
	calls    map[string]int
	changes  map[string]int
}

// NewMigrationStore returns an empty store.
func NewMigrationStore() *MigrationStore {
	return &MigrationStore{applied: map[string][]FakeStep{}, fence: map[string]fence{}, progress: map[string]int{}, calls: map[string]int{}, changes: map[string]int{}}
}

// fence is a migrator's fencing record: tokens compare within an epoch, a
// new epoch takes over, and the epochs it took over from are refused.
type fence struct {
	epoch      string
	token      uint64
	superseded []string
}

// staleLocked reports why lease l can't apply, or nil.
func (s *MigrationStore) staleLocked(name string, l coord.Lease) error {
	f := s.fence[name]
	if f.epoch == l.Epoch() && l.Token() < f.token {
		return errs.New(errs.FailedPrecondition, "testkit.stale_token", fmt.Sprintf("fencing token %d is below %d in epoch %q", l.Token(), f.token, f.epoch))
	}
	if slices.Contains(f.superseded, l.Epoch()) {
		return errs.New(errs.FailedPrecondition, "testkit.stale_epoch", fmt.Sprintf("epoch %q was superseded by %q", l.Epoch(), f.epoch))
	}
	return nil
}

// raiseLocked moves the fence of name to lease l, which isn't stale.
func (s *MigrationStore) raiseLocked(name string, l coord.Lease) {
	f := s.fence[name]
	if f.epoch != l.Epoch() && f.epoch != "" {
		f.superseded = append(f.superseded, f.epoch)
	}
	f.epoch, f.token = l.Epoch(), l.Token()
	s.fence[name] = f
}

// FakeStep is one change of a fake migrator's build.
type FakeStep struct {
	ID   string
	Risk migrate.Risk
	// Content stands for the change's source: a step applied with other
	// content makes the status Drifted.
	Content string
}

// FakeTask is a background task of Units units of work.
type FakeTask struct {
	ID    string
	Units int
}

// FakeMigrator is a fake migrate.Migrator of a fixed build of steps, with
// every optional capability: Planner, OfflinePlanner, Verifier,
// Backgrounder and Completion. Wrap it with CoreOnly to test code against
// a migrator without them.
type FakeMigrator struct {
	store *MigrationStore
	name  string
	steps []FakeStep
	// Tasks are its background tasks, available once its steps are applied.
	Tasks []FakeTask
	// BeforeStep, when set, runs before each step is applied; an error
	// fails the apply there. Tests use it to block an apply or fail it
	// part way.
	BeforeStep func(ctx context.Context, s migrate.StepInfo) error
}

var (
	_ migrate.Migrator       = (*FakeMigrator)(nil)
	_ migrate.Planner        = (*FakeMigrator)(nil)
	_ migrate.OfflinePlanner = (*FakeMigrator)(nil)
	_ migrate.Verifier       = (*FakeMigrator)(nil)
	_ migrate.Backgrounder   = (*FakeMigrator)(nil)
	_ migrate.Completion     = (*FakeMigrator)(nil)
)

// Migrator returns a fake migrator named name whose build is steps.
func (s *MigrationStore) Migrator(name string, steps ...FakeStep) *FakeMigrator {
	return &FakeMigrator{store: s, name: name, steps: append([]FakeStep(nil), steps...)}
}

// ApplyCalls counts the calls to Apply of migrator name, on every node.
func (s *MigrationStore) ApplyCalls(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[name]
}

// Changes counts the Apply calls of migrator name that applied a step.
func (s *MigrationStore) Changes(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changes[name]
}

// Applied lists the IDs of the steps migrator name has applied, in order.
func (s *MigrationStore) Applied(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, st := range s.applied[name] {
		out = append(out, st.ID)
	}
	return out
}

func (m *FakeMigrator) Name() string { return m.name }

func info(s FakeStep) migrate.StepInfo {
	return migrate.StepInfo{ID: s.ID, Description: "fake step " + s.ID, Risk: s.Risk}
}

// compare is the store against the build, under the store's lock.
func (m *FakeMigrator) compareLocked() (pending []FakeStep, drift, ahead []string) {
	applied := map[string]FakeStep{}
	for _, a := range m.store.applied[m.name] {
		applied[a.ID] = a
	}
	// A pending step before the build's last applied one would apply out
	// of order: it has drifted.
	last := -1
	for i, s := range m.steps {
		if _, ok := applied[s.ID]; ok {
			last = i
		}
	}
	inBuild := map[string]bool{}
	for i, s := range m.steps {
		inBuild[s.ID] = true
		a, ok := applied[s.ID]
		switch {
		case !ok && i < last:
			drift = append(drift, fmt.Sprintf("%s is older than applied %s", s.ID, m.steps[last].ID))
		case !ok:
			pending = append(pending, s)
		case a.Content != s.Content:
			drift = append(drift, s.ID+" was edited after it was applied")
		}
	}
	// A step the build lacks is ahead once every step of the build is
	// applied before it; anywhere else, it has drifted.
	seen := 0
	for _, a := range m.store.applied[m.name] {
		if inBuild[a.ID] {
			seen++
			continue
		}
		if seen == len(m.steps) {
			ahead = append(ahead, a.ID)
		} else {
			drift = append(drift, a.ID+" is applied, but this build doesn't have it")
		}
	}
	return pending, drift, ahead
}

func (m *FakeMigrator) Status(ctx context.Context) (migrate.Status, error) {
	if err := ctx.Err(); err != nil {
		return migrate.Status{}, err
	}
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	pending, drift, ahead := m.compareLocked()
	st := migrate.Status{Expected: strconv.Itoa(len(m.steps)), Current: strconv.Itoa(len(m.store.applied[m.name]))}
	switch {
	case len(drift) > 0:
		st.State, st.Detail = migrate.Drifted, strings.Join(drift, "; ")
	case len(ahead) > 0:
		st.State, st.Detail = migrate.Ahead, "applied by a newer build: "+strings.Join(ahead, ", ")
	case len(pending) > 0:
		st.State, st.Detail = migrate.Pending, fmt.Sprintf("%d pending", len(pending))
	default:
		st.State = migrate.UpToDate
	}
	return st, nil
}

func (m *FakeMigrator) planLocked() migrate.Plan {
	pending, _, _ := m.compareLocked()
	steps := make([]migrate.StepInfo, len(pending))
	content := make([][]byte, len(pending))
	for i, s := range pending {
		steps[i], content[i] = info(s), []byte(s.Content)
	}
	return migrate.NewPlan(migrate.Fingerprint(steps, content), steps)
}

func (m *FakeMigrator) Plan(ctx context.Context) (migrate.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	return m.planLocked(), nil
}

// PlanOffline plans the whole build, as if the database were empty.
func (m *FakeMigrator) PlanOffline(ctx context.Context) (migrate.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	steps := make([]migrate.StepInfo, len(m.steps))
	content := make([][]byte, len(m.steps))
	for i, s := range m.steps {
		steps[i], content[i] = info(s), []byte(s.Content)
	}
	return migrate.NewPlan(migrate.Fingerprint(steps, content), steps), nil
}

func (m *FakeMigrator) Verify(ctx context.Context) (migrate.Drift, error) {
	if err := ctx.Err(); err != nil {
		return migrate.Drift{}, err
	}
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	_, drift, ahead := m.compareLocked()
	d := migrate.Drift{Differences: drift}
	for _, a := range ahead {
		d.Differences = append(d.Differences, a+" is applied by a newer build")
	}
	return d, nil
}

// Apply applies the pending steps in order, refusing a stale fencing
// token, a plan other than opts.Fingerprint, or a step the options don't
// allow, before applying anything.
func (m *FakeMigrator) Apply(ctx context.Context, opts migrate.ApplyOptions) (migrate.Result, error) {
	s := m.store
	s.mu.Lock()
	s.calls[m.name]++
	if opts.Lock == nil {
		s.mu.Unlock()
		return migrate.Result{}, errs.New(errs.FailedPrecondition, "testkit.no_lock", "a fake migrator never applies without a lease")
	}
	pending, drift, ahead := m.compareLocked()
	pl := m.planLocked()
	var err error
	switch stale := s.staleLocked(m.name, opts.Lock); {
	case stale != nil:
		err = stale
	case len(drift) > 0 || len(ahead) > 0:
		err = errs.New(errs.FailedPrecondition, "testkit.not_pending", "the store has drifted or is ahead")
	case opts.Fingerprint != "" && opts.Fingerprint != pl.Fingerprint():
		err = errs.New(errs.FailedPrecondition, migrate.CodeFingerprintMismatch, "the plan isn't the approved one")
	default:
		for _, st := range pending {
			if !opts.Allows(st.Risk) {
				err = errs.New(errs.FailedPrecondition, migrate.CodeRiskRefused, fmt.Sprintf("step %s is of %s risk", st.ID, st.Risk))
				break
			}
		}
		if err == nil {
			s.raiseLocked(m.name, opts.Lock)
		}
	}
	s.mu.Unlock()
	if err != nil {
		return migrate.Result{}, err
	}
	var res migrate.Result
	for _, st := range pending {
		sctx, done := opts.Step(ctx, info(st))
		err := sctx.Err()
		if err == nil && m.BeforeStep != nil {
			err = m.BeforeStep(sctx, info(st))
		}
		if err == nil {
			err = sctx.Err()
		}
		if err == nil {
			s.mu.Lock()
			if stale := s.staleLocked(m.name, opts.Lock); stale != nil {
				err = stale
			} else if !m.appliedLocked(st.ID) {
				s.applied[m.name] = append(s.applied[m.name], st)
				res.Applied = append(res.Applied, info(st))
				if len(res.Applied) == 1 {
					s.changes[m.name]++
				}
			}
			s.mu.Unlock()
		}
		done(err)
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

func (m *FakeMigrator) appliedLocked(id string) bool {
	for _, a := range m.store.applied[m.name] {
		if a.ID == id {
			return true
		}
	}
	return false
}

// BackgroundTasks returns Tasks once every step is applied.
func (m *FakeMigrator) BackgroundTasks(ctx context.Context) ([]migrate.BackgroundTask, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []migrate.BackgroundTask
	for _, t := range m.Tasks {
		out = append(out, &fakeTask{m: m, task: t})
	}
	return out, nil
}

// Done reports whether task id has done all its units, or step id is
// applied.
func (m *FakeMigrator) Done(ctx context.Context, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	for _, t := range m.Tasks {
		if t.ID == id {
			return m.store.progress[m.name+"/"+id] >= t.Units, nil
		}
	}
	return m.appliedLocked(id), nil
}

// Progress is how many units task id of migrator name has done.
func (s *MigrationStore) Progress(name, id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progress[name+"/"+id]
}

type fakeTask struct {
	m    *FakeMigrator
	task FakeTask
}

func (t *fakeTask) ID() string { return t.task.ID }

// Step does the unit after cursor. Repeating a step from the same cursor
// does the same unit again, which changes nothing.
func (t *fakeTask) Step(ctx context.Context, cursor []byte) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	n := 0
	if len(cursor) > 0 {
		var err error
		if n, err = strconv.Atoi(string(cursor)); err != nil {
			return nil, false, fmt.Errorf("bad cursor %q", cursor)
		}
	}
	n = min(n+1, t.task.Units)
	key := t.m.name + "/" + t.task.ID
	s := t.m.store
	s.mu.Lock()
	s.progress[key] = max(s.progress[key], n)
	s.mu.Unlock()
	return []byte(strconv.Itoa(n)), n >= t.task.Units, nil
}

// CoreOnly hides every optional capability of m, leaving the required
// core, to test code against a migrator that can't plan, verify or run
// background tasks.
func CoreOnly(m migrate.Migrator) migrate.Migrator { return coreOnly{m} }

type coreOnly struct{ m migrate.Migrator }

func (c coreOnly) Name() string { return c.m.Name() }
func (c coreOnly) Status(ctx context.Context) (migrate.Status, error) {
	return c.m.Status(ctx)
}
func (c coreOnly) Apply(ctx context.Context, opts migrate.ApplyOptions) (migrate.Result, error) {
	return c.m.Apply(ctx, opts)
}
