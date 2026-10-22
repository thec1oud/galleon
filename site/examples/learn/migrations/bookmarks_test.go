package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/migrate"
	"github.com/thec1oud/galleon/pg"
	"github.com/thec1oud/galleon/testkit"
)

func TestTheServiceMigratesItsSchema(t *testing.T) {
	dsn := testkit.PackageDSN(t)          // an empty database; skips unless GALLEON_TEST_POSTGRES is set
	t.Cleanup(func() { dropAll(t, dsn) }) // registered first, so it runs after the app stops
	h := testkit.New(t,                   // the Test profile applies migrations at boot
		galleon.Modules(pg.Module(), Bookmarks{}),
		galleon.ConfigJSON([]byte(`{"pg": {"primary": {"dsn": "`+dsn+`"}}}`)),
	)
	ctx := context.Background()

	reports, err := h.Migrations().Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range reports {
		if r.Name == "bookmarks" {
			found = true
			if r.Status.State != migrate.UpToDate {
				t.Fatalf("the bookmarks migrator is %s, want up_to_date", r.Status.State)
			}
		}
	}
	if !found {
		t.Fatal("no status for the bookmarks migrator")
	}
	saved, err := testkit.Get[*Store](h).Create(ctx, &CreateIn{URL: "https://go.dev", Title: "Go"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.Get[*Store](h).Get(ctx, saved.ID); err != nil {
		t.Fatal(err)
	}
}

// dropAll removes what the test made: PackageDSN's writes are committed,
// and other tests in the package would see them.
func dropAll(t *testing.T, dsn string) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP TABLE IF EXISTS bookmarks, galleon_migrations, galleon_migrations_fence"); err != nil {
		t.Error(err)
	}
}
