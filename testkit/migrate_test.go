package testkit_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/migrate"
	"github.com/thec1oud/galleon/testkit"
	"github.com/thec1oud/galleon/testkit/conformance/datatest"
)

var (
	stepA = testkit.FakeStep{ID: "1_a", Risk: migrate.Safe, Content: "a"}
	stepB = testkit.FakeStep{ID: "2_b", Risk: migrate.Safe, Content: "b"}
	stepC = testkit.FakeStep{ID: "3_c", Risk: migrate.Caution, Content: "c"}
)

func schemaModule(store *testkit.MigrationStore, steps ...testkit.FakeStep) galleon.Module {
	return galleon.ModuleFunc("schema", func(r *galleon.Registry) error {
		galleon.Migrator(r, "schema", func() migrate.Migrator { return store.Migrator("schema", steps...) })
		return nil
	})
}

func TestTwoAppsSharingCoordinationApplyExactlyOnce(t *testing.T) {
	store := testkit.NewMigrationStore()
	backend := local.New()
	const nodes = 2
	apps := make([]*galleon.App, nodes)
	for i := range apps {
		app, err := galleon.New(
			galleon.Name("orders"), galleon.Coordination(backend), galleon.Deploy(galleon.SingleNode), galleon.DrainGrace(0),
			galleon.Environment(galleon.Test), galleon.Logger(slog.New(slog.NewTextHandler(io.Discard, nil))),
			galleon.Modules(schemaModule(store, stepA, stepB)),
		)
		if err != nil {
			t.Fatal(err)
		}
		apps[i] = app
		t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	}
	start := make(chan struct{})
	errs := make([]error, nodes)
	var wg sync.WaitGroup
	for i, app := range apps {
		wg.Go(func() {
			<-start
			errs[i] = app.Start(context.Background())
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("app %d: %v", i, err)
		}
		if err := apps[i].Ready(context.Background()); err != nil {
			t.Errorf("app %d isn't ready: %v", i, err)
		}
	}
	if calls, changes := store.ApplyCalls("schema"), store.Changes("schema"); calls != 1 || changes != 1 {
		t.Fatalf("Apply ran %d times and changed the store %d times: want exactly once", calls, changes)
	}
	if got := strings.Join(store.Applied("schema"), ","); got != "1_a,2_b" {
		t.Errorf("applied %s", got)
	}
}

func TestAVerifyingNodeBecomesReadyOnceAnotherNodeApplies(t *testing.T) {
	store := testkit.NewMigrationStore()
	h := testkit.New(t, galleon.Environment(galleon.Production), galleon.Modules(schemaModule(store, stepA, stepB)))
	err := h.Ready(context.Background())
	if err == nil || !strings.Contains(err.Error(), `migrate.schema: migrate.not_ready: migrator "schema": changes are pending`) {
		t.Fatalf("a pending node is ready: %v", err)
	}
	if store.ApplyCalls("schema") != 0 {
		t.Fatal("production applied at boot")
	}
	// Another node, a newer generation, applies.
	other := testkit.New(t, galleon.Environment(galleon.Test), galleon.Modules(schemaModule(store, stepA, stepB)))
	if err := other.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The gate rereads the status once per recheck interval, so probes
	// don't each query the database.
	if err := h.Ready(context.Background()); err == nil {
		t.Fatal("the gate reread the status within its interval")
	}
	h.Clock.Advance(5 * time.Second)
	if err := h.Ready(context.Background()); err != nil {
		t.Fatalf("the gate didn't pass once applied: %v", err)
	}
	if !h.Logs.Contains("migration gate fails") || !h.Logs.Contains("migration gate passes") {
		t.Errorf("the gate's changes aren't logged:\n%s", h.Logs)
	}
}

func TestAnOlderBuildIsReadyOnAheadOnlyWhenTolerated(t *testing.T) {
	store := testkit.NewMigrationStore()
	testkit.New(t, galleon.Environment(galleon.Test), galleon.Modules(schemaModule(store, stepA, stepB, stepC)))
	strict := testkit.New(t, galleon.Environment(galleon.Production), galleon.Modules(schemaModule(store, stepA, stepB)))
	if err := strict.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "ahead of this build") {
		t.Fatalf("an older build is ready: %v", err)
	}
	tolerant := testkit.New(t, galleon.Environment(galleon.Production), galleon.ConfigJSON([]byte(`{"migrate": {"tolerate_ahead": true}}`)),
		galleon.Modules(schemaModule(store, stepA, stepB)))
	if err := tolerant.Ready(context.Background()); err != nil {
		t.Fatalf("an older build isn't ready though Ahead is tolerated: %v", err)
	}
}

func TestAnApprovedFingerprintPinsThePlan(t *testing.T) {
	store := testkit.NewMigrationStore()
	pl, err := store.Migrator("schema", stepA, stepB).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	opts := []galleon.Option{galleon.Name("orders"), galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode),
		galleon.Logger(slog.New(slog.NewTextHandler(io.Discard, nil))), galleon.ConfigJSON([]byte(`{"migrate": {"mode": "apply"}}`)),
		galleon.Modules(schemaModule(store, stepA, stepB))}
	t.Setenv("ORDERS_MIGRATE_APPROVE", "schema=0123abcd")
	app, err := galleon.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	err = app.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "migrate.fingerprint_mismatch") || !strings.Contains(err.Error(), "0123abcd was approved") {
		t.Fatalf("a plan other than the approved one: %v", err)
	}
	if store.ApplyCalls("schema") != 0 {
		t.Fatal("Apply ran")
	}
	t.Setenv("ORDERS_MIGRATE_APPROVE", "schema="+pl.Fingerprint())
	app, err = galleon.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("the approved plan: %v", err)
	}
	defer app.Shutdown(context.Background())
	if store.Changes("schema") != 1 {
		t.Error("the approved plan wasn't applied")
	}
}

