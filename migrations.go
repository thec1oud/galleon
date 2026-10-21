package galleon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/migrate"
)

// Env is the environment profile the app runs in. A profile changes only
// defaults, never semantics; today it sets the migration policy's.
type Env uint8

const (
	// Production is the default profile: migrations are verified at boot,
	// never applied (ModeVerify), up to Caution risk, and Unknown risk needs
	// approval.
	Production Env = iota
	// Development applies migrations at boot, whatever their risk.
	Development
	// Test is Development's defaults, for tests; testkit sets it.
	Test
)

func (e Env) String() string {
	switch e {
	case Development:
		return "development"
	case Test:
		return "test"
	}
	return "production"
}

// Environment sets the app's environment profile. The default is Production.
// It can only be set in code for now: no config section or environment
// variable chooses it.
func Environment(e Env) Option { return func(s *settings) { s.env = e } }

// Env returns the app's environment profile.
func (a *App) Env() Env { return a.settings.env }

// MigrationPolicy adjusts the profile's migration policy in code: edit
// changes only the fields it sets, and the rest keep the profile's
// defaults, so MigrationPolicy(func(p *migrate.Policy) { p.Mode =
// migrate.ModeApply }) in Production still refuses what Production refuses.
// The "migrate" config section then overrides each field it sets.
func MigrationPolicy(edit func(p *migrate.Policy)) Option {
	return func(s *settings) { s.migratePolicy = append(s.migratePolicy, edit) }
}

// MigratorOption configures a migrator's registration.
type MigratorOption func(*migratorReg)

// MigrateAfter runs the migrator after the named ones, such as a module's
// tables after the tables they reference.
func MigrateAfter(names ...string) MigratorOption {
	return func(m *migratorReg) { m.after = append(m.after, names...) }
}

// MigrateOrder sets the migrator's order key: among migrators MigrateAfter
// leaves free to run in any order, lower keys run first, then names in
// order. The default is 0.
func MigrateOrder(n int) MigratorOption { return func(m *migratorReg) { m.order = n } }

type migratorReg struct {
	name, module, site string
	ctor               reflect.Value
	params             []reflect.Type
	returnsErr         bool
	after              []string
	order              int
}

// Migrator registers migrator name, built at Start by ctor, a function
// whose parameters are provided types and which returns a migrate.Migrator,
// or (migrate.Migrator, error). It is built once every provider has
// started, so it can take a started data plane, such as *pg.Manager for
// its pool, and its Name must be name.
//
// At boot, after the providers start and before OnStart hooks run, the
// kernel acts on every migrator, in order, as the policy's mode says (see
// migrate.Mode), and gates readiness on each migrator's status as
// "migrate.<name>". Migrators apply under the coordination lock, so a
// module with a migrator needs coord.Locks.
func Migrator(r *Registry, name string, ctor any, opts ...MigratorOption) {
	if err := migrate.ValidName(name); err != nil {
		r.errorf("Migrator: %v", err)
		return
	}
	cv := reflect.ValueOf(ctor)
	if !cv.IsValid() || cv.Kind() != reflect.Func {
		r.errorf("Migrator %q: constructor must be a function, got %T", name, ctor)
		return
	}
	ct := cv.Type()
	switch {
	case ct.IsVariadic():
		r.errorf("Migrator %q: constructor must not be variadic", name)
		return
	case ct.NumOut() == 0 || ct.NumOut() > 2 || !ct.Out(0).Implements(migratorType) || ct.NumOut() == 2 && ct.Out(1) != errorType:
		r.errorf("Migrator %q: constructor must return a migrate.Migrator, or (migrate.Migrator, error)", name)
		return
	}
	m := &migratorReg{name: name, module: r.module, site: callerSite(2), ctor: cv, returnsErr: ct.NumOut() == 2}
	for i := 0; i < ct.NumIn(); i++ {
		m.params = append(m.params, ct.In(i))
	}
	for _, o := range opts {
		o(m)
	}
	for _, prev := range r.app.migratorRegs {
		if prev.name == name {
			r.errorf("migrator %q is already registered by module %q (%s)", name, prev.module, prev.site)
			return
		}
	}
	r.app.migratorRegs = append(r.app.migratorRegs, m)
	r.app.needs = append(r.app.needs, need{cap: coord.Locks, module: r.module})
}

