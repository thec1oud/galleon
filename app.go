// Package galleon is the kernel: it runs modules, wires dependencies, loads
// configuration, owns the lifecycle and checks coordination needs. It knows
// nothing about HTTP routing, databases or identity; those are planes built on it.
package galleon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/coord"
)

// Deployment declares how many instances of the app will run. It lets boot
// refuse a process-scoped coordination backend in a multi-node deployment.
type Deployment uint8

const (
	DeploymentUnspecified Deployment = iota
	SingleNode
	MultiNode
)

func (d Deployment) String() string {
	switch d {
	case SingleNode:
		return "single-node"
	case MultiNode:
		return "multi-node"
	default:
		return "unspecified"
	}
}

type settings struct {
	name            string
	backend         coord.Backend
	modules         []Module
	logger          *slog.Logger
	deployment      Deployment
	allowPerProcess bool
	configJSON      []byte
	configFile      string
	startTimeout    time.Duration
	stopTimeout     time.Duration
	drainGrace      time.Duration
	gateTimeout     time.Duration
	clock           clock.Clock
	tracerProvider  trace.TracerProvider
	meterProvider   metric.MeterProvider
	ownTracer       bool
	ownMeter        bool
}

// Option configures an App.
type Option func(*settings)

func Name(name string) Option { return func(s *settings) { s.name = name } }

// Coordination sets the coordination backend. It is required: there is no default.
// Use coord/local's New() for a single-node deployment.
func Coordination(b coord.Backend) Option { return func(s *settings) { s.backend = b } }

func Modules(m ...Module) Option { return func(s *settings) { s.modules = append(s.modules, m...) } }

// Logger sets the logger. The kernel wraps its handler with ContextHandler and
// adds the app name and node to every record.
func Logger(l *slog.Logger) Option { return func(s *settings) { s.logger = l } }

// Clock sets the clock the app provides to modules. The default is clock.System().
func Clock(c clock.Clock) Option { return func(s *settings) { s.clock = c } }

func Deploy(d Deployment) Option { return func(s *settings) { s.deployment = d } }

// AllowPerProcessGuarantees lets a multi-node deployment run on a process-scoped
// backend. Every affected guarantee is logged at boot. Use with care.
func AllowPerProcessGuarantees() Option { return func(s *settings) { s.allowPerProcess = true } }

// ConfigJSON supplies configuration as a JSON object keyed by section.
func ConfigJSON(b []byte) Option { return func(s *settings) { s.configJSON = b } }

// ConfigFile reads configuration from a JSON file keyed by section.
func ConfigFile(path string) Option { return func(s *settings) { s.configFile = path } }

func StartTimeout(d time.Duration) Option { return func(s *settings) { s.startTimeout = d } }
func StopTimeout(d time.Duration) Option  { return func(s *settings) { s.stopTimeout = d } }

// DrainGrace is how long the app keeps serving after readiness turns off, so
// load balancers stop sending traffic before listeners close.
func DrainGrace(d time.Duration) Option { return func(s *settings) { s.drainGrace = d } }

type need struct {
	cap    coord.Capability
	module string
}

type require struct {
	module, needs, site string
}

type gate struct {
	name  string
	check func(ctx context.Context) error
}

type hook struct {
	module, site string
	fn           func(ctx context.Context) error
}

type state uint8

const (
	stateBuilt state = iota
	stateStarting
	stateStarted
	stateDraining
	stateStopped
)

// App is a built, validated application. Create it with New.
type App struct {
	name     string
	log      *slog.Logger
	level    *slog.LevelVar
	node     Node
	backend  coord.Backend
	settings settings

	providers     map[reflectType]*provider
	providerOrder []*provider
	buildOrder    []*provider

	configRaw    map[string]json.RawMessage
	configOwners map[string]string
	configReport []ConfigValue

	needs      []need
	requires   []require
	gates      []gate
	startHooks []hook
	stopHooks  []hook
	drainHooks []hook

	bootErrs []error

	mu           sync.Mutex
	state        state
	started      []*provider // providers whose Start succeeded, in order
	fullyStarted bool        // every provider and start hook succeeded
}

