package migrate_test

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	membedded "go.opentelemetry.io/otel/metric/embedded"
	mnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/migrate"
)

// db is a fake database the fake migrators of a test share.
type db struct {
	mu      sync.Mutex
	applied map[string]int // steps applied, per migrator
	calls   map[string]int // calls to Apply
	opts    []migrate.ApplyOptions
}

func newDB() *db { return &db{applied: map[string]int{}, calls: map[string]int{}} }

func (d *db) callsOf(name string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[name]
}

// fake is a migrator of steps; with plan set it is a Planner.
type fake struct {
	db    *db
	name  string
	steps []migrate.StepInfo
	// state, when set, overrides the status.
	state migrate.State
	// block, when set, is waited on inside Apply, or ctx's end.
	block chan struct{}
	// started is closed when Apply begins.
	started chan struct{}
	// stuck leaves the status Pending after Apply.
	stuck   bool
	fail    error
	statErr error
	// afterApply runs as Apply returns successfully.
	afterApply func(ctx context.Context)
}

func (f *fake) Name() string { return f.name }

func (f *fake) Status(ctx context.Context) (migrate.Status, error) {
	if err := ctx.Err(); err != nil {
		return migrate.Status{}, err // as a database call would
	}
	if f.statErr != nil {
		return migrate.Status{}, f.statErr
	}
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	if f.state != 0 {
		return migrate.Status{State: f.state, Expected: "2", Current: "3", Detail: "detail"}, nil
	}
	if f.db.applied[f.name] >= len(f.steps) {
		return migrate.Status{State: migrate.UpToDate}, nil
	}
	return migrate.Status{State: migrate.Pending, Detail: "steps pending"}, nil
}

func (f *fake) Apply(ctx context.Context, opts migrate.ApplyOptions) (migrate.Result, error) {
	f.db.mu.Lock()
	f.db.calls[f.name]++
	f.db.opts = append(f.db.opts, opts)
	f.db.mu.Unlock()
	if f.started != nil {
		close(f.started)
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return migrate.Result{}, ctx.Err()
		}
	}
	if f.fail != nil {
		return migrate.Result{}, f.fail
	}
	var res migrate.Result
	for _, s := range f.steps {
		_, done := opts.Step(ctx, s)
		done(nil)
		res.Applied = append(res.Applied, s)
	}
	if !f.stuck {
		f.db.mu.Lock()
		f.db.applied[f.name] = len(f.steps)
		f.db.mu.Unlock()
	}
	if f.afterApply != nil {
		f.afterApply(ctx)
	}
	return res, nil
}

// planner is a fake that can plan.
type planner struct{ *fake }

func (p planner) Plan(context.Context) (migrate.Plan, error) {
	p.db.mu.Lock()
	defer p.db.mu.Unlock()
	if p.db.applied[p.name] >= len(p.steps) {
		return migrate.NewPlan("", nil), nil
	}
	return migrate.NewPlan("", p.steps), nil
}

// full is a planner with every other capability too.
type full struct{ planner }

func (f full) PlanOffline(context.Context) (migrate.Plan, error) {
	return migrate.NewPlan("", f.steps), nil
}
func (f full) Verify(context.Context) (migrate.Drift, error) {
	return migrate.Drift{Differences: []string{"x was edited"}}, nil
}
func (f full) BackgroundTasks(context.Context) ([]migrate.BackgroundTask, error) {
	return []migrate.BackgroundTask{task("backfill"), task("reindex")}, nil
}
func (f full) Done(_ context.Context, id string) (bool, error) { return id == "reindex", nil }

type task string

func (t task) ID() string { return string(t) }
func (t task) Step(context.Context, []byte) ([]byte, bool, error) {
	return nil, true, nil
}

// locker wraps a Locker to count waits and hand out controllable leases.
type locker struct {
	coord.Locker
	mu       sync.Mutex
	waits    int
	leases   []*lease
	renewed  error // what Renew returns
	flaky    int   // how many Renews fail before they succeed again
	renewals int   // Renews called
	// realTTL, when set, is the TTL the wrapped Locker grants instead, so
	// a test on a fake clock isn't cut short by real time.
	realTTL time.Duration
	// renew, when set, is what Renew does instead.
	renew func(ctx context.Context) error
}

func (l *locker) TryAcquire(ctx context.Context, key string, ttl time.Duration) (coord.Lease, bool, error) {
	ttl = cmp.Or(l.realTTL, ttl)
	in, ok, err := l.Locker.TryAcquire(ctx, key, ttl)
	if !ok || err != nil {
		return in, ok, err
	}
	return l.wrap(in), true, nil
}

func (l *locker) Acquire(ctx context.Context, key string, ttl time.Duration) (coord.Lease, error) {
	l.mu.Lock()
	l.waits++
	l.mu.Unlock()
	in, err := l.Locker.Acquire(ctx, key, cmp.Or(l.realTTL, ttl))
	if err != nil {
		return nil, err
	}
	return l.wrap(in), nil
}

