package cli_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/cli"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/migrate"
)

// schema is a migrator with one step, whose database is a flag.
type schema struct {
	mu   sync.Mutex
	up   bool
	risk migrate.Risk
}

func (s *schema) Name() string { return "schema" }

func (s *schema) Status(context.Context) (migrate.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.up {
		return migrate.Status{State: migrate.UpToDate, Expected: "1", Current: "1"}, nil
	}
	return migrate.Status{State: migrate.Pending, Expected: "1", Current: "none"}, nil
}

func (s *schema) Plan(context.Context) (migrate.Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.up {
		return migrate.NewPlan("", nil), nil
	}
	return migrate.NewPlan("", []migrate.StepInfo{{ID: "0001_create", Risk: s.risk, Description: "create the table"}}), nil
}

func (s *schema) Apply(context.Context, migrate.ApplyOptions) (migrate.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up = true
	return migrate.Result{Applied: []migrate.StepInfo{{ID: "0001_create", Risk: s.risk}}}, nil
}

// app returns the options of an app with s as its only migrator, and a
// record of whether its hooks ran.
func app(s *schema, config string) ([]galleon.Option, *bool) {
	hooked := new(bool)
	mod := galleon.ModuleFunc("m", func(r *galleon.Registry) error {
		galleon.Migrator(r, "schema", func() migrate.Migrator { return s })
		r.OnStart(func(context.Context) error { *hooked = true; return nil })
		return nil
	})
	return []galleon.Option{
		galleon.Name("orders"),
		galleon.Logger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
		galleon.Modules(mod),
		galleon.ConfigJSON([]byte(config)),
	}, hooked
}

func runCLI(t *testing.T, opts []galleon.Option, args ...string) (int, string, string) {
	t.Helper()
	var out, errs bytes.Buffer
	code := cli.Run(context.Background(), args, &out, &errs, opts...)
	return code, out.String(), errs.String()
}

func TestStatusPlanApply(t *testing.T) {
	s := &schema{risk: migrate.Safe}
	// mode apply would apply at boot; the commands apply only when asked.
	opts, hooked := app(s, `{"migrate": {"mode": "apply"}}`)

	code, out, _ := runCLI(t, opts, "migrate", "status")
	if code != cli.ExitFailed || !strings.Contains(out, "schema: pending (database none, build 1), not ready") {
		t.Fatalf("status before apply: %d\n%s", code, out)
	}
	code, out, _ = runCLI(t, opts, "migrate", "plan")
	if code != cli.ExitOK || !strings.Contains(out, "schema: 1 step(s), max risk safe, fingerprint ") || !strings.Contains(out, "0001_create [safe] create the table") {
		t.Fatalf("plan: %d\n%s", code, out)
	}
	if s.up {
		t.Fatal("status or plan applied")
	}
	code, out, _ = runCLI(t, opts, "migrate", "apply")
	if code != cli.ExitOK || !strings.Contains(out, "schema: applied, 1 step(s)") || !s.up {
		t.Fatalf("apply: %d\n%s", code, out)
	}
	code, out, _ = runCLI(t, opts, "migrate", "status")
	if code != cli.ExitOK || !strings.Contains(out, "up_to_date (database 1, build 1), ready") {
		t.Fatalf("status after apply: %d\n%s", code, out)
	}
	if *hooked {
		t.Error("a command ran the app's start hooks")
	}
}

func TestThePolicyStillRefuses(t *testing.T) {
	s := &schema{risk: migrate.Destructive}
	opts, _ := app(s, `{}`) // Production: up to caution

	code, out, errs := runCLI(t, opts, "migrate", "plan")
	if code != cli.ExitFailed || !strings.Contains(out, "refused: migrate.risk_refused") || !strings.Contains(errs, "migrate plan: schema can't be applied as planned") {
		t.Fatalf("plan: %d\n%s\n%s", code, out, errs)
	}
	code, out, errs = runCLI(t, opts, "migrate", "apply")
	if code != cli.ExitFailed || !strings.Contains(out, "error: migrate.risk_refused") || s.up {
		t.Fatalf("apply: %d, applied %v\n%s", code, s.up, out)
	}
	if errs != "migrate apply: migrator \"schema\": refused; the report above says why\n" {
		t.Errorf("stderr should name the migrator once, not repeat the report:\n%s", errs)
	}
}

