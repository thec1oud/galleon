// Package pg is the data plane on PostgreSQL, through pgx: a data.Manager
// over a pool for the primary and one for each named source.
//
//	galleon.New(..., galleon.Modules(pg.Module(), server.Module()))
//
// with the "pg" config section:
//
//	{"pg": {"primary": {"dsn": "postgres://app@db/orders", "max_conns": 20},
//	        "named": {"analytics": {"dsn": "postgres://app@warehouse/events"}}}}
//
// The module provides data.Manager and *pg.Manager. Pools open when the app
// starts, which fails if a source can't be reached, and close after every
// drain and stop hook, so requests still in flight while draining keep their
// connections. Each source is a readiness gate ("pg.primary", "pg.<name>").
//
// Every root transaction an operation begins is tracked until the operation
// ends; one still open then is rolled back and reported (see Config.Leaks).
// A transaction begun after its operation ended, by a goroutine that kept
// the operation's ctx, isn't tracked: like any transaction begun outside an
// operation, its owner must end it.
//
// Transactions get a span each, and statements, batches and copies a client
// span each, with the database semantic conventions. Statement spans carry
// the SQL text, never its arguments. The spans of statements whose ctx
// names a model (data.ForModel) carry it as galleon.data.model, and, when
// a module registered it (galleon.Collect of a data.ModelInfo), its table
// as db.collection.name.
//
// The primary may have streaming replicas ("replicas"). A read that begins
// a transaction on the primary source runs on one when its read consistency
// allows (see data.Level), chosen from the readings a background tracker
// takes of each; writes never do. Every operation call has a causal floor:
// the WAL position of each write it commits, read in the commit's own round
// trip, and of the token it brought. With "token_key" set, a call that
// committed a write sends a signed token for its floor in the
// Galleon-Consistency response header, and a later call that brings it
// back, to any node sharing the key, reads at least that far.
package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

// Manager is the pgx data.Manager. Build it with New, or Within for tests.
type Manager struct {
	sources map[string]*source
	names   []string // sorted, the primary first
	tel     atomic.Pointer[telemetry]
	apps    appStack
	life    sync.Mutex // serializes Start and Stop
	started bool
	retry   data.RetryPolicy
	leaks   string
	within  *within
	// topo is the primary's replicas; nil without any.
	topo *topology
	// level is the read consistency of reads that set none.
	level   data.Level
	keys    *keyring // nil without a token key
	clock   clock.Clock
	metrics *metrics
	// foreignLevels is the operations already logged for declaring a read
	// consistency that isn't a data.Level.
	foreignLevels sync.Map
	// wal is the primary's WAL page layout, which the primary's poller
	// reads until it can; nil until then, and without replicas.
	wal atomic.Pointer[walLayout]
}

var _ data.Manager = (*Manager)(nil)

// telemetry is where a Manager sends spans and logs, and the models its
// spans name. A Within manager gets the app's when the module is installed,
// after it was built.
type telemetry struct {
	tracer trace.Tracer
	log    *slog.Logger
	// tables is each registered model's table, as a span names it.
	tables map[string]attribute.KeyValue
}

func newTelemetry(tp trace.TracerProvider, log *slog.Logger, models *data.Models) *telemetry {
	t := &telemetry{tracer: tp.Tracer(tracerName), log: log, tables: map[string]attribute.KeyValue{}}
	for _, m := range models.All() {
		t.tables[m.Name] = semconv.DBCollectionName(m.Identifier().Sanitize())
	}
	return t
}

func (m *Manager) setTelemetry(tp trace.TracerProvider, log *slog.Logger, models *data.Models) {
	t := newTelemetry(tp, log, models)
	m.apps.mu.Lock()
	defer m.apps.mu.Unlock()
	m.apps.own = t
	m.tel.Store(t)
}

// appStack is the telemetry of the apps running on a Use'd Manager, in the
// order they started. The Manager uses the last one's, or its own when none
// runs.
type appStack struct {
	mu   sync.Mutex
	own  *telemetry
	live []*telemetry
}

func (m *Manager) pushTelemetry(t *telemetry) {
	m.apps.mu.Lock()
	defer m.apps.mu.Unlock()
	m.apps.live = append(m.apps.live, t)
	m.tel.Store(t)
}

