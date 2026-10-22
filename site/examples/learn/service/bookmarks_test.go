package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thec1oud/galleon/server"
	"github.com/thec1oud/galleon/testkit"
)

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
