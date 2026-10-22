package main

import (
	"context"
	_ "embed"
	"testing"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/testkit"
)

//go:embed schema.sql
var schema string

func TestABookmarkIsSavedInTheDatabase(t *testing.T) {
	db := testkit.DB(t, testkit.Schema(schema)) // skips unless GALLEON_TEST_POSTGRES is set
	h := testkit.New(t, galleon.Modules(db.Module(), Bookmarks{}))
	store := testkit.Get[*Store](h)
	ctx := context.Background()

	saved, err := store.Create(ctx, &CreateIn{URL: "https://go.dev", Title: "Go"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, saved.ID)
	if err != nil || got.Title != "Go" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	for _, id := range []string{"999", "abc", "0" + saved.ID} {
		if _, err := store.Get(ctx, id); errs.KindOf(err) != errs.NotFound {
			t.Errorf("bookmark %s: %v, want NotFound", id, err)
		}
	}
}

func TestTheStoreKeepsItsLimit(t *testing.T) {
	db := testkit.DB(t, testkit.Schema(schema))
	h := testkit.New(t, galleon.Modules(db.Module(), Bookmarks{}), galleon.ConfigJSON([]byte(`{"limits": {"max_bookmarks": 1}}`)))
	store := testkit.Get[*Store](h)
	ctx := context.Background()
	if _, err := store.Create(ctx, &CreateIn{URL: "https://go.dev", Title: "Go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, &CreateIn{URL: "https://pkg.go.dev", Title: "Packages"}); errs.KindOf(err) != errs.FailedPrecondition {
		t.Fatalf("the second bookmark: %v, want the store to be full", err)
	}
}