func (m *Manager) popTelemetry(t *telemetry) {
	m.apps.mu.Lock()
	defer m.apps.mu.Unlock()
	m.apps.live = slices.DeleteFunc(m.apps.live, func(x *telemetry) bool { return x == t })
	if n := len(m.apps.live); n > 0 {
		m.tel.Store(m.apps.live[n-1])
	} else {
		m.tel.Store(m.apps.own)
	}
}

func (m *Manager) tracer() trace.Tracer { return m.tel.Load().tracer }

// table is the table of the model named name, as a span names it, when the
// model is registered.
func (m *Manager) table(name string) (attribute.KeyValue, bool) {
	kv, ok := m.tel.Load().tables[name]
	return kv, ok
}
func (m *Manager) logger() *slog.Logger { return m.tel.Load().log }

// source is one database and its pool.
type source struct {
	name   string
	config *pgxpool.Config
	// pool is nil before Start and after Stop. Read it once per use: Stop
	// can clear it at any time.
	pool atomic.Pointer[pgxpool.Pool]
	// db, host and port label spans.
	db, host string
	port     int
}

// Option configures a Manager.
type Option func(*settings)

type settings struct {
	tp     trace.TracerProvider
	mp     metric.MeterProvider
	log    *slog.Logger
	clock  clock.Clock
	models *data.Models
}

// TracerProvider, MeterProvider, Logger and Clock replace the defaults (no
// tracing, no metrics, the default logger, the system clock); the module
// sets them from the app. The clock dates causal tokens; tracking replicas
// keeps real time.
func TracerProvider(tp trace.TracerProvider) Option { return func(s *settings) { s.tp = tp } }
func MeterProvider(mp metric.MeterProvider) Option  { return func(s *settings) { s.mp = mp } }
func Logger(l *slog.Logger) Option                  { return func(s *settings) { s.log = l } }
func Clock(c clock.Clock) Option                    { return func(s *settings) { s.clock = c } }

// Models is the model registry whose tables statement spans name, for
// statements whose ctx names a model (data.ForModel). The module sets it
// from the models modules collect.
func Models(r *data.Models) Option { return func(s *settings) { s.models = r } }

func (s *settings) defaults() {
	if s.tp == nil {
		s.tp = noop.NewTracerProvider()
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.clock == nil {
		s.clock = clock.System()
	}
}

// beforePools runs in New between validating cfg and building the pools'
// configs. Tests replace it.
var beforePools = func() {}

// New builds a Manager for cfg. It doesn't connect: Start opens the pools.
func New(cfg Config, opts ...Option) (*Manager, error) {
	cfg.withDefaults()
	// Validate writes the validated sources back, and the map and slice
	// are the caller's, perhaps shared.
	cfg.Named = maps.Clone(cfg.Named)
	cfg.Replicas = slices.Clone(cfg.Replicas)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("pg: %w", err)
	}
	var s settings
	for _, o := range opts {
		o(&s)
	}
	s.defaults()
	m := &Manager{
		sources: map[string]*source{},
		retry:   cfg.retryPolicy(),
		leaks:   cfg.Leaks,
		level:   cfg.defaultLevel(),
		keys:    newKeyring(cfg),
		clock:   s.clock,
	}
	m.setTelemetry(s.tp, s.log, s.models)
	beforePools()
	build := func(name string, src Source, tracer *queryTracer) (*source, error) {
		// Validate parsed it, but parsing reads files, such as an
		// sslrootcert, that may have changed since.
		pc, err := src.poolConfig()
		if err != nil {
			return nil, fmt.Errorf("pg: source %q: %w", name, err)
		}
		pc.ConnConfig.Tracer = tracer
		return &source{name: name, config: pc, db: pc.ConnConfig.Database, host: pc.ConnConfig.Host, port: int(pc.ConnConfig.Port)}, nil
	}
	add := func(name string, src Source) error {
		s, err := build(name, src, &queryTracer{m: m, source: name})
		m.sources[name] = s
		return err
	}
	if err := add(data.PrimarySource, cfg.Primary); err != nil {
		return nil, err
	}
	for name, src := range cfg.Named {
		if err := add(name, src); err != nil {
			return nil, err
		}
	}
	m.names = sortedNames(cfg)
	if len(cfg.Replicas) > 0 {
		t := &topology{sel: cfg.Select, zone: cfg.Zone, maxLag: cfg.MaxLag, maxBytes: cfg.MaxLagBytes, poll: cfg.LagPoll, now: time.Now, layoutRetry: defaultLayoutRetry}
		for _, rc := range cfg.Replicas {
			// A replica's statements belong to the primary source, on the
			// replica that ran them.
			src, err := build(rc.Name, rc, &queryTracer{m: m, source: data.PrimarySource, replica: rc.Name})
			if err != nil {
				return nil, err
			}
			t.replicas = append(t.replicas, &replica{src: src, zone: rc.Zone})
		}
		m.topo = t
	}
	m.metrics = newMetrics(s.mp, m.topo)
	return m, nil
}