func (l *locker) waiting() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waits
}

func (l *locker) wrap(in coord.Lease) *lease {
	out := &lease{Lease: in, l: l, lost: make(chan struct{})}
	go func() {
		select {
		case <-in.Lost():
			out.lose()
		case <-out.lost:
		}
	}()
	l.mu.Lock()
	l.leases = append(l.leases, out)
	l.mu.Unlock()
	return out
}

func (l *locker) last() *lease {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leases[len(l.leases)-1]
}

type lease struct {
	coord.Lease
	l    *locker
	once sync.Once
	lost chan struct{}
}

func (l *lease) lose()                 { l.once.Do(func() { close(l.lost) }) }
func (l *lease) Lost() <-chan struct{} { return l.lost }
func (l *lease) Renew(ctx context.Context) error {
	l.l.mu.Lock()
	if f := l.l.renew; f != nil {
		l.l.renewals++
		l.l.mu.Unlock()
		return f(ctx)
	}
	err := l.l.renewed
	l.l.renewals++
	if l.l.flaky > 0 {
		l.l.flaky--
		err = errors.New("timeout")
	}
	l.l.mu.Unlock()
	if err != nil {
		return err
	}
	return l.Lease.Renew(ctx)
}

// recorder keeps every span.
type recorder struct {
	embedded.TracerProvider
	mu    sync.Mutex
	spans []*span
}

func (r *recorder) Tracer(string, ...trace.TracerOption) trace.Tracer { return tracer{r: r} }

func (r *recorder) named(name string) []*span {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*span
	for _, s := range r.spans {
		if s.name == name {
			out = append(out, s)
		}
	}
	return out
}

type tracer struct {
	embedded.Tracer
	r *recorder
}

func (t tracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	cfg := trace.NewSpanStartConfig(opts...)
	s := &span{name: name, attrs: map[string]string{}}
	s.parent, _ = trace.SpanFromContext(ctx).(*span)
	s.SetAttributes(cfg.Attributes()...)
	t.r.mu.Lock()
	t.r.spans = append(t.r.spans, s)
	t.r.mu.Unlock()
	return trace.ContextWithSpan(ctx, s), s
}

type span struct {
	noop.Span
	mu     sync.Mutex
	name   string
	parent *span
	attrs  map[string]string
	status string
	ended  bool
}

func (s *span) IsRecording() bool { return true }
func (s *span) SetAttributes(kv ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range kv {
		s.attrs[string(a.Key)] = a.Value.Emit()
	}
}
func (s *span) SetStatus(_ codes.Code, desc string) { s.mu.Lock(); s.status = desc; s.mu.Unlock() }
func (s *span) End(...trace.SpanEndOption)          { s.mu.Lock(); s.ended = true; s.mu.Unlock() }
func (s *span) attr(k string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.attrs[k]
	return v, ok
}

// meter counts what instruments record, by name and attributes.
type meter struct {
	mnoop.Meter
	mu   sync.Mutex
	seen map[string]float64
}

type meterProvider struct {
	membedded.MeterProvider
	m *meter
}

func (p meterProvider) Meter(string, ...metric.MeterOption) metric.Meter { return p.m }

func (m *meter) add(name string, v float64, set attribute.Set) {
	var keys []string
	for _, kv := range set.ToSlice() {
		keys = append(keys, string(kv.Key)+"="+kv.Value.Emit())
	}
	slices.Sort(keys)
	key := name
	for _, k := range keys {
		key += " " + k
	}
	m.mu.Lock()
	m.seen[key] += v
	m.mu.Unlock()
}

func (m *meter) get(key string) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seen[key]
}

type counter struct {
	mnoop.Int64Counter
	m    *meter
	name string
}

func (c counter) Add(_ context.Context, v int64, opts ...metric.AddOption) {
	c.m.add(c.name, float64(v), metric.NewAddConfig(opts).Attributes())
}

type histogram struct {
	mnoop.Float64Histogram
	m    *meter
	name string
}

// Record counts records, rather than summing durations.
func (h histogram) Record(_ context.Context, _ float64, opts ...metric.RecordOption) {
	h.m.add(h.name, 1, metric.NewRecordConfig(opts).Attributes())
}

func (m *meter) Int64Counter(name string, _ ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return counter{m: m, name: name}, nil
}

func (m *meter) Float64Histogram(name string, _ ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return histogram{m: m, name: name}, nil
}

var errBoom = errors.New("boom")

// sqlError is a database error that quotes a statement, as a driver's may.
type sqlError struct{}

func (sqlError) Error() string    { return `syntax error at or near "DROP TABLE payroll"` }
func (sqlError) SQLState() string { return "42601" }