func TestPlanOfflineSaysWhatCantPlanOffline(t *testing.T) {
	opts, _ := app(&schema{risk: migrate.Safe}, `{}`)
	code, out, _ := runCLI(t, opts, "migrate", "plan", "-offline")
	if code != cli.ExitFailed || !strings.Contains(out, `can't plan without a database`) {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestABootProblemFails(t *testing.T) {
	opts, _ := app(&schema{}, `{"migrat": {}}`)
	code, _, errs := runCLI(t, opts, "migrate", "status")
	if code != cli.ExitFailed || !strings.Contains(errs, `config section "migrat" is not used`) {
		t.Fatalf("%d\n%s", code, errs)
	}
}

func TestAnAppWithoutMigrators(t *testing.T) {
	opts := []galleon.Option{
		galleon.Logger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
	}
	code, _, errs := runCLI(t, opts, "migrate", "status")
	if code != cli.ExitFailed || !strings.Contains(errs, "the app has no migrators") {
		t.Fatalf("%d\n%s", code, errs)
	}
}

func TestUsage(t *testing.T) {
	opts, _ := app(&schema{}, `{}`)
	for _, tc := range []struct {
		args      []string
		code      int
		out, errs string
	}{
		{[]string{"help"}, cli.ExitOK, "migrate plan", ""},
		{[]string{"-h"}, cli.ExitOK, "Without a command, the app serves", ""},
		{[]string{"help", "migrate", "plan"}, cli.ExitOK, "-offline", ""},
		{[]string{"migrate", "apply", "-h"}, cli.ExitOK, "Exit codes:", ""},
		{[]string{"help", "nope"}, cli.ExitUsage, "", `unknown command "nope"`},
		{[]string{"nope"}, cli.ExitUsage, "", `unknown command "nope"`},
		{[]string{"migrate"}, cli.ExitUsage, "", "migrate needs a subcommand"},
		{[]string{"migrate", "drop"}, cli.ExitUsage, "", `unknown command "migrate drop"`},
		{[]string{"migrate", "-offline", "plan"}, cli.ExitUsage, "", "put flags after the subcommand"},
		{[]string{"help", "migrate"}, cli.ExitOK, "migrate apply", ""},
		{[]string{"migrate", "-h"}, cli.ExitOK, "migrate status", ""},
		{[]string{"help", "migrate", "plan", "extra"}, cli.ExitUsage, "", `unknown command "migrate plan extra"`},
		{[]string{"migrate", "plan", "-online"}, cli.ExitUsage, "", "flag provided but not defined: -online"},
		{[]string{"migrate", "status", "extra"}, cli.ExitUsage, "", `unexpected argument "extra"`},
	} {
		code, out, errs := runCLI(t, opts, tc.args...)
		if code != tc.code || !strings.Contains(out, tc.out) || !strings.Contains(errs, tc.errs) {
			t.Errorf("%v: %d\nstdout:\n%s\nstderr:\n%s", tc.args, code, out, errs)
		}
	}
}

func TestServeRunsTheApp(t *testing.T) {
	opts, hooked := app(&schema{up: true}, `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	opts = append(opts, galleon.DrainGrace(0), galleon.Modules(galleon.ModuleFunc("stop", func(r *galleon.Registry) error {
		r.OnStart(func(context.Context) error { close(started); return nil })
		return nil
	})))
	done := make(chan int)
	go func() { done <- cli.Run(ctx, nil, io.Discard, io.Discard, opts...) }()
	<-started
	cancel()
	if code := <-done; code != cli.ExitOK || !*hooked {
		t.Fatalf("serve: %d, hooks ran %v", code, *hooked)
	}
}

func TestCommandsDescribeEveryCommand(t *testing.T) {
	var names []string
	for _, c := range cli.Commands() {
		names = append(names, c.Name)
		if c.Summary == "" || len(c.Exits) == 0 {
			t.Errorf("%q has no summary or exit codes", c.Name)
		}
		if c.Name == "migrate plan" && (len(c.Flags) != 1 || c.Flags[0] != cli.Flag{Name: "offline", Default: "false", Usage: "plan without reading the database's state"}) {
			t.Errorf("migrate plan's flags, as it parses them: %+v", c.Flags)
		}
	}
	if got := strings.Join(names, ","); got != "serve,migrate status,migrate plan,migrate apply" {
		t.Errorf("commands %s", got)
	}
}
