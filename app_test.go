package galleon_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/coord/local"
)

func quiet() galleon.Option { return galleon.Logger(slog.New(slog.NewTextHandler(io.Discard, nil))) }

func base(mods ...galleon.Module) []galleon.Option {
	return []galleon.Option{
		quiet(),
		galleon.Name("orders"),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
		galleon.DrainGrace(0),
		galleon.Modules(mods...),
	}
}

// ---- test components ----

type Repo struct{ log *[]string }

func (r *Repo) Start(context.Context) error { *r.log = append(*r.log, "start repo"); return nil }
func (r *Repo) Stop(context.Context) error  { *r.log = append(*r.log, "stop repo"); return nil }

type Service struct {
	Repo *Repo
	log  *[]string
}

func (s *Service) Start(context.Context) error { *s.log = append(*s.log, "start service"); return nil }
func (s *Service) Stop(context.Context) error  { *s.log = append(*s.log, "stop service"); return nil }

func TestLifecycleOrder(t *testing.T) {
	var log []string
	mod := galleon.ModuleFunc("orders", func(r *galleon.Registry) error {
		// Registered out of dependency order on purpose.
		galleon.Provide[*Service](r, func(repo *Repo) *Service { return &Service{Repo: repo, log: &log} })
		galleon.Provide[*Repo](r, func() *Repo { return &Repo{log: &log} })
		r.OnStart(func(context.Context) error { log = append(log, "hook start"); return nil })
		r.OnStop(func(context.Context) error { log = append(log, "hook stop"); return nil })
		r.OnDrain(func(context.Context) error { log = append(log, "drain"); return nil })
		return nil
	})
	app, err := galleon.New(base(mod)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc := galleon.MustResolve[*Service](app)
	if svc.Repo == nil {
		t.Fatal("dependency not injected")
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "start repo,start service,hook start,drain,hook stop,stop service,stop repo"
	if got := strings.Join(log, ","); got != want {
		t.Fatalf("order:\n got %s\nwant %s", got, want)
	}
}

func TestMissingDependencyIsReportedAtBoot(t *testing.T) {
	mod := galleon.ModuleFunc("orders", func(r *galleon.Registry) error {
		galleon.Provide[*Service](r, func(repo *Repo) *Service { return &Service{Repo: repo} })
		return nil
	})
	_, err := galleon.New(base(mod)...)
	var be *galleon.BootError
	if !errors.As(err, &be) {
		t.Fatalf("expected BootError, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"*galleon_test.Service", "needs *galleon_test.Repo", `module "orders"`, "app_test.go"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q:\n%s", want, msg)
		}
	}
}

type A struct{}
type B struct{}

func TestCycleIsReportedAtBoot(t *testing.T) {
	mod := galleon.ModuleFunc("m", func(r *galleon.Registry) error {
		galleon.Provide[*A](r, func(*B) *A { return &A{} })
		galleon.Provide[*B](r, func(*A) *B { return &B{} })
		return nil
	})
	_, err := galleon.New(base(mod)...)
	if err == nil || !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestAllBootProblemsReportedTogether(t *testing.T) {
	mod := galleon.ModuleFunc("m", func(r *galleon.Registry) error {
		galleon.Provide[*A](r, "not a function")
		galleon.Provide[*B](r, func() (*B, string) { return nil, "" })
		r.Requires("missing-module")
		return nil
	})
	_, err := galleon.New(append(base(mod), galleon.ConfigJSON([]byte(`{"typo":{}}`)))...)
	var be *galleon.BootError
	if !errors.As(err, &be) || len(be.Errs) < 4 {
		t.Fatalf("expected at least 4 problems, got %v", err)
	}
}

type failing struct{}

func (failing) Start(context.Context) error { return errors.New("db unreachable") }

func TestFailedStartStopsWhatStarted(t *testing.T) {
	var log []string
	mod := galleon.ModuleFunc("m", func(r *galleon.Registry) error {
		galleon.Provide[*Repo](r, func() *Repo { return &Repo{log: &log} })
		galleon.Provide[failing](r, func(*Repo) failing { return failing{} })
		r.OnStop(func(context.Context) error { log = append(log, "hook stop"); return nil })
		return nil
	})
	app, err := galleon.New(base(mod)...)
	if err != nil {
		t.Fatal(err)
	}
	err = app.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "db unreachable") {
		t.Fatalf("expected start failure, got %v", err)
	}
	if got := strings.Join(log, ","); got != "start repo,stop repo" {
		t.Fatalf("rollback order = %s (stop hooks must not run after a failed start)", got)
	}
}

func TestReadinessGatesAndProbes(t *testing.T) {
	healthy := false
	mod := galleon.ModuleFunc("m", func(r *galleon.Registry) error {
		r.Gate("schema", func(context.Context) error {
			if !healthy {
				return errors.New("schema pending")
			}
			return nil
		})
		return nil
	})
	app, err := galleon.New(base(mod)...)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.HealthHandler(true))
	defer srv.Close()
	status := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if status("/readyz") != 503 {
		t.Error("should not be ready before start")
	}
	_ = app.Start(context.Background())
	if status("/readyz") != 503 || status("/livez") != 200 {
		t.Error("gate failing: want not ready but live")
	}
	healthy = true
	if status("/readyz") != 200 {
		t.Error("gate passing: want ready")
	}
	_ = app.Shutdown(context.Background())
	if status("/readyz") != 503 || status("/livez") != 503 {
		t.Error("after shutdown: want not ready and not live")
	}
}