var migratorType = reflect.TypeFor[migrate.Migrator]()

// migrateSection is the kernel's "migrate" config section. A field it sets
// overrides the profile's policy; one it leaves unset keeps it.
//
//	{"migrate": {"mode": "apply", "max_risk": "destructive", "allow_unknown": true,
//	             "approve": "schema=3f2a…,billing=91cc…"}}
//
// or the environment: <APP>_MIGRATE_MODE=apply, <APP>_MIGRATE_APPROVE=…
type migrateSection struct {
	// Mode is off, verify or apply.
	Mode string `json:"mode"`
	// MaxRisk is safe, caution, destructive or manual.
	MaxRisk       string `json:"max_risk"`
	AllowUnknown  bool   `json:"allow_unknown"`
	TolerateAhead bool   `json:"tolerate_ahead"`
	// Approve pins approved plan fingerprints, as name=fingerprint pairs
	// separated by commas: each the online plan's, of the changes pending
	// on the database (see migrate.Policy.Approved).
	Approve string `json:"approve"`
	// The durations are strings with a unit, such as "30s" or "10m".
	// LockTTL is at least 1s (the default is 30s); LockWait is 0, waiting as
	// long as the apply may, or at least 1s.
	LockTTL  durationText `json:"lock_ttl"`
	LockWait durationText `json:"lock_wait"`
	// Recheck is how often the readiness gate reads each migrator's status
	// again: at least 100ms, and 5s by default.
	Recheck durationText `json:"recheck"`
	// ApplyTimeout bounds applying at boot, waiting for the lock included:
	// at least 1s, and 10m by default.
	ApplyTimeout durationText `json:"apply_timeout"`
	// Debug records step descriptions, which may hold SQL, in spans and logs.
	Debug bool `json:"debug"`
}

func (s *migrateSection) Validate() error {
	var errs []error
	if s.Mode != "" {
		if _, err := migrate.ParseMode(s.Mode); err != nil {
			errs = append(errs, err)
		}
	}
	if s.MaxRisk != "" {
		if r, err := migrate.ParseRisk(s.MaxRisk); err != nil {
			errs = append(errs, err)
		} else if r == migrate.Unknown {
			errs = append(errs, errors.New("max_risk can't be unknown: set allow_unknown instead"))
		}
	}
	if _, err := parseApprovals(s.Approve); err != nil {
		errs = append(errs, err)
	}
	for _, d := range []struct {
		name string
		v    durationText
		min  time.Duration
		zero bool // 0 is allowed, meaning no limit
	}{
		{"lock_ttl", s.LockTTL, time.Second, false},
		{"lock_wait", s.LockWait, time.Second, true},
		{"recheck", s.Recheck, 100 * time.Millisecond, false},
		{"apply_timeout", s.ApplyTimeout, time.Second, false},
	} {
		if d.v == "" {
			continue
		}
		v, err := time.ParseDuration(string(d.v))
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %q is not a duration: write it with a unit, such as \"30s\"", d.name, string(d.v)))
		case v == 0 && d.zero:
		case v < d.min:
			errs = append(errs, fmt.Errorf("%s must be at least %s, not %s", d.name, d.min, v))
		}
	}
	return errors.Join(errs...)
}

// durationText is a duration in config: a string with a unit, such as
// "30s". A bare JSON number is kept as its text, so Validate can say which
// field lacks a unit rather than guessing it means nanoseconds.
type durationText string

func (d *durationText) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		s = string(b)
	}
	*d = durationText(s)
	return nil
}

// get returns the duration, or def when it is unset. Validate has checked it.
func (d durationText) get(def time.Duration) time.Duration {
	if d == "" {
		return def
	}
	v, _ := time.ParseDuration(string(d))
	return v
}