func sortedNames(cfg Config) []string {
	names := []string{data.PrimarySource}
	for name := range cfg.Named {
		names = append(names, name)
	}
	sort.Strings(names[1:])
	return names
}

// Start opens every pool and checks that its database answers. Starting a
// Manager that is already started is an error; one that was stopped can be
// started again.
func (m *Manager) Start(ctx context.Context) error {
	if m.within != nil {
		return nil
	}
	m.life.Lock()
	defer m.life.Unlock()
	if m.started {
		return errors.New("pg: the Manager is already started")
	}
	for _, name := range m.names {
		src := m.sources[name]
		pool, err := pgxpool.NewWithConfig(ctx, src.config)
		if err == nil {
			src.pool.Store(pool)
			err = pool.Ping(ctx)
		}
		if err != nil {
			cerr := m.closePools(context.Background())
			return errors.Join(fmt.Errorf("pg: source %q: %w", name, classify(ctx, err)), cerr)
		}
	}
	if m.topo != nil {
		// A replica that can't be reached doesn't stop the start: reads
		// go to the primary until the tracker finds it healthy.
		for _, r := range m.topo.replicas {
			pool, err := pgxpool.NewWithConfig(ctx, r.src.config)
			if err != nil {
				cerr := m.closePools(context.Background())
				return errors.Join(fmt.Errorf("pg: replica %q: %w", r.src.name, err), cerr)
			}
			r.src.pool.Store(pool)
		}
		// The layout matters only where replicas wait on positions. It is
		// read here so reads can rely on it from the start, and the
		// primary's poller tries again until it can.
		if m.wal.Load() == nil {
			m.topo.layoutTried = m.topo.now()
			if err := m.readWALLayout(ctx); err != nil && ctx.Err() == nil {
				m.logger().WarnContext(ctx, "the primary's WAL layout can't be read, so a write ending at a WAL page may make its ReadYourWrites reads wait for more WAL; retrying", "error", err)
			}
		}
		if m.topo.maxLag < m.topo.staleAfter() {
			m.logger().WarnContext(ctx, "max_lag is below the tracker's staleness bound, which a caught-up replica's lag can reach; caught-up replicas may leave rotation", "max_lag", m.topo.maxLag, "staleness_bound", m.topo.staleAfter())
		}
		m.topo.start(m, m.sources[data.PrimarySource])
	}
	m.started = true
	return nil
}

// Stop closes the pools. A pool's Close waits for every connection to be
// returned, so Stop gives up when ctx ends.
func (m *Manager) Stop(ctx context.Context) error {
	m.life.Lock()
	defer m.life.Unlock()
	m.started = false
	if m.topo != nil {
		m.topo.stop()
	}
	return m.closePools(ctx)
}

func (m *Manager) closePools(ctx context.Context) error {
	var errs []error
	srcs := make([]*source, 0, len(m.names))
	for _, name := range m.names {
		srcs = append(srcs, m.sources[name])
	}
	if m.topo != nil {
		for _, r := range m.topo.replicas {
			srcs = append(srcs, r.src)
		}
	}
	for _, src := range srcs {
		pool := src.pool.Swap(nil)
		if pool == nil {
			continue
		}
		done := make(chan struct{})
		go func() { pool.Close(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("pg: closing source %q: connections still in use: %w", src.name, ctx.Err()))
		}
	}
	return errors.Join(errs...)
}

