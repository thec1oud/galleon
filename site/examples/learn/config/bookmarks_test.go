package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/server"
	"github.com/thec1oud/galleon/testkit"
)

// docs:start test
// api boots the app and returns its HTTP handler.
func api(t *testing.T) http.Handler {
	t.Setenv("BOOKMARKS_HTTP_ADDR", "127.0.0.1:0") // a free port, not :8080
	h := testkit.New(t, options()...)
	return testkit.Get[*server.Server](h).Handler()
}

func TestABookmarkCanBeSavedAndRead(t *testing.T) {
	api := api(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/bookmarks", strings.NewReader(`{"url": "https://go.dev", "title": "Go"}`))
	req.Header.Set("Content-Type", "application/json")
	api.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var saved Bookmark
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}

	w = httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest("GET", "/bookmarks/"+saved.ID, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"title":"Go"`) {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
}

// docs:end test

func TestAMissingBookmarkIsNotFound(t *testing.T) {
	w := httptest.NewRecorder()
	api(t).ServeHTTP(w, httptest.NewRequest("GET", "/bookmarks/9", nil))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"code":"bookmarks.not_found"`) {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
}

func TestAnInvalidBookmarkIsRefused(t *testing.T) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/bookmarks", strings.NewReader(`{"url": "not a url", "title": ""}`))
	req.Header.Set("Content-Type", "application/json")
	api(t).ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"galleon.invalid_input"`) {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
}

// docs:start limit
func TestTheStoreKeepsItsLimit(t *testing.T) {
	h := testkit.New(t,
		galleon.Modules(Bookmarks{}),
		galleon.ConfigJSON([]byte(`{"limits": {"max_bookmarks": 1}}`)),
	)
	store := testkit.Get[*Store](h)
	ctx := context.Background()
	if _, err := store.Create(ctx, &CreateIn{URL: "https://go.dev", Title: "Go"}); err != nil {
		t.Fatal(err)
	}
	_, err := store.Create(ctx, &CreateIn{URL: "https://pkg.go.dev", Title: "Packages"})
	if errs.KindOf(err) != errs.FailedPrecondition {
		t.Fatalf("the second bookmark: %v, want the store to be full", err)
	}
}

// docs:end limit

func TestAnInvalidLimitFailsAtBoot(t *testing.T) {
	_, err := galleon.New(
		galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode),
		galleon.Modules(Bookmarks{}), galleon.ConfigJSON([]byte(`{"limits": {"max_bookmarks": 0}}`)),
	)
	if err == nil || !strings.Contains(err.Error(), "max_bookmarks must be at least 1") {
		t.Fatalf("New = %v", err)
	}
	t.Log(err)
}

func TestTheEnvironmentWinsOverTheFile(t *testing.T) {
	t.Setenv("BOOKMARKS_LIMITS_MAX_BOOKMARKS", "7")
	h := testkit.New(t, galleon.Name("bookmarks"), galleon.Modules(Bookmarks{}), galleon.ConfigJSON([]byte(`{"limits": {"max_bookmarks": 500}}`)))
	if got := testkit.Get[*Limits](h).MaxBookmarks; got != 7 {
		t.Errorf("max_bookmarks = %d, want 7", got)
	}
}

func TestTheAppReadsConfigJSON(t *testing.T) {
	t.Setenv("BOOKMARKS_HTTP_ADDR", "127.0.0.1:0")
	h := testkit.New(t, options()...)
	if got := testkit.Get[*Limits](h).MaxBookmarks; got != 500 {
		t.Errorf("max_bookmarks = %d, want 500 from config.json", got)
	}
}
