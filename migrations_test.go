package galleon_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/migrate"
)

// stub is a migrator whose database is a flag; applying appends its name to
// a shared log.
type stub struct {
	name string
	mu   *sync.Mutex
	log  *[]string
	up   bool
	opts migrate.ApplyOptions
	fail error
}

func newStub(name string, log *[]string, mu *sync.Mutex) *stub {
	return &stub{name: name, log: log, mu: mu}
}

func (s *stub) Name() string { return s.name }
func (s *stub) Status(context.Context) (migrate.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.up {
		return migrate.Status{State: migrate.UpToDate, Expected: "1", Current: "1"}, nil
	}
	return migrate.Status{State: migrate.Pending, Expected: "1", Current: "none"}, nil
}
func (s *stub) Apply(_ context.Context, opts migrate.ApplyOptions) (migrate.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return migrate.Result{}, s.fail
	}
	s.opts = opts
	*s.log = append(*s.log, s.name)
	s.up = true
	return migrate.Result{Applied: []migrate.StepInfo{{ID: "1"}}}, nil
}

func migratorModule(name string, register func(r *galleon.Registry)) galleon.Module {
	return galleon.ModuleFunc(name, func(r *galleon.Registry) error { register(r); return nil })
}

func TestMigratorsRunInDeclaredOrderAcrossModules(t *testing.T) {
	var mu sync.Mutex
	var log []string
	m := map[string]*stub{}
	for _, n := range []string{"billing", "orders", "audit", "zeta"} {
		m[n] = newStub(n, &log, &mu)
	}
	ctor := func(n string) func() migrate.Migrator { return func() migrate.Migrator { return m[n] } }
	mods := []galleon.Module{
		migratorModule("orders", func(r *galleon.Registry) {
			galleon.Migrator(r, "orders", ctor("orders"), galleon.MigrateAfter("billing"))
		}),
		migratorModule("billing", func(r *galleon.Registry) {
			galleon.Migrator(r, "billing", ctor("billing"))
			galleon.Migrator(r, "zeta", ctor("zeta"), galleon.MigrateOrder(-1))
		}),
		migratorModule("audit", func(r *galleon.Registry) {
			galleon.Migrator(r, "audit", ctor("audit"), galleon.MigrateAfter("orders"))
		}),
	}
	app, err := galleon.New(append(base(mods...), galleon.Environment(galleon.Development))...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	if got := strings.Join(log, ","); got != "zeta,billing,orders,audit" {
		t.Fatalf("applied in order %s", got)
	}
	if m["orders"].opts.Lock == nil {
		t.Error("the kernel applied without a lease")
	}
	reps, err := app.Migrations().Status(context.Background())
	if err != nil || len(reps) != 4 || reps[0].Name != "zeta" {
		t.Fatalf("%v, %v", reps, err)
	}
	if err := app.Ready(context.Background()); err != nil {
		t.Errorf("not ready after applying: %v", err)
	}
}

func TestMigratorRegistrationProblemsFailBoot(t *testing.T) {
	var mu sync.Mutex
	var log []string
	ok := func() migrate.Migrator { return newStub("a", &log, &mu) }
	for name, tc := range map[string]struct {
		register func(r *galleon.Registry)
		says     string
	}{
		"bad name":       {func(r *galleon.Registry) { galleon.Migrator(r, "Bad", ok) }, "must match"},
		"not a function": {func(r *galleon.Registry) { galleon.Migrator(r, "a", 42) }, "must be a function"},
		"wrong return":   {func(r *galleon.Registry) { galleon.Migrator(r, "a", func() string { return "" }) }, "must return a migrate.Migrator"},
		"twice": {func(r *galleon.Registry) {
			galleon.Migrator(r, "a", ok)
			galleon.Migrator(r, "a", ok)
		}, `migrator "a" is already registered`},
		"unknown after": {func(r *galleon.Registry) { galleon.Migrator(r, "a", ok, galleon.MigrateAfter("b")) }, `runs after migrator "b", which isn't registered`},
		"cycle": {func(r *galleon.Registry) {
			galleon.Migrator(r, "a", ok, galleon.MigrateAfter("b"))
			galleon.Migrator(r, "b", ok, galleon.MigrateAfter("a"))
			galleon.Migrator(r, "c", ok)
		}, "migrators a, b run after each other in a cycle"},
		"missing dependency": {func(r *galleon.Registry) {
			galleon.Migrator(r, "a", func(*DB) migrate.Migrator { return nil })
		}, `migrator "a" (module "m"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := galleon.New(base(migratorModule("m", tc.register))...)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("want %q, got %v", tc.says, err)
			}
		})
	}
}

func TestAMigratorMustBeNamedAsRegistered(t *testing.T) {
	var mu sync.Mutex
	var log []string
	mod := migratorModule("m", func(r *galleon.Registry) {
		galleon.Migrator(r, "schema", func() migrate.Migrator { return newStub("other", &log, &mu) })
	})
	app, err := galleon.New(base(mod)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err == nil || !strings.Contains(err.Error(), `is named "other"`) {
		t.Fatalf("%v", err)
	}
}

func TestMigratorsNeedClusterLocks(t *testing.T) {
	var mu sync.Mutex
	var log []string
	mod := migratorModule("m", func(r *galleon.Registry) {
		galleon.Migrator(r, "schema", func() migrate.Migrator { return newStub("schema", &log, &mu) })
	})
	_, err := galleon.New(quiet(), galleon.Coordination(local.New()), galleon.Deploy(galleon.MultiNode), galleon.Modules(mod))
	if err == nil || !strings.Contains(err.Error(), "multi-node") {
		t.Fatalf("a per-process lock was accepted for several nodes: %v", err)
	}
}

func TestProductionVerifiesAndKeepsAPendingNodeOutOfTraffic(t *testing.T) {
	var mu sync.Mutex
	var log []string
	s := newStub("schema", &log, &mu)
	serving := make(chan struct{})
	mod := migratorModule("m", func(r *galleon.Registry) {
		galleon.Migrator(r, "schema", func() migrate.Migrator { return s })
		r.OnReady(func(context.Context) error { close(serving); return nil })
	})
	var logs bytes.Buffer
	lockedLogs := &syncWriter{w: &logs}
	opts := append(base(mod), galleon.Logger(slog.New(slog.NewTextHandler(lockedLogs, nil))), galleon.ConfigJSON([]byte(`{"migrate": {"allow_unknown": true}}`)))
	app, err := galleon.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	if len(log) != 0 {
		t.Fatal("production applied at boot")
	}
	srv := httptest.NewServer(app.HealthHandler(true))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || !strings.Contains(body.String(), `migrate.schema: migrate.not_ready: migrator "schema": changes are pending`) {
		t.Fatalf("%d %s", resp.StatusCode, body.String())
	}
	if !strings.Contains(lockedLogs.String(), "migration gate fails") {
		t.Errorf("the reason isn't logged:\n%s", lockedLogs.String())
	}
	select {
	case <-serving:
		t.Fatal("listeners started while the gate fails")
	default:
	}

	// Another node applies; the gate reads the status again and passes.
	if _, err := app.Migrations().Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.Ready(context.Background()); err != nil {
		t.Fatalf("still not ready after the apply: %v", err)
	}
	if !strings.Contains(lockedLogs.String(), "migration gate passes") {
		t.Errorf("passing isn't logged:\n%s", lockedLogs.String())
	}
	select {
	case <-serving:
	case <-time.After(10 * time.Second):
		t.Fatal("listeners didn't start once the gate passed")
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.String()
}

func TestModeOffChecksNothing(t *testing.T) {
	var mu sync.Mutex
	var log []string
	s := newStub("schema", &log, &mu)
	mod := migratorModule("m", func(r *galleon.Registry) {
		galleon.Migrator(r, "schema", func() migrate.Migrator { return s })
	})
	t.Setenv("ORDERS_MIGRATE_MODE", "off")
	app, err := galleon.New(append(base(mod), galleon.Environment(galleon.Development))...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	if len(log) != 0 {
		t.Error("mode off applied")
	}
	if err := app.Ready(context.Background()); err != nil {
		t.Errorf("mode off gates readiness: %v", err)
	}
}

func TestAFailedApplyAtBootFailsStart(t *testing.T) {
	var mu sync.Mutex
	var log []string
	s := newStub("schema", &log, &mu)
	s.fail = errors.New("syntax error")
	started := false
	mod := migratorModule("m", func(r *galleon.Registry) {
		galleon.Migrator(r, "schema", func() migrate.Migrator { return s })
		r.OnStart(func(context.Context) error { started = true; return nil })
	})
	app, err := galleon.New(append(base(mod), galleon.Environment(galleon.Test))...)
	if err != nil {
		t.Fatal(err)
	}
	err = app.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "applying migrations at boot (mode apply)") || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("%v", err)
	}
	if started {
		t.Error("OnStart ran after the failed apply")
	}
}

func TestTheMigrateSectionOverridesTheProfile(t *testing.T) {
	var mu sync.Mutex
	var log []string
	s := newStub("schema", &log, &mu)
	mod := migratorModule("m", func(r *galleon.Registry) {
		galleon.Migrator(r, "schema", func() migrate.Migrator { return s })
	})
	cfg := `{"migrate": {"mode": "apply", "max_risk": "destructive", "allow_unknown": true}}`
	app, err := galleon.New(append(base(mod), galleon.ConfigJSON([]byte(cfg)))...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	if len(log) != 1 || s.opts.MaxRisk != migrate.Destructive || !s.opts.AllowUnknown {
		t.Fatalf("applied %v with %+v", log, s.opts)
	}
	r, err := app.Migrations().Runner()
	if err != nil || r.Policy().Mode != migrate.ModeApply {
		t.Fatalf("%v, %v", r, err)
	}

	// Production refuses Unknown unless told otherwise.
	s2 := newStub("schema", &log, &mu)
	mod2 := migratorModule("m", func(r *galleon.Registry) {
		galleon.Migrator(r, "schema", func() migrate.Migrator { return s2 })
	})
	app2, err := galleon.New(append(base(mod2), galleon.ConfigJSON([]byte(`{"migrate": {"mode": "apply"}}`)))...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app2.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "can't plan") {
		t.Fatalf("production applied a migrator of unknown risk: %v", err)
	}
}

func TestBadMigrateConfigFailsBoot(t *testing.T) {
	var mu sync.Mutex
	var log []string
	mod := migratorModule("m", func(r *galleon.Registry) {
		galleon.Migrator(r, "schema", func() migrate.Migrator { return newStub("schema", &log, &mu) })
	})
	for cfg, says := range map[string]string{
		`{"migrate": {"mode": "sometimes"}}`:     "unknown migration mode",
		`{"migrate": {"max_risk": "unknown"}}`:   "max_risk can't be unknown",
		`{"migrate": {"approve": "schema"}}`:     "not name=fingerprint",
		`{"migrate": {"approve": "scheme=ab"}}`:  `migrator "scheme", which isn't registered`,
		`{"migrate": {"lock_ttl": 30}}`:          `lock_ttl: "30" is not a duration: write it with a unit, such as "30s"`,
		`{"migrate": {"lock_ttl": "30"}}`:        `lock_ttl: "30" is not a duration`,
		`{"migrate": {"lock_ttl": "10ms"}}`:      "lock_ttl must be at least 1s, not 10ms",
		`{"migrate": {"lock_wait": "-1s"}}`:      "lock_wait must be at least 1s, not -1s",
		`{"migrate": {"recheck": "1ms"}}`:        "recheck must be at least 100ms, not 1ms",
		`{"migrate": {"apply_timeout": "0s"}}`:   "apply_timeout must be at least 1s, not 0s",
		`{"migrate": {"approve": "schema=ab",}}`: "config must be a JSON object",
	} {
		_, err := galleon.New(append(base(mod), galleon.ConfigJSON([]byte(cfg)))...)
		if err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: want %q, got %v", cfg, says, err)
		}
	}
	// The section is the kernel's: an app without migrators accepts it,
	// and still checks it.
	if _, err := galleon.New(append(base(), galleon.ConfigJSON([]byte(`{"migrate": {"mode": "apply", "lock_wait": "0s"}}`)))...); err != nil {
		t.Errorf("a migrate section without migrators: %v", err)
	}
	if _, err := galleon.New(append(base(), galleon.ConfigJSON([]byte(`{"migrate": {"mode": "often"}}`)))...); err == nil {
		t.Error("a bad migrate section without migrators was accepted")
	}
	if _, err := galleon.New(append(base(mod), galleon.MigrationPolicy(func(p *migrate.Policy) { p.LockTTL = time.Millisecond }))...); err == nil || !strings.Contains(err.Error(), "LockTTL must be 0 (30s) or at least 1s") {
		t.Errorf("a tiny LockTTL in code: %v", err)
	}
}

func TestMigrationsWorkOnlyOnceStarted(t *testing.T) {
	app, err := galleon.New(base()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Migrations().Status(context.Background()); err == nil {
		t.Error("Status worked before Start")
	}
	if _, err := app.Migrations().Done(context.Background(), "schema", "x"); err == nil {
		t.Error("Done worked before Start")
	}
}

func TestTheMigrateSeamDoesntDependOnADriver(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./migrate").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	if strings.Contains(string(out), "pgx") || strings.Contains(string(out), "database/sql") {
		t.Errorf("package migrate depends on a driver:\n%s", out)
	}
}

// risky is a stub that plans a Destructive step.
type risky struct{ *stub }

func (r risky) Plan(context.Context) (migrate.Plan, error) {
	return migrate.NewPlan("", []migrate.StepInfo{{ID: "1_drop", Risk: migrate.Destructive}}), nil
}

func TestMigrationPolicyKeepsTheProfilesDefaultsItDoesntSet(t *testing.T) {
	var mu sync.Mutex
	var log []string
	s := newStub("schema", &log, &mu)
	mod := migratorModule("m", func(r *galleon.Registry) {
		galleon.Migrator(r, "schema", func() migrate.Migrator { return risky{s} })
	})
	app, err := galleon.New(append(base(mod), galleon.MigrationPolicy(func(p *migrate.Policy) { p.Mode = migrate.ModeApply }))...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "1_drop (destructive)") || !strings.Contains(err.Error(), "the maximum risk is caution") {
		t.Fatalf("Production with mode apply applied a Destructive step: %v", err)
	}
	if len(log) != 0 {
		t.Fatal("the step was applied")
	}
}

// cancelling is a stub that ends its caller's context once the Runner has
// read its status after applying, and whose status reads honour ctx.
type cancelling struct {
	*stub
	cancel context.CancelFunc
	reads  int
}

func (c *cancelling) Status(ctx context.Context) (migrate.Status, error) {
	if err := ctx.Err(); err != nil {
		return migrate.Status{}, err
	}
	st, err := c.stub.Status(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.up {
		c.reads++
		if c.reads == 1 && c.cancel != nil {
			c.cancel() // the Runner's recheck has its answer
		}
	}
	return st, err
}

func TestAnOperatorApplyUpdatesTheGatesEvenWhenItsContextEnds(t *testing.T) {
	var mu sync.Mutex
	var log []string
	s := &cancelling{stub: newStub("schema", &log, &mu)}
	mod := migratorModule("m", func(r *galleon.Registry) {
		galleon.Migrator(r, "schema", func() migrate.Migrator { return s })
	})
	t.Setenv("ORDERS_MIGRATE_MODE", "verify")
	t.Setenv("ORDERS_MIGRATE_RECHECK", "1h")
	app, err := galleon.New(append(base(mod), galleon.Environment(galleon.Development))...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	if err := app.Ready(context.Background()); err == nil {
		t.Fatal("ready with the migration pending")
	}
	ctx, cancel := context.WithCancel(context.Background())
	mu.Lock()
	s.cancel = cancel
	mu.Unlock()
	reps, err := app.Migrations().Apply(ctx)
	if err != nil || reps[0].Outcome != migrate.Applied {
		t.Fatalf("%+v, %v", reps, err)
	}
	if ctx.Err() == nil {
		t.Fatal("the test's context didn't end")
	}
	if err := app.Ready(context.Background()); err != nil {
		t.Errorf("not ready after the apply: %v", err)
	}
}

func TestAMigrateSectionSetOnlyInTheEnvironmentIsCheckedWithoutMigrators(t *testing.T) {
	t.Setenv("ORDERS_MIGRATE_LOCK_TTL", "30")
	_, err := galleon.New(base()...)
	if err == nil || !strings.Contains(err.Error(), "lock_ttl") {
		t.Fatalf("got %v, want an error about lock_ttl", err)
	}
}