// Ping checks that the named source answers ("" is the primary).
func (m *Manager) Ping(ctx context.Context, name string) error {
	src, err := m.source(name)
	if err != nil {
		return err
	}
	if m.within != nil {
		return m.within.ping(ctx)
	}
	pool := src.pool.Load()
	if pool == nil {
		return errUnavailable
	}
	return classify(ctx, pool.Ping(ctx))
}

var errUnavailable = errs.New(errs.Unavailable, data.CodeUnavailable, "the database is unavailable")

// Pool returns the named source's pool ("" is the primary), for code that
// needs pgx itself, such as a migration tool. It is nil before Start, and
// for a Manager made with Within.
func (m *Manager) Pool(name string) *pgxpool.Pool {
	if name == "" {
		name = data.PrimarySource
	}
	if src := m.sources[name]; src != nil {
		return src.pool.Load()
	}
	return nil
}

func (m *Manager) source(name string) (*source, error) {
	if name == "" {
		name = data.PrimarySource
	}
	src := m.sources[name]
	if src == nil {
		return nil, errs.New(errs.Internal, data.CodeUnknownSource, fmt.Sprintf("no data source is named %q", name))
	}
	return src, nil
}

// within is the state of a Manager made with Within.
type within struct {
	tx   pgx.Tx
	turn chan struct{} // held by the open root transaction
	wait time.Duration
	mu   sync.Mutex
	held []uintptr // where the open root began
}

// Within returns a Manager that runs every transaction inside tx, which the
// caller owns and ends, so nothing it does outlives tx. testkit.DB uses it to
// give each test a transaction that is rolled back when the test ends.
//
// A root transaction is a savepoint of tx that behaves as a root: its Commit
// releases the savepoint and runs AfterCommit callbacks. Since tx is one
// connection, root transactions take turns: one waits for the open one to
// end, and fails after a while saying where that one began. For the same
// reason Independent fails inside a transaction, isolation levels are tx's
// own, and named sources don't exist. Reads run READ ONLY all the same.
func Within(tx pgx.Tx, opts ...Option) *Manager {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	s.defaults()
	cfg := tx.Conn().Config()
	m := &Manager{
		sources: map[string]*source{data.PrimarySource: {name: data.PrimarySource, db: cfg.Database, host: cfg.Host, port: int(cfg.Port)}},
		names:   []string{data.PrimarySource},
		leaks:   leaksError,
		within:  &within{tx: tx, turn: make(chan struct{}, 1), wait: 10 * time.Second},
		level:   defaultConsistency,
		clock:   s.clock,
		metrics: newMetrics(s.mp, nil),
	}
	m.setTelemetry(s.tp, s.log, s.models)
	return m
}

// QueryTracer returns the pgx tracer that makes the Manager's statement
// spans, for a connection the Manager didn't open itself, such as the one
// a Within manager runs on. It uses whatever tracer provider the Manager has
// when a statement runs.
func (m *Manager) QueryTracer() pgx.QueryTracer {
	return &queryTracer{m: m, source: data.PrimarySource}
}

// ping checks the test's connection when no transaction holds it; an open
// one is using the connection, which shows it works, and a ping then would
// race it.
func (w *within) ping(ctx context.Context) error {
	select {
	case w.turn <- struct{}{}:
	default:
		return nil
	}
	defer func() { <-w.turn }()
	return classify(ctx, w.tx.Conn().Ping(ctx))
}

// appTelemetry points a Use'd Manager at one app's tracer provider and
// logger while that app runs.
type appTelemetry struct {
	m   *Manager
	tel *telemetry
}

func (a *appTelemetry) Start(context.Context) error {
	a.m.pushTelemetry(a.tel)
	return nil
}

// Stop hands the Manager back to the latest app still running, or to its
// own telemetry when none is.
func (a *appTelemetry) Stop(context.Context) error {
	a.m.popTelemetry(a.tel)
	return nil
}

// managerView is the Manager as a data.Manager alone, so the kernel doesn't
// start and stop it a second time.
type managerView struct{ data.Manager }

// ModuleOption configures the module.
type ModuleOption func(*moduleSettings)

type moduleSettings struct {
	use *Manager
}