func parseApprovals(s string) (map[string]string, error) {
	out := map[string]string{}
	for pair := range strings.SplitSeq(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, fp, ok := strings.Cut(pair, "=")
		name, fp = strings.TrimSpace(name), strings.TrimSpace(fp)
		if !ok || name == "" || fp == "" {
			return nil, fmt.Errorf("approve: %q is not name=fingerprint", pair)
		}
		out[name] = fp
	}
	return out, nil
}

// migrations is the kernel's state for the app's migrators.
type migrations struct {
	policy       migrate.Policy
	recheck      time.Duration
	applyTimeout time.Duration
	order        []*migratorReg
	gates        map[string]*migrationGate
	handle       *Migrations
}

// configureMigrations orders the registered migrators, loads the policy
// and registers their gates. It runs in New, after modules register.
func (a *App) configureMigrations() {
	kernel := &Registry{app: a, module: kernelModule}
	if len(a.migratorRegs) == 0 {
		// The section is the kernel's, so it is claimed and checked even
		// in an app without migrators, such as one that has none yet.
		if _, ok := a.configRaw["migrate"]; ok {
			Config[migrateSection](kernel, "migrate")
		}
		return
	}
	policy := migrate.ProductionPolicy()
	if a.settings.env != Production {
		policy = migrate.DevelopmentPolicy()
	}
	for _, edit := range a.settings.migratePolicy {
		edit(&policy)
	}
	before := len(a.configReport)
	cfg := Config[migrateSection](kernel, "migrate")
	set := map[string]bool{}
	for _, v := range a.configReport[before:] {
		if v.Source != "default" {
			set[v.Field] = true
		}
	}
	if set["mode"] {
		policy.Mode, _ = migrate.ParseMode(cfg.Mode)
	}
	if set["max_risk"] {
		policy.MaxRisk, _ = migrate.ParseRisk(cfg.MaxRisk)
	}
	if set["allow_unknown"] {
		policy.AllowUnknown = cfg.AllowUnknown
	}
	if set["tolerate_ahead"] {
		policy.TolerateAhead = cfg.TolerateAhead
	}
	if set["approve"] {
		policy.Approved, _ = parseApprovals(cfg.Approve)
	}
	if set["lock_ttl"] {
		policy.LockTTL = cfg.LockTTL.get(0)
	}
	if set["lock_wait"] {
		policy.LockWait = cfg.LockWait.get(0)
	}
	if set["debug"] {
		policy.Debug = cfg.Debug
	}
	if policy.Mode == 0 {
		policy.Mode = migrate.ModeVerify
	}
	if policy.LockTTL != 0 && policy.LockTTL < time.Second || policy.LockWait < 0 {
		a.bootErrs = append(a.bootErrs, fmt.Errorf("migrate: the policy's LockTTL must be 0 (30s) or at least 1s, and its LockWait not negative; it has %s and %s", policy.LockTTL, policy.LockWait))
	}
	mg := &migrations{policy: policy, recheck: cfg.Recheck.get(5 * time.Second), applyTimeout: cfg.ApplyTimeout.get(10 * time.Minute), gates: map[string]*migrationGate{}, handle: a.migrationsHandle}
	for name := range policy.Approved {
		if !slices.ContainsFunc(a.migratorRegs, func(m *migratorReg) bool { return m.name == name }) {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("migrate: a fingerprint is approved for migrator %q, which isn't registered (typo?)", name))
		}
	}
	order, err := orderMigrators(a.migratorRegs)
	if err != nil {
		a.bootErrs = append(a.bootErrs, err)
		return
	}
	mg.order = order
	a.migrations = mg
	if policy.Mode == migrate.ModeOff {
		a.log.Warn("migrations are off: no migrator's status is checked before the app takes traffic", "migrators", len(order))
		return
	}
	for _, m := range order {
		g := &migrationGate{app: a, name: m.name, err: errors.New("not checked yet")}
		mg.gates[m.name] = g
		a.gates = append(a.gates, gate{name: "migrate." + m.name, check: g.check})
	}
}