// New configures, registers and validates the whole app. Every wiring, config
// and coordination problem is reported here, together, before anything starts.
func New(opts ...Option) (*App, error) {
	s := settings{
		startTimeout: 30 * time.Second,
		stopTimeout:  30 * time.Second,
		drainGrace:   5 * time.Second,
		gateTimeout:  2 * time.Second,
	}
	for _, o := range opts {
		o(&s)
	}
	if s.name == "" {
		s.name = "app"
	}
	if s.clock == nil {
		s.clock = clock.System()
	}
	if s.tracerProvider == nil {
		s.tracerProvider = otel.GetTracerProvider()
	}
	if s.meterProvider == nil {
		s.meterProvider = otel.GetMeterProvider()
	}
	var level *slog.LevelVar
	if s.logger == nil {
		level = new(slog.LevelVar)
		s.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	}
	node, nodeErr := currentNode()
	a := &App{
		name:         s.name,
		log:          slog.New(ContextHandler(s.logger.Handler())).With("app", s.name, "node", node.String()),
		level:        level,
		node:         node,
		backend:      s.backend,
		settings:     s,
		providers:    map[reflectType]*provider{},
		configRaw:    map[string]json.RawMessage{},
		configOwners: map[string]string{},
	}

	if nodeErr != nil {
		a.bootErrs = append(a.bootErrs, nodeErr)
	}
	if err := a.loadConfig(); err != nil {
		return nil, a.failBoot(err)
	}

	kernel := &Registry{app: a, module: "galleon"}
	Supply[*slog.Logger](kernel, a.log)
	Supply[clock.Clock](kernel, s.clock)
	Supply[Node](kernel, node)
	Supply[trace.TracerProvider](kernel, s.tracerProvider)
	Supply[metric.MeterProvider](kernel, s.meterProvider)
	if a.backend != nil {
		Supply[coord.Backend](kernel, a.backend)
	}

	seen := map[string]bool{}
	for _, m := range s.modules {
		name := m.Name()
		if seen[name] {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("module %q is installed twice", name))
			continue
		}
		seen[name] = true
		r := &Registry{app: a, module: name}
		if err := m.Register(r); err != nil {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("module %q: %w", name, err))
		}
	}
	for _, rq := range a.requires {
		if !seen[rq.needs] {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("module %q requires module %q, which is not installed (%s)", rq.module, rq.needs, rq.site))
		}
	}
	for section := range a.configRaw {
		if _, ok := a.configOwners[section]; !ok {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("config section %q is not used by any module (typo?)", section))
		}
	}
	a.checkCoordination()

	if order, err := a.resolveGraph(); err != nil {
		a.bootErrs = append(a.bootErrs, err)
	} else {
		a.buildOrder = order
	}
	if len(a.bootErrs) > 0 {
		return nil, a.failBoot(&BootError{Errs: a.bootErrs})
	}
	return a, nil
}

// failBoot releases the telemetry providers the app was handed, since no App
// is returned to shut them down later.
func (a *App) failBoot(err error) error {
	if terr := shutdownTelemetry(context.Background(), &a.settings); terr != nil {
		return errors.Join(err, terr)
	}
	return err
}

// BootError collects every problem found while building the app.
type BootError struct{ Errs []error }

func (e *BootError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "galleon: %d boot problem(s):", len(e.Errs))
	for _, err := range e.Errs {
		b.WriteString("\n  - ")
		b.WriteString(strings.ReplaceAll(err.Error(), "\n", "\n    "))
	}
	return b.String()
}

func (e *BootError) Unwrap() []error { return e.Errs }

func (a *App) loadConfig() error {
	raw := a.settings.configJSON
	if a.settings.configFile != "" {
		f, err := os.Open(a.settings.configFile)
		if err != nil {
			return fmt.Errorf("galleon: reading config: %w", err)
		}
		defer f.Close()
		if raw, err = io.ReadAll(f); err != nil {
			return fmt.Errorf("galleon: reading config: %w", err)
		}
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &a.configRaw); err != nil {
		return fmt.Errorf("galleon: config must be a JSON object keyed by section: %w", err)
	}
	return nil
}

func (a *App) checkCoordination() {
	if a.backend == nil {
		a.bootErrs = append(a.bootErrs, errors.New("no coordination backend configured. Galleon has no default: "+
			"pass galleon.Coordination(local.New()) for a single node, or a cluster backend (pg, redis, nats, etcd) for several"))
		return
	}
	caps := a.backend.Capabilities()
	for _, n := range a.needs {
		if !caps.Has(n.cap) {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("module %q needs coordination capability %q, which backend %q does not provide", n.module, n.cap, caps.Name))
		}
	}
	if caps.Scope == coord.ScopeCluster || len(a.needs) == 0 {
		return
	}
	switch a.settings.deployment {
	case MultiNode:
		if !a.settings.allowPerProcess {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("deployment is multi-node but coordination backend %q only guarantees within one process; "+
				"use a cluster backend, or set galleon.AllowPerProcessGuarantees() to accept per-process guarantees", caps.Name))
			return
		}
		for _, n := range a.needs {
			a.log.Warn("coordination guarantee is per-process in a multi-node deployment", "capability", n.cap, "module", n.module, "backend", caps.Name)
		}
	case DeploymentUnspecified:
		a.log.Info("coordination guarantees are per-process; declare galleon.Deploy(galleon.SingleNode) to confirm", "backend", caps.Name)
	}
}