func TestProductionRefusesAPlanAboveCaution(t *testing.T) {
	store := testkit.NewMigrationStore()
	drop := testkit.FakeStep{ID: "3_drop", Risk: migrate.Destructive, Content: "drop"}
	app, err := galleon.New(galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode),
		galleon.Logger(slog.New(slog.NewTextHandler(io.Discard, nil))), galleon.ConfigJSON([]byte(`{"migrate": {"mode": "apply"}}`)),
		galleon.Modules(schemaModule(store, stepA, drop)))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "3_drop (destructive)") {
		t.Fatalf("%v", err)
	}
	if len(store.Applied("schema")) != 0 {
		t.Error("part of the refused plan was applied")
	}
}

// Backfills waits on a background task through the Migrations handle.
type Backfills struct{ m *galleon.Migrations }

func TestAppCodeAsksWhetherABackgroundTaskIsDone(t *testing.T) {
	store := testkit.NewMigrationStore()
	mod := galleon.ModuleFunc("schema", func(r *galleon.Registry) error {
		galleon.Migrator(r, "schema", func() migrate.Migrator {
			m := store.Migrator("schema", stepA)
			m.Tasks = []testkit.FakeTask{{ID: "backfill", Units: 1}}
			return m
		})
		galleon.Provide[*Backfills](r, func(m *galleon.Migrations) *Backfills { return &Backfills{m} })
		return nil
	})
	h := testkit.New(t, galleon.Modules(mod))
	b := testkit.Get[*Backfills](h)
	if done, err := b.m.Done(context.Background(), "schema", "backfill"); err != nil || done {
		t.Fatalf("done before running: %v, %v", done, err)
	}
	reps, err := b.m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	_ = migrate.WriteStatus(&out, reps)
	if !strings.Contains(out.String(), "background backfill: pending") {
		t.Errorf("status doesn't list the pending task:\n%s", out.String())
	}
	// The job system that will run background tasks doesn't exist yet
	// (phase 6), so the test steps the task as it would.
	m := store.Migrator("schema", stepA)
	m.Tasks = []testkit.FakeTask{{ID: "backfill", Units: 1}}
	ts, _ := m.BackgroundTasks(context.Background())
	if _, _, err := ts[0].Step(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if done, err := b.m.Done(context.Background(), "schema", "backfill"); err != nil || !done {
		t.Fatalf("done after running: %v, %v", done, err)
	}
}

func TestTheKernelTracesMigrationsWithTheAppsProvider(t *testing.T) {
	store := testkit.NewMigrationStore()
	tp := &datatest.Recorder{}
	testkit.New(t, galleon.TracerProvider(tp), galleon.Modules(schemaModule(store, stepA, stepB)))
	applies := tp.Find(func(s *datatest.Span) bool { return s.Name() == "migrate.apply" })
	steps := tp.Find(func(s *datatest.Span) bool { return s.Name() == "migrate.step" })
	if len(applies) != 1 || len(steps) != 2 || steps[0].Parent() != applies[0] || steps[1].Attr("galleon.migrate.step") != "2_b" {
		t.Fatalf("spans %v", tp.Names())
	}
}

func TestTheFakeCallsAGapBelowItsLatestAppliedStepDrift(t *testing.T) {
	store := testkit.NewMigrationStore()
	testkit.New(t, galleon.Modules(schemaModule(store, stepA, stepC)))
	st, err := store.Migrator("schema", stepA, stepB, stepC).Status(context.Background())
	if err != nil || st.State != migrate.Drifted || !strings.Contains(st.Detail, "2_b is older than applied 3_c") {
		t.Fatalf("%+v, %v", st, err)
	}
}

// epochLease is a lease of a given epoch and token.
type epochLease struct {
	epoch string
	token uint64
}

func (epochLease) Key() string                   { return "" }
func (l epochLease) Token() uint64               { return l.token }
func (l epochLease) Epoch() string               { return l.epoch }
func (epochLease) Renew(context.Context) error   { return nil }
func (epochLease) Release(context.Context) error { return nil }
func (epochLease) Lost() <-chan struct{}         { return nil }

func TestTheFakeSupersedesTheEmptyEpochAsSQLMigrateDoes(t *testing.T) {
	ctx := context.Background()
	store := testkit.NewMigrationStore()
	apply := func(epoch string, steps ...testkit.FakeStep) error {
		_, err := store.Migrator("schema", steps...).Apply(ctx, migrate.ApplyOptions{Lock: epochLease{epoch, 1}, MaxRisk: migrate.Manual, AllowUnknown: true})
		return err
	}
	if err := apply("", stepA); err != nil {
		t.Fatalf("the empty epoch: %v", err)
	}
	if err := apply("e1", stepA, stepB); err != nil {
		t.Fatalf("e1: %v", err)
	}
	if err := apply("", stepA, stepB, stepC); err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("the empty epoch after e1: %v", err)
	}
}
