package protect

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/security"
	"github.com/thec1oud/galleon/server"
	"github.com/thec1oud/galleon/testkit"
)

// docs:start test
func TestPeopleSeeOnlyTheirOwnNotes(t *testing.T) {
	sec := testkit.NewSecurity() // stands in for the token issuer
	sec.User("ann")
	sec.User("bob")
	sec.User("root", "admin")
	t.Setenv("TEST_HTTP_ADDR", "127.0.0.1:0")
	h := testkit.New(t, galleon.Modules(server.Module(), Security(security.Authenticate(sec)), Notes{}))
	api := testkit.Get[*server.Server](h).Handler()

	// Without credentials: 401, with a challenge naming the scheme.
	if w := call(api, "GET", "/notes", "", ""); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Test" {
		t.Fatalf("anonymous: %d %v", w.Code, w.Header())
	}
	w := call(api, "POST", "/notes", "ann", `{"text": "ann's"}`)
	var note Note
	if err := json.Unmarshal(w.Body.Bytes(), &note); err != nil || note.Owner != "ann" {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	// Bob may read notes, but not ann's.
	if w := call(api, "GET", "/notes/"+note.ID, "bob", ""); w.Code != http.StatusForbidden {
		t.Fatalf("bob reads ann's note: %d", w.Code)
	}
	if w := call(api, "GET", "/notes", "bob", ""); !strings.Contains(w.Body.String(), `"notes":[]`) {
		t.Fatalf("bob's list: %s", w.Body)
	}
	// An admin may read anyone's.
	if w := call(api, "GET", "/notes/"+note.ID, "root", ""); w.Code != http.StatusOK {
		t.Fatalf("admin reads ann's note: %d", w.Code)
	}
	// Stats need an organization, and ann's test token names none.
	if w := call(api, "GET", "/org/stats", "ann", ""); w.Code != http.StatusForbidden {
		t.Fatalf("stats outside an org: %d", w.Code)
	}
}

// call sends a request as user, or anonymously when user is empty.
func call(api http.Handler, method, path, user, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("Authorization", "Test "+user)
	}
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)
	return w
}

// docs:end test

func TestTheAppBoots(t *testing.T) {
	if _, err := galleon.New(options()...); err != nil {
		t.Fatal(err)
	}
}

func TestAnAnalystSeesTheirOrgsStats(t *testing.T) {
	sec := testkit.NewSecurity().InContext("org", "acme")
	sec.User("ann", "analyst")
	t.Setenv("TEST_HTTP_ADDR", "127.0.0.1:0")
	h := testkit.New(t, galleon.Modules(server.Module(), security.Module(security.Authenticate(sec), security.ResolveContext(sec), security.Decide(policy)), Notes{}))
	api := testkit.Get[*server.Server](h).Handler()
	if w := call(api, "GET", "/org/stats", "ann", ""); w.Code != http.StatusOK {
		t.Fatalf("stats in an org: %d %s", w.Code, w.Body)
	}
}
