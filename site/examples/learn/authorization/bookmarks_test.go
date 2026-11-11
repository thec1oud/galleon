package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/pg"
	"github.com/thec1oud/galleon/server"
	"github.com/thec1oud/galleon/testkit"
)

// docs:start test
func TestEachCallerSeesOnlyTheirOwnBookmarks(t *testing.T) {
	dsn := testkit.PackageDSN(t)          // an empty database; skips unless GALLEON_TEST_POSTGRES is set
	t.Cleanup(func() { dropAll(t, dsn) }) // registered first, so it runs after the app stops
	t.Setenv("TEST_HTTP_ADDR", "127.0.0.1:0")
	h := testkit.New(t,
		galleon.Modules(server.Module(), pg.Module(), Security(), Bookmarks{}),
		galleon.ConfigJSON([]byte(`{"pg": {"primary": {"dsn": "`+dsn+`"}}}`)),
	)
	ctx := context.Background()
	keys := testkit.Get[*Keys](h)
	ann, err := keys.Add(ctx, "ann")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := keys.Add(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	api := testkit.Get[*server.Server](h).Handler()

	if w := call(api, "POST", "/bookmarks", "", `{"url": "https://go.dev", "title": "Go"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("without a key: %d", w.Code)
	}
	if w := call(api, "POST", "/bookmarks", "bk_made_up", `{"url": "https://go.dev", "title": "Go"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("with a made-up key: %d", w.Code)
	}
	w := call(api, "POST", "/bookmarks", ann, `{"url": "https://go.dev", "title": "Go"}`)
	var saved Bookmark
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || saved.Owner != "ann" {
		t.Fatalf("ann saves: %d %s", w.Code, w.Body)
	}
	if w := call(api, "GET", "/bookmarks/"+saved.ID, ann, ""); w.Code != http.StatusOK {
		t.Fatalf("ann reads hers: %d", w.Code)
	}
	if w := call(api, "GET", "/bookmarks/"+saved.ID, bob, ""); w.Code != http.StatusForbidden {
		t.Fatalf("bob reads ann's: %d", w.Code)
	}
}

// call sends a request with an API key, or none when key is empty.
func call(api http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)
	return w
}

// docs:end test

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
	if _, err := conn.Exec(ctx, "DROP TABLE IF EXISTS bookmarks, api_keys, galleon_migrations, galleon_migrations_fence"); err != nil {
		t.Error(err)
	}
}

func TestTheAppBoots(t *testing.T) {
	if _, err := galleon.New(options(galleon.Development)...); err != nil {
		t.Fatal(err)
	}
}