// Start builds and starts every provider in dependency order, then runs start
// hooks. If anything fails, everything already started is stopped in reverse.
func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.state != stateBuilt {
		a.mu.Unlock()
		return errors.New("galleon: Start called twice")
	}
	a.state = stateStarting
	a.mu.Unlock()

	fail := func(err error) error {
		stopCtx, cancel := context.WithTimeout(context.Background(), a.settings.stopTimeout)
		defer cancel()
		if serr := a.stopAll(stopCtx); serr != nil {
			err = errors.Join(err, serr)
		}
		if terr := shutdownTelemetry(stopCtx, &a.settings); terr != nil {
			err = errors.Join(err, terr)
		}
		a.mu.Lock()
		a.state = stateStopped
		a.mu.Unlock()
		return err
	}

	for _, p := range a.buildOrder {
		if err := a.build(p); err != nil {
			return fail(err)
		}
		if s, ok := p.value.Interface().(Starter); ok && p.ctor.IsValid() {
			sctx, cancel := context.WithTimeout(ctx, a.settings.startTimeout)
			err := s.Start(sctx)
			cancel()
			if err != nil {
				return fail(fmt.Errorf("starting %s: %w", p.describe(), err))
			}
		}
		a.started = append(a.started, p)
	}
	for _, h := range a.startHooks {
		hctx, cancel := context.WithTimeout(ctx, a.settings.startTimeout)
		err := h.fn(hctx)
		cancel()
		if err != nil {
			return fail(fmt.Errorf("start hook of module %q (%s): %w", h.module, h.site, err))
		}
	}

	a.mu.Lock()
	a.state = stateStarted
	a.fullyStarted = true
	a.mu.Unlock()
	a.log.Info("started", "providers", len(a.started), "deployment", a.settings.deployment.String(), "coordination", a.backend.Capabilities().Name)
	return nil
}

// Ready runs every readiness gate. It returns nil only when the app is started,
// not draining, and every gate passes.
func (a *App) Ready(ctx context.Context) error {
	a.mu.Lock()
	st := a.state
	a.mu.Unlock()
	switch st {
	case stateStarted:
	case stateDraining:
		return errors.New("draining")
	default:
		return errors.New("not started")
	}
	var errs []error
	for _, g := range a.gates {
		gctx, cancel := context.WithTimeout(ctx, a.settings.gateTimeout)
		err := g.check(gctx)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", g.name, err))
		}
	}
	return errors.Join(errs...)
}

// Shutdown drains then stops the app: readiness off, grace period, drain hooks,
// stop hooks, then providers in reverse dependency order.
func (a *App) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	switch a.state {
	case stateStarted:
	case stateBuilt:
		// Never started: nothing to drain or stop, but the telemetry
		// providers the app was handed still need flushing.
		a.state = stateStopped
		a.mu.Unlock()
		return shutdownTelemetry(ctx, &a.settings)
	default:
		a.mu.Unlock()
		return nil
	}
	a.state = stateDraining
	a.mu.Unlock()
	a.log.Info("draining", "grace", a.settings.drainGrace)

	if a.settings.drainGrace > 0 {
		t := time.NewTimer(a.settings.drainGrace)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
		}
	}
	var errs []error
	for i := len(a.drainHooks) - 1; i >= 0; i-- {
		if err := a.runHook(ctx, a.drainHooks[i], "drain"); err != nil {
			errs = append(errs, err)
		}
	}
	if err := a.stopAll(ctx); err != nil {
		errs = append(errs, err)
	}
	a.mu.Lock()
	a.state = stateStopped
	a.mu.Unlock()
	a.log.Info("stopped")
	if err := shutdownTelemetry(ctx, &a.settings); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (a *App) stopAll(ctx context.Context) error {
	var errs []error
	// Stop hooks pair with a fully started app; after a failed start only the
	// providers that did start are stopped.
	if a.fullyStarted {
		for i := len(a.stopHooks) - 1; i >= 0; i-- {
			if err := a.runHook(ctx, a.stopHooks[i], "stop"); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for i := len(a.started) - 1; i >= 0; i-- {
		p := a.started[i]
		s, ok := p.value.Interface().(Stopper)
		if !ok || !p.ctor.IsValid() {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, a.settings.stopTimeout)
		err := s.Stop(sctx)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("stopping %s: %w", p.describe(), err))
		}
	}
	a.started = nil
	return errors.Join(errs...)
}

func (a *App) runHook(ctx context.Context, h hook, kind string) error {
	hctx, cancel := context.WithTimeout(ctx, a.settings.stopTimeout)
	defer cancel()
	if err := h.fn(hctx); err != nil {
		return fmt.Errorf("%s hook of module %q (%s): %w", kind, h.module, h.site, err)
	}
	return nil
}

// Run starts the app, waits for ctx to end or for SIGINT/SIGTERM, then shuts down.
func (a *App) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), a.settings.drainGrace+a.settings.stopTimeout)
	defer cancel()
	return a.Shutdown(sctx)
}

// Name returns the app's name.
func (a *App) Name() string { return a.name }

// Node returns this process's identity.
func (a *App) Node() Node { return a.node }

// Coordination returns the configured backend.
func (a *App) Coordination() coord.Backend { return a.backend }
