package main

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
	"github.com/thec1oud/galleon/cli"
	"github.com/thec1oud/galleon/testkit"
)

// docs:start approve
func TestADeployAppliesOnlyTheApprovedPlan(t *testing.T) {
	dsn := testkit.PackageDSN(t) // an empty database, standing in for production
	t.Cleanup(func() { dropAll(t, dsn) })

	// CI: notes migrate plan
	plan, code := notes(dsn, Migrations, "migrate", "plan")
	if code != cli.ExitOK {
		t.Fatalf("plan exited %d:\n%s", code, plan)
	}
	t.Log(plan)
	approve := approval(t, plan)

	// The release step: notes migrate apply. A plan other than the
	// approved one is refused.
	t.Setenv("NOTES_MIGRATE_APPROVE", "notes=0123abcd")
	if out, code := notes(dsn, Migrations, "migrate", "apply"); code != cli.ExitFailed || !strings.Contains(out, "migrate.fingerprint_mismatch") {
		t.Fatalf("an unapproved plan: exited %d:\n%s", code, out)
	}

	// The approved plan applies.
	t.Setenv("NOTES_MIGRATE_APPROVE", approve)
	if out, code := notes(dsn, Migrations, "migrate", "apply"); code != cli.ExitOK {
		t.Fatalf("the approved plan: exited %d:\n%s", code, out)
	}

	// The new release's nodes, verifying at boot, find the schema ready.
	if out, code := notes(dsn, Migrations, "migrate", "status"); code != cli.ExitOK || !strings.Contains(out, "notes: up_to_date") {
		t.Fatalf("after the deploy: exited %d:\n%s", code, out)
	}
}

// notes runs the service's command line, as production configures it, on
// database dsn and the migrations in files. It returns what the command
// printed, stdout and stderr together, and its exit code.
func notes(dsn string, files fs.FS, args ...string) (string, int) {
	var out bytes.Buffer
	code := cli.Run(context.Background(), args, &out, &out, options(files,
		galleon.Logger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		galleon.ConfigJSON([]byte(`{"pg": {"primary": {"dsn": "`+dsn+`"}}}`)),
	)...)
	return out.String(), code
}

// docs:end approve

// docs:start risk
func TestProductionRefusesADestructiveStep(t *testing.T) {
	dsn := testkit.PackageDSN(t)
	t.Cleanup(func() { dropAll(t, dsn) })
	if out, code := notes(dsn, Migrations, "migrate", "apply"); code != cli.ExitOK { // no approval pinned: any plan up to caution
		t.Fatalf("exited %d:\n%s", code, out)
	}

	drop := fstest.MapFS{"0003_drop_body.sql": {Data: []byte("-- galleon:risk destructive\nALTER TABLE notes DROP COLUMN body;\n")}}
	files := merge(Migrations, drop)

	// CI's plan already says the deploy would be refused.
	if out, code := notes(dsn, files, "migrate", "plan"); code != cli.ExitFailed || !strings.Contains(out, "refused") {
		t.Fatalf("plan exited %d:\n%s", code, out)
	}

	// And so is the apply, if it gets that far.
	if out, code := notes(dsn, files, "migrate", "apply"); code != cli.ExitFailed || !strings.Contains(out, "3_drop_body (destructive)") {
		t.Fatalf("a destructive step in production: exited %d:\n%s", code, out)
	}
}

// docs:end risk

// approval turns the plan migrate plan printed into the value of
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