type DBConfig struct {
	Host     string        `json:"host"`
	Port     int           `json:"port"`
	Timeout  time.Duration `json:"timeout"`
	Password string        `json:"password" secret:"true"`
}

func (c *DBConfig) Default() { c.Host = "localhost"; c.Port = 5432; c.Timeout = time.Second }
func (c *DBConfig) Validate() error {
	if c.Port <= 0 {
		return errors.New("port must be positive")
	}
	return nil
}

type DB struct{ cfg *DBConfig }

func TestConfigPrecedenceAndInjection(t *testing.T) {
	t.Setenv("ORDERS_DB_PORT", "6543")
	t.Setenv("ORDERS_DB_PASSWORD", "hunter2")
	mod := galleon.ModuleFunc("db", func(r *galleon.Registry) error {
		galleon.Config[DBConfig](r, "db")
		galleon.Provide[*DB](r, func(c *DBConfig) *DB { return &DB{cfg: c} })
		return nil
	})
	app, err := galleon.New(append(base(mod), galleon.ConfigJSON([]byte(`{"db":{"host":"db.internal"}}`)))...)
	if err != nil {
		t.Fatal(err)
	}
	_ = app.Start(context.Background())
	db := galleon.MustResolve[*DB](app)
	if db.cfg.Host != "db.internal" || db.cfg.Port != 6543 || db.cfg.Timeout != time.Second || db.cfg.Password != "hunter2" {
		t.Fatalf("config = %+v", *db.cfg)
	}
	sources := map[string]string{}
	for _, v := range app.ConfigReport() {
		sources[v.Field] = v.Source
		if v.Field == "password" && v.Value != "[REDACTED]" {
			t.Errorf("secret not redacted: %q", v.Value)
		}
	}
	want := map[string]string{"host": "file", "port": "env", "timeout": "default", "password": "env"}
	for f, s := range want {
		if sources[f] != s {
			t.Errorf("%s source = %q, want %q", f, sources[f], s)
		}
	}
}

