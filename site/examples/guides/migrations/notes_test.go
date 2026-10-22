package notes

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/migrate"
	"github.com/thec1oud/galleon/pg"
	"github.com/thec1oud/galleon/testkit"
)

// docs:start approve
func TestADeployAppliesOnlyTheApprovedPlan(t *testing.T) {
	dsn := testkit.PackageDSN(t) // an empty database, standing in for production
	t.Cleanup(func() { dropAll(t, dsn) })
	ctx := context.Background()

	// CI: boot as production does, in verify mode, and print the plan.
	ci := testkit.New(t,
		galleon.Name("notes"),
		galleon.Environment(galleon.Production),
		galleon.Modules(pg.Module(), Notes{Migrations}),
		galleon.ConfigJSON([]byte(`{"pg": {"primary": {"dsn": "`+dsn+`"}}}`)),
	)
	var out bytes.Buffer
	if err := PrintPlan(ctx, ci.App, &out); err != nil {
		t.Fatal(err)
	}
	t.Log(out.String())
	approve := approval(t, out.String())

	// The deploy: a plan other than the approved one is refused.
	t.Setenv("NOTES_MIGRATE_APPROVE", "notes=0123abcd")
	err := deploy(dsn, Migrations)
	if err == nil || !strings.Contains(err.Error(), "migrate.fingerprint_mismatch") {
		t.Fatalf("an unapproved plan: %v", err)
	}

	// The approved plan applies.
	t.Setenv("NOTES_MIGRATE_APPROVE", approve)
	if err := deploy(dsn, Migrations); err != nil {
		t.Fatalf("the approved plan: %v", err)
	}
	reports, err := ci.Migrations().Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Status.State != migrate.UpToDate {
		t.Fatalf("after the deploy: %+v", reports)
	}
}

// deploy boots the service as production does, with migrations applied at
// boot, and stops it again.
func deploy(dsn string, files fs.FS) error {
	app, err := galleon.New(
		galleon.Name("notes"),
		galleon.Environment(galleon.Production),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
		galleon.DrainGrace(0), // nothing to drain: the test stops it at once
		galleon.Logger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		galleon.Modules(pg.Module(), Notes{files}),
		galleon.ConfigJSON([]byte(`{"pg": {"primary": {"dsn": "`+dsn+`"}}, "migrate": {"mode": "apply"}}`)),
	)
	if err != nil {
		return err
	}
	if err := app.Start(context.Background()); err != nil {
		return err
	}
	return app.Shutdown(context.Background())
}

// docs:end approve

// docs:start risk
func TestProductionRefusesADestructiveStep(t *testing.T) {
	dsn := testkit.PackageDSN(t)
	t.Cleanup(func() { dropAll(t, dsn) })
	if err := deploy(dsn, Migrations); err != nil { // no approval pinned: any plan up to caution
		t.Fatal(err)
	}

	drop := fstest.MapFS{"0003_drop_body.sql": {Data: []byte("-- galleon:risk destructive\nALTER TABLE notes DROP COLUMN body;\n")}}
	files := merge(Migrations, drop)

	// CI's plan already says the deploy would be refused.
	ci := testkit.New(t,
		galleon.Name("notes"),
		galleon.Environment(galleon.Production),
		galleon.Modules(pg.Module(), Notes{files}),
		galleon.ConfigJSON([]byte(`{"pg": {"primary": {"dsn": "`+dsn+`"}}}`)),
	)
	var out bytes.Buffer
	if err := PrintPlan(context.Background(), ci.App, &out); err == nil || !strings.Contains(out.String(), "refused") {
		t.Fatalf("PrintPlan = %v:\n%s", err, out.String())
	}

	// And so is the deploy, if it gets that far.
	err := deploy(dsn, files)
	if err == nil || !strings.Contains(err.Error(), "3_drop_body (destructive)") {
		t.Fatalf("a destructive step in production: %v", err)
	}
}

// docs:end risk

// approval turns the plan PrintPlan wrote into the value of
// NOTES_MIGRATE_APPROVE: "notes=" and the fingerprint.
func approval(t *testing.T, plan string) string {
	_, fp, ok := strings.Cut(plan, "fingerprint ")
	if !ok {
		t.Fatalf("no fingerprint in the plan:\n%s", plan)
	}
	fp, _, _ = strings.Cut(fp, "\n")
	return "notes=" + fp
}

// merge returns base's files plus extra's.
func merge(base fs.FS, extra fstest.MapFS) fs.FS {
	out := fstest.MapFS{}
	names, _ := fs.Glob(base, "*.sql")
	for _, n := range names {
		b, _ := fs.ReadFile(base, n)
		out[n] = &fstest.MapFile{Data: b}
	}
	for n, f := range extra {
		out[n] = f
	}
	return out
}

// dropAll removes what the test made, so other tests in the package start
// from an empty database.
func dropAll(t *testing.T, dsn string) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP TABLE IF EXISTS notes, galleon_migrations, galleon_migrations_fence"); err != nil {
		t.Error(err)
	}
}