// Use makes the module provide m, as built, instead of one from the config
// section. The app never starts or stops m: its caller does. While the app
// runs, from its start to its stop, m sends spans and logs to the app's
// tracer provider and logger, and its spans name the app's models. With
// several apps on one Manager at once, the latest started of those still
// running gets them; with none, m uses its own. data.Manager is built once
// the app's telemetry is in place, so a provider that starts with it should
// depend on it rather than on *Manager. testkit.DB uses it.
func Use(m *Manager) ModuleOption { return func(s *moduleSettings) { s.use = m } }

// Module returns the data module: it provides *pg.Manager and data.Manager
// from the "pg" config section, registers a readiness gate per source, and
// tracks the transactions each operation begins.
//
// It also provides the model registry, *data.Models, built from the
// data.ModelInfo every module adds with galleon.Collect. An invalid model,
// or one collected as a *data.ModelInfo, fails boot (New); two that share
// a name or a table fail Start, when the registry is built.
func Module(opts ...ModuleOption) galleon.Module {
	var s moduleSettings
	for _, o := range opts {
		o(&s)
	}
	return galleon.ModuleFunc("pg", func(r *galleon.Registry) error {
		// Models collected by pointer, as *data.ModelInfo, are a collection
		// nothing takes, which fails boot.
		galleon.Collect[data.ModelInfo](r)
		galleon.Provide[*data.Models](r, func(models []data.ModelInfo) (*data.Models, error) { return data.NewModels(models...) })
		if s.use != nil {
			m := s.use
			// Supplied, so the kernel doesn't start or stop it.
			galleon.Supply[*Manager](r, m)
			galleon.Provide[*appTelemetry](r, func(tp trace.TracerProvider, log *slog.Logger, models *data.Models) *appTelemetry {
				return &appTelemetry{m: m, tel: newTelemetry(tp, log, models)}
			})
			// Built after the app's telemetry is in place, so what depends
			// on data.Manager starts with it. A managerView has no Start or
			// Stop for the kernel to call.
			galleon.Provide[data.Manager](r, func(*appTelemetry) data.Manager { return managerView{m} })
			r.Intercept(op.Before(op.StageRoute), func(next op.Invoker) op.Invoker {
				return func(ctx context.Context, call *op.Call) (any, error) { return m.track(ctx, call, next) }
			})
			return nil
		}
		cfg := galleon.Config[Config](r, "pg")
		var m *Manager
		galleon.Provide[*Manager](r, func(tp trace.TracerProvider, mp metric.MeterProvider, log *slog.Logger, clk clock.Clock, models *data.Models) (*Manager, error) {
			var err error
			m, err = New(*cfg, TracerProvider(tp), MeterProvider(mp), Logger(log), Clock(clk), Models(models))
			return m, err
		})
		galleon.Provide[data.Manager](r, func(m *Manager) data.Manager { return managerView{m} })
		for _, name := range sortedNames(*cfg) {
			r.Gate(name, func(ctx context.Context) error { return m.Ping(ctx, name) })
		}
		// Outermost, so the check runs once the whole pipeline, encode
		// included, has finished with the call.
		r.Intercept(op.Before(op.StageRoute), func(next op.Invoker) op.Invoker {
			return func(ctx context.Context, call *op.Call) (any, error) { return m.track(ctx, call, next) }
		})
		return nil
	})
}

// callers records the stack, for leak reports.
func callers() []uintptr {
	pcs := make([]uintptr, 32)
	return pcs[:runtime.Callers(2, pcs)]
}

// formatStack renders a stack recorded by callers, from where the caller
// called into this package.
func formatStack(pcs []uintptr) string {
	var b []byte
	frames := runtime.CallersFrames(pcs)
	for {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "github.com/thec1oud/galleon/pg.") {
			b = fmt.Appendf(b, "%s\n\t%s:%d\n", f.Function, f.File, f.Line)
		}
		if !more {
			break
		}
	}
	return string(b)
}

// firstFrame is the first frame of a stack recorded by callers outside
// this package, as "function file:line".
func firstFrame(pcs []uintptr) string {
	frames := runtime.CallersFrames(pcs)
	for {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "github.com/thec1oud/galleon/pg.") {
			return fmt.Sprintf("%s %s:%d", f.Function, f.File, f.Line)
		}
		if !more {
			return ""
		}
	}
}