func TestConfigValidationAndUnknownFields(t *testing.T) {
	mod := galleon.ModuleFunc("db", func(r *galleon.Registry) error {
		galleon.Config[DBConfig](r, "db")
		return nil
	})
	_, err := galleon.New(append(base(mod), galleon.ConfigJSON([]byte(`{"db":{"port":-1}}`)))...)
	if err == nil || !strings.Contains(err.Error(), "port must be positive") {
		t.Fatalf("expected validation error, got %v", err)
	}
	_, err = galleon.New(append(base(mod), galleon.ConfigJSON([]byte(`{"db":{"hots":"x"}}`)))...)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestCoordinationIsRequired(t *testing.T) {
	_, err := galleon.New(quiet())
	if err == nil || !strings.Contains(err.Error(), "no coordination backend") {
		t.Fatalf("expected missing backend error, got %v", err)
	}
}

func TestProcessBackendRefusedForMultiNode(t *testing.T) {
	mod := galleon.ModuleFunc("cron", func(r *galleon.Registry) error {
		r.Needs(coord.LeaderElection)
		return nil
	})
	opts := []galleon.Option{quiet(), galleon.Coordination(local.New()), galleon.Deploy(galleon.MultiNode), galleon.Modules(mod)}
	if _, err := galleon.New(opts...); err == nil || !strings.Contains(err.Error(), "multi-node") {
		t.Fatalf("expected multi-node refusal, got %v", err)
	}
	if _, err := galleon.New(append(opts, galleon.AllowPerProcessGuarantees())...); err != nil {
		t.Fatalf("explicit override should boot: %v", err)
	}
}

func TestResolveBeforeStartFails(t *testing.T) {
	app, err := galleon.New(base()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := galleon.Resolve[*slog.Logger](app); err == nil {
		t.Fatal("Resolve before Start should fail")
	}
}

type Mailer interface{ Send(to string) error }
type smtpMailer struct{}

func (smtpMailer) Send(string) error { return errors.New("would send real mail") }

type fakeMailer struct {
	sent      []string
	lifecycle []string
}

func (f *fakeMailer) Send(to string) error { f.sent = append(f.sent, to); return nil }
func (f *fakeMailer) Start(context.Context) error {
	f.lifecycle = append(f.lifecycle, "start")
	return nil
}
func (f *fakeMailer) Stop(context.Context) error {
	f.lifecycle = append(f.lifecycle, "stop")
	return nil
}

func TestReplaceSwapsAProvider(t *testing.T) {
	mod := galleon.ModuleFunc("mail", func(r *galleon.Registry) error {
		galleon.Provide[Mailer](r, func(*Repo) Mailer { return smtpMailer{} })
		return nil
	})
	fake := &fakeMailer{}
	// *Repo is not provided: replacing Mailer also drops its constructor's needs.
	app, err := galleon.New(append(base(mod), galleon.Replace[Mailer](fake))...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	if err := galleon.MustResolve[Mailer](app).Send("a@b"); err != nil || len(fake.sent) != 1 {
		t.Fatalf("fake not used: err=%v sent=%v", err, fake.sent)
	}
	_ = app.Shutdown(context.Background())
	if len(fake.lifecycle) != 0 {
		t.Fatalf("a replaced value belongs to the test and must not be started or stopped: %v", fake.lifecycle)
	}

	_, err = galleon.New(append(base(), galleon.Replace[Mailer](fake))...)
	if err == nil || !strings.Contains(err.Error(), "nothing provides galleon_test.Mailer") || !strings.Contains(err.Error(), "app_test.go") {
		t.Fatalf("stale replacement should fail boot naming the call site, got %v", err)
	}
}

func TestReplaceRefusesKernelValuesAndDuplicates(t *testing.T) {
	_, err := galleon.New(append(base(), galleon.Replace[coord.Backend](local.New()))...)
	if err == nil || !strings.Contains(err.Error(), "set it with galleon.Coordination instead") {
		t.Fatalf("replacing a kernel value should point at its option, got %v", err)
	}
	mod := galleon.ModuleFunc("mail", func(r *galleon.Registry) error {
		galleon.Provide[Mailer](r, func() Mailer { return smtpMailer{} })
		return nil
	})
	_, err = galleon.New(append(base(mod), galleon.Replace[Mailer](&fakeMailer{}), galleon.Replace[Mailer](&fakeMailer{}))...)
	if err == nil || !strings.Contains(err.Error(), "is already replaced at") {
		t.Fatalf("a second Replace of the same type should fail, got %v", err)
	}
	_, err = galleon.New(append(base(), galleon.Replace[galleon.Node](galleon.Node{Host: "fake"}))...)
	if err == nil || !strings.Contains(err.Error(), "the kernel provides galleon.Node") {
		t.Fatalf("every kernel value should be refused, got %v", err)
	}
	_, err = galleon.New(append(base(mod), galleon.Replace[Mailer](nil))...)
	if err == nil || !strings.Contains(err.Error(), "the replacement is nil") {
		t.Fatalf("a nil replacement should fail boot, got %v", err)
	}
}

func TestRegistryFatalStopsTheApp(t *testing.T) {
	var reg *galleon.Registry
	app, err := galleon.New(base(galleon.ModuleFunc("listener", func(r *galleon.Registry) error {
		reg = r
		return nil
	}))...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	for app.Ready(ctx) != nil {
		if ctx.Err() != nil {
			t.Fatal("the app never became ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	reg.Fatal(errors.New("listener died"))
	if err := app.Ready(ctx); err == nil {
		t.Fatal("readiness must fail after Fatal")
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), `module "listener": listener died`) {
		t.Fatalf("Run must return the fatal error, got %v", err)
	}
}

// managedBackend is a coordination backend that owns resources: a module
// with its own config section, started and stopped by the app.
type managedBackend struct {
	*local.Backend
	log     *[]string
	setting *struct{ Value string }
}

func (b *managedBackend) Name() string { return "managed_coord" }
func (b *managedBackend) Register(r *galleon.Registry) error {
	b.setting = galleon.Config[struct{ Value string }](r, "managed_coord")
	return nil
}
func (b *managedBackend) Start(context.Context) error {
	*b.log = append(*b.log, "start backend "+b.setting.Value)
	return nil
}
func (b *managedBackend) Stop(context.Context) error {
	*b.log = append(*b.log, "stop backend")
	return nil
}

func TestABackendOwnsItsLifecycleThroughTheApp(t *testing.T) {
	var log []string
	b := &managedBackend{Backend: local.New(), log: &log}
	mod := galleon.ModuleFunc("orders", func(r *galleon.Registry) error {
		galleon.Provide[*Repo](r, func() *Repo { return &Repo{log: &log} })
		return nil
	})
	app, err := galleon.New(quiet(), galleon.Coordination(b), galleon.Deploy(galleon.SingleNode), galleon.DrainGrace(0),
		galleon.ConfigJSON([]byte(`{"managed_coord": {"Value": "configured"}}`)), galleon.Modules(mod))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"start backend configured", "start repo", "stop repo", "stop backend"}
	if strings.Join(log, ",") != strings.Join(want, ",") {
		t.Fatalf("lifecycle = %v, want %v", log, want)
	}

	_, err = galleon.New(quiet(), galleon.Coordination(b), galleon.Modules(b))
	if err == nil || !strings.Contains(err.Error(), "leave it out of galleon.Modules") {
		t.Fatalf("installing the backend as a module too = %v", err)
	}
}

func TestABackendThatFailsToStartFailsStart(t *testing.T) {
	var log []string
	b := &failingBackend{managedBackend{Backend: local.New(), log: &log}}
	app, err := galleon.New(quiet(), galleon.Coordination(b), galleon.Deploy(galleon.SingleNode))
	if err != nil {
		t.Fatal(err)
	}
	err = app.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `starting coordination backend "local": unreachable`) {
		t.Fatalf("Start = %v", err)
	}
	if len(log) != 0 {
		t.Fatalf("a backend that didn't start was stopped: %v", log)
	}
}

type failingBackend struct{ managedBackend }

func (b *failingBackend) Start(context.Context) error { return errors.New("unreachable") }