// orderMigrators sorts migrators so each runs after those it names in
// MigrateAfter; among those free to run, by order key, then name.
func orderMigrators(regs []*migratorReg) ([]*migratorReg, error) {
	byName := map[string]*migratorReg{}
	for _, m := range regs {
		byName[m.name] = m
	}
	var errs []error
	waiting := map[*migratorReg]int{}
	next := map[string][]*migratorReg{}
	for _, m := range regs {
		for _, dep := range m.after {
			if byName[dep] == nil {
				errs = append(errs, fmt.Errorf("migrator %q (module %q, %s) runs after migrator %q, which isn't registered", m.name, m.module, m.site, dep))
				continue
			}
			waiting[m]++
			next[dep] = append(next[dep], m)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	less := func(x, y *migratorReg) int {
		return cmp.Or(cmp.Compare(x.order, y.order), cmp.Compare(x.name, y.name))
	}
	var ready, out []*migratorReg
	for _, m := range regs {
		if waiting[m] == 0 {
			ready = append(ready, m)
		}
	}
	for len(ready) > 0 {
		slices.SortFunc(ready, less)
		m := ready[0]
		ready = ready[1:]
		out = append(out, m)
		for _, n := range next[m.name] {
			if waiting[n]--; waiting[n] == 0 {
				ready = append(ready, n)
			}
		}
	}
	if len(out) < len(regs) {
		var stuck []string
		for _, m := range regs {
			if waiting[m] > 0 {
				stuck = append(stuck, m.name)
			}
		}
		slices.Sort(stuck)
		return nil, fmt.Errorf("migrators %s run after each other in a cycle", strings.Join(stuck, ", "))
	}
	return out, nil
}

// checkMigratorDeps reports constructor parameters nothing provides.
func (a *App) checkMigratorDeps() {
	for _, m := range a.migratorRegs {
		for _, dep := range m.params {
			if _, ok := a.providers[dep]; !ok {
				a.bootErrs = append(a.bootErrs, fmt.Errorf("migrator %q (module %q, %s) needs %s, but nothing provides it%s", m.name, m.module, m.site, dep, suggest(dep, a.providers)))
			}
		}
	}
}

// startMigrations builds the migrators and acts on them as the mode says.
// It runs in Start once every provider has started.
func (a *App) startMigrations(ctx context.Context) error {
	mg := a.migrations
	if mg == nil {
		return nil
	}
	var built []migrate.Migrator
	for _, m := range mg.order {
		args := make([]reflect.Value, len(m.params))
		for i, dep := range m.params {
			args[i] = a.providers[dep].value
		}
		out := m.ctor.Call(args)
		if m.returnsErr && !out[1].IsNil() {
			return fmt.Errorf("constructing migrator %q (module %q, %s): %w", m.name, m.module, m.site, out[1].Interface().(error))
		}
		mm, _ := out[0].Interface().(migrate.Migrator)
		if mm == nil || isNil(out[0]) {
			return fmt.Errorf("constructing migrator %q (module %q, %s): the constructor returned nil", m.name, m.module, m.site)
		}
		if mm.Name() != m.name {
			return fmt.Errorf("migrator %q (module %q, %s) is named %q by its constructor", m.name, m.module, m.site, mm.Name())
		}
		built = append(built, mm)
	}
	runner, err := migrate.NewRunner(migrate.Config{
		Locker:         a.backend.Locks(),
		Migrators:      built,
		Policy:         mg.policy,
		Logger:         a.log.With("component", "migrate"),
		TracerProvider: a.settings.tracerProvider,
		MeterProvider:  a.settings.meterProvider,
	})
	if err != nil {
		return err
	}
	for _, m := range built {
		if g := mg.gates[m.Name()]; g != nil {
			g.set(m, runner.Policy())
		}
	}
	refresh := func(ctx context.Context) {
		for _, g := range mg.gates {
			gctx, cancel := context.WithTimeout(ctx, a.settings.gateTimeout)
			g.refresh(gctx)
			cancel()
		}
	}
	mg.handle.set(runner, refresh)
	switch mg.policy.Mode {
	case migrate.ModeOff:
		return nil
	case migrate.ModeApply:
		actx, cancel := context.WithTimeout(ctx, mg.applyTimeout)
		defer cancel()
		if _, err := runner.Apply(actx); err != nil {
			return fmt.Errorf("applying migrations at boot (mode apply): %w", err)
		}
	}
	// Seed every gate now: the status after the apply, or, verifying, the
	// status as found.
	refresh(ctx)
	return nil
}

// migrationGate is one migrator's readiness gate. It reads the migrator's
// status at most once per recheck interval, so probes don't each query the
// database, and logs every change of verdict.
type migrationGate struct {
	app  *App
	name string

	mu      sync.Mutex
	m       migrate.Migrator
	policy  migrate.Policy
	err     error
	checked time.Time
	logged  string
	seeded  bool
}

func (g *migrationGate) set(m migrate.Migrator, p migrate.Policy) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.m, g.policy = m, p
}

func (g *migrationGate) check(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		return g.err
	}
	if !g.seeded || g.app.settings.clock.Since(g.checked) >= g.app.migrations.recheck {
		g.refreshLocked(ctx)
	}
	return g.err
}

