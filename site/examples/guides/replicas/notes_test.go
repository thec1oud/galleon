package notes

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/pg"
	"github.com/thec1oud/galleon/server"
	"github.com/thec1oud/galleon/testkit"
)

//go:embed schema.sql
var schema string

// docs:start config
// config is the "pg" section with one replica, and a key that signs the
// token a write returns.
func config(primary, replica string) []byte {
	return []byte(`{"pg": {
		"primary": {"dsn": "` + primary + `"},
		"replicas": [{"dsn": "` + replica + `"}],
		"max_lag": 60000000000,
		"token_key": "a key of at least 32 bytes, from a secret store"
	}}`)
}

// docs:end config

// docs:start test
func TestTheTokenReadsTheCallersOwnWrite(t *testing.T) {
	db := testkit.DB(t, testkit.Schema(schema)) // skips unless GALLEON_TEST_POSTGRES is set
	rep := db.Replica(t)                        // skips unless GALLEON_TEST_POSTGRES_REPLICA is set
	t.Cleanup(func() { deleteAll(t, db.DSN()) })
	t.Setenv("NOTES_HTTP_ADDR", "127.0.0.1:0")
	h := testkit.New(t,
		galleon.Name("notes"),
		galleon.Modules(server.Module(), pg.Module(), Notes{}),
		galleon.ConfigJSON(config(db.DSN(), rep.DSN())),
	)
	api := testkit.Get[*server.Server](h).Handler()

	rep.Pause() // from here on, the replica lags behind every write

	w := call(api, "POST", "/notes", `{"body": "replicas lag"}`, "")
	var saved Note
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	token := w.Header().Get(pg.ConsistencyHeader)
	if token == "" {
		t.Fatal("the write returned no token")
	}

	// With the token, the read goes to the primary: the replica hasn't
	// replayed the write.
	if w := call(api, "GET", "/notes/"+saved.ID, "", token); w.Code != http.StatusOK {
		t.Fatalf("with the token: %d %s", w.Code, w.Body)
	}
	// Without it, the replica qualifies, and doesn't have the note yet.
	// (The tracker polls in the background, so the replica joins the
	// rotation a moment after boot.)
	eventually(t, "a read without the token to miss the note", func() bool {
		return call(api, "GET", "/notes/"+saved.ID, "", "").Code == http.StatusNotFound
	})
	// Count reads any replica, so it is behind too.
	if w := call(api, "GET", "/notes/count", "", ""); !strings.Contains(w.Body.String(), `"notes":0`) {
		t.Fatalf("count: %s", w.Body)
	}
}

// call sends a request to api, with a token when it isn't empty.
func call(api http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set(pg.ConsistencyHeader, token)
	}
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)
	return w
}

// docs:end test

// eventually polls cond for up to 5 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waited 5s for %s", what)
		}
	}
}

// deleteAll removes the notes the test committed.
func deleteAll(t *testing.T, dsn string) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DELETE FROM notes"); err != nil {
		t.Error(err)
	}
}
