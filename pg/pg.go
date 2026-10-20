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
// the SQL text, never its arguments.
package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

// Manager is the pgx data.Manager. Build it with New, or Within for tests.
type Manager struct {
	sources map[string]*source
	names   []string // sorted, the primary first
	tel     atomic.Pointer[telemetry]
	retry   data.RetryPolicy
	leaks   string
	within  *within
}

var _ data.Manager = (*Manager)(nil)

// telemetry is where a Manager sends spans and logs. A Within manager gets
// the app's when the module is installed, after it was built.
type telemetry struct {
	tracer trace.Tracer
	log    *slog.Logger
}

func (m *Manager) setTelemetry(tp trace.TracerProvider, log *slog.Logger) {
	m.tel.Store(&telemetry{tracer: tp.Tracer(tracerName), log: log})
}

func (m *Manager) tracer() trace.Tracer { return m.tel.Load().tracer }
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
	tp  trace.TracerProvider
	log *slog.Logger
}

// TracerProvider and Logger replace the defaults (no tracing, the default
// logger); the module sets them from the app.
func TracerProvider(tp trace.TracerProvider) Option { return func(s *settings) { s.tp = tp } }
func Logger(l *slog.Logger) Option                  { return func(s *settings) { s.log = l } }

func (s *settings) defaults() {
	if s.tp == nil {
		s.tp = noop.NewTracerProvider()
	}
	if s.log == nil {
		s.log = slog.Default()
	}
}

// New builds a Manager for cfg. It doesn't connect: Start opens the pools.
func New(cfg Config, opts ...Option) (*Manager, error) {
	if cfg.Leaks == "" {
		cfg.Leaks = leaksError
	}
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
	}
	m.setTelemetry(s.tp, s.log)
	add := func(name string, src Source) {
		pc, _ := src.poolConfig() // Validate has parsed it
		pc.ConnConfig.Tracer = &queryTracer{m: m, source: name}
		m.sources[name] = &source{name: name, config: pc, db: pc.ConnConfig.Database, host: pc.ConnConfig.Host, port: int(pc.ConnConfig.Port)}
	}
	add(data.Primary, cfg.Primary)
	for name, src := range cfg.Named {
		add(name, src)
	}
	m.names = sortedNames(cfg)
	return m, nil
}

func sortedNames(cfg Config) []string {
	names := []string{data.Primary}
	for name := range cfg.Named {
		names = append(names, name)
	}
	sort.Strings(names[1:])
	return names
}

// Start opens every pool and checks that its database answers.
func (m *Manager) Start(ctx context.Context) error {
	if m.within != nil {
		return nil
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
	return nil
}

// Stop closes the pools. A pool's Close waits for every connection to be
// returned, so Stop gives up when ctx ends.
func (m *Manager) Stop(ctx context.Context) error { return m.closePools(ctx) }

func (m *Manager) closePools(ctx context.Context) error {
	var errs []error
	for _, name := range m.names {
		pool := m.sources[name].pool.Swap(nil)
		if pool == nil {
			continue
		}
		done := make(chan struct{})
		go func() { pool.Close(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("pg: closing source %q: connections still in use: %w", name, ctx.Err()))
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
		name = data.Primary
	}
	if src := m.sources[name]; src != nil {
		return src.pool.Load()
	}
	return nil
}

func (m *Manager) source(name string) (*source, error) {
	if name == "" {
		name = data.Primary
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
		sources: map[string]*source{data.Primary: {name: data.Primary, db: cfg.Database, host: cfg.Host, port: int(cfg.Port)}},
		names:   []string{data.Primary},
		leaks:   leaksError,
		within:  &within{tx: tx, turn: make(chan struct{}, 1), wait: 10 * time.Second},
	}
	m.setTelemetry(s.tp, s.log)
	return m
}

// QueryTracer returns the pgx tracer that makes the Manager's statement
// spans, for a connection the Manager didn't open itself, such as the one
// a Within manager runs on. It uses whatever tracer provider the Manager has
// when a statement runs.
func (m *Manager) QueryTracer() pgx.QueryTracer { return &queryTracer{m: m, source: data.Primary} }

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

// managerView is the Manager as a data.Manager alone, so the kernel doesn't
// start and stop it a second time.
type managerView struct{ data.Manager }

// ModuleOption configures the module.
type ModuleOption func(*moduleSettings)

type moduleSettings struct {
	use *Manager
}

// Use makes the module provide m, as built, instead of one from the config
// section; the caller starts and stops it. testkit.DB uses it.
func Use(m *Manager) ModuleOption { return func(s *moduleSettings) { s.use = m } }

// Module returns the data module: it provides *pg.Manager and data.Manager
// from the "pg" config section, registers a readiness gate per source, and
// tracks the transactions each operation begins.
func Module(opts ...ModuleOption) galleon.Module {
	var s moduleSettings
	for _, o := range opts {
		o(&s)
	}
	return galleon.ModuleFunc("pg", func(r *galleon.Registry) error {
		if s.use != nil {
			m := s.use
			// Provided rather than supplied, so it takes the app's tracer
			// provider and logger. Its Start and Stop leave a Within
			// manager's connection alone.
			galleon.Provide[*Manager](r, func(tp trace.TracerProvider, log *slog.Logger) *Manager {
				m.setTelemetry(tp, log)
				return m
			})
			galleon.Provide[data.Manager](r, func(m *Manager) data.Manager { return managerView{m} })
			r.Intercept(op.Before(op.StageRoute), func(next op.Invoker) op.Invoker {
				return func(ctx context.Context, call *op.Call) (any, error) { return m.track(ctx, call, next) }
			})
			return nil
		}
		cfg := galleon.Config[Config](r, "pg")
		var m *Manager
		galleon.Provide[*Manager](r, func(tp trace.TracerProvider, log *slog.Logger) (*Manager, error) {
			var err error
			m, err = New(*cfg, TracerProvider(tp), Logger(log))
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