func (g *migrationGate) refresh(ctx context.Context) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refreshLocked(ctx)
}

func (g *migrationGate) refreshLocked(ctx context.Context) {
	st, err := g.m.Status(ctx)
	if err != nil {
		g.err = errs.Wrap(err, errs.Unavailable, migrate.CodeNotReady, fmt.Sprintf("migrator %q: reading its status failed", g.name))
	} else {
		g.err = g.policy.Ready(g.name, st)
	}
	g.checked, g.seeded = g.app.settings.clock.Now(), true
	verdict := ""
	if g.err != nil {
		verdict = g.err.Error()
	}
	if verdict == g.logged {
		return
	}
	g.logged = verdict
	if g.err != nil {
		g.app.log.Warn("migration gate fails: the app takes no traffic until it passes", "migrator", g.name, "reason", verdict)
	} else {
		g.app.log.Info("migration gate passes", "migrator", g.name, "state", st.State.String())
	}
}

// Migrations is the app's operator surface for its migrators: status, plan
// and apply, as `migrate status|plan|apply` will run them, and Done for app
// code waiting on a background task. The kernel provides it, so a
// constructor can take it; it works once the app has started.
type Migrations struct {
	mu      sync.Mutex
	runner  *migrate.Runner
	refresh func(ctx context.Context) // rereads every gate
}

func (m *Migrations) set(r *migrate.Runner, refresh func(ctx context.Context)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runner, m.refresh = r, refresh
}

// Runner returns the app's migration runner, or an error before Start or
// when the app has no migrators.
func (m *Migrations) Runner() (*migrate.Runner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runner == nil {
		return nil, errs.New(errs.FailedPrecondition, migrate.CodeNotSupported, "the app has no migrators, or hasn't started")
	}
	return m.runner, nil
}

// Status reads every migrator's status, drift and background tasks.
func (m *Migrations) Status(ctx context.Context) ([]migrate.Report, error) {
	r, err := m.Runner()
	if err != nil {
		return nil, err
	}
	return r.Status(ctx), nil
}

// Plan asks every migrator what Apply would do; offline, without a database.
func (m *Migrations) Plan(ctx context.Context, offline bool) ([]migrate.PlanReport, error) {
	r, err := m.Runner()
	if err != nil {
		return nil, err
	}
	return r.Plan(ctx, offline), nil
}

// Apply applies every migrator in order under the cluster lock, whatever
// the boot mode, and stops at the first failure. Then this app's readiness
// gates read every status again, rather than at their next recheck.
func (m *Migrations) Apply(ctx context.Context) ([]migrate.ApplyReport, error) {
	r, err := m.Runner()
	if err != nil {
		return nil, err
	}
	reps, err := r.Apply(ctx)
	m.mu.Lock()
	refresh := m.refresh
	m.mu.Unlock()
	refresh(ctx)
	return reps, err
}

// Done asks migrator name whether its task or change id is finished.
func (m *Migrations) Done(ctx context.Context, name, id string) (bool, error) {
	r, err := m.Runner()
	if err != nil {
		return false, err
	}
	return r.Done(ctx, name, id)
}

// Migrations returns the app's operator surface for its migrators.
func (a *App) Migrations() *Migrations { return a.migrationsHandle }
