package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/server"
)

type CreateIn struct {
	Org      string `path:"org" json:"-"`
	DryRun   bool   `query:"dry_run" json:"-"`
	Customer string `json:"customer" validate:"required"`
	Qty      int    `json:"qty" validate:"min=1"`
}

type CreateOut struct {
	Org      string `json:"org"`
	Customer string `json:"customer"`
	Qty      int    `json:"qty"`
	DryRun   bool   `json:"dry_run"`
}

type Empty struct{}

var (
	createOrder = op.Unary[CreateIn, CreateOut]("orders.create").HTTP("POST", "/orgs/{org}/orders").Public().BodyLimit(256)
	deleteOrder = op.Unary[Empty, Empty]("orders.delete").HTTP("DELETE", "/orders/{id}").Public()
	failing     = op.Unary[Empty, Empty]("orders.fail").HTTP("GET", "/fail").Public()
	panicking   = op.Unary[Empty, Empty]("orders.panic").HTTP("GET", "/panic").Public()
	listOrders  = op.Unary[Empty, CreateOut]("orders.list").HTTP("GET", "/orders").Public().RateLimit(1, time.Hour)
)

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *logBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type env struct {
	app  *galleon.App
	srv  *server.Server
	h    http.Handler
	logs *logBuffer
}

func boot(t *testing.T, cfg string, extra ...galleon.Module) *env {
	t.Helper()
	logs := &logBuffer{}
	mod := galleon.ModuleFunc("orders", func(r *galleon.Registry) error {
		r.Operation(createOrder, func(_ context.Context, in *CreateIn) (*CreateOut, error) {
			return &CreateOut{Org: in.Org, Customer: in.Customer, Qty: in.Qty, DryRun: in.DryRun}, nil
		})
		r.Operation(deleteOrder, func(context.Context, *Empty) (*Empty, error) { return nil, nil })
		r.Operation(failing, func(context.Context, *Empty) (*Empty, error) { return nil, errors.New("database password is hunter2") })
		r.Operation(panicking, func(context.Context, *Empty) (*Empty, error) { panic("boom") })
		r.Operation(listOrders, func(context.Context, *Empty) (*CreateOut, error) { return &CreateOut{}, nil })
		return nil
	})
	if cfg == "" {
		cfg = `{"http":{"addr":"127.0.0.1:0","trusted_proxies":["10.0.0.0/8"]}}`
	}
	app, err := galleon.New(
		galleon.Name("orders"),
		galleon.Logger(slog.New(slog.NewTextHandler(logs, nil))),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
		galleon.DrainGrace(0),
		galleon.ConfigJSON([]byte(cfg)),
		galleon.Modules(append([]galleon.Module{mod, server.Module()}, extra...)...),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	srv := galleon.MustResolve[*server.Server](app)
	return &env{app: app, srv: srv, h: srv.Handler(), logs: logs}
}

func (e *env) do(method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func problem(t *testing.T, w *httptest.ResponseRecorder) errs.Problem {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("want problem+json, got %q: %s", ct, w.Body)
	}
	var p errs.Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServesAnOperation(t *testing.T) {
	e := boot(t, "")
	w := e.do("POST", "/orgs/acme/orders?dry_run=true", `{"customer":"ann","qty":2}`)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %q: %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	var out CreateOut
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out != (CreateOut{Org: "acme", Customer: "ann", Qty: 2, DryRun: true}) {
		t.Fatalf("out: %+v", out)
	}
	routes := e.srv.Routes()
	if len(routes) != 5 || routes[0] != (server.Route{Method: "POST", Path: "/orgs/{org}/orders", Operation: "orders.create"}) {
		t.Fatalf("routes: %v", routes)
	}
}

func TestNilOutputIsNoContent(t *testing.T) {
	if w := boot(t, "").do("DELETE", "/orders/1", ""); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
}

func TestValidationErrorsAreProblems(t *testing.T) {
	w := boot(t, "").do("POST", "/orgs/acme/orders", `{"qty":0}`)
	p := problem(t, w)
	if w.Code != 400 || p.Code != "galleon.invalid_input" {
		t.Fatalf("got %d %+v", w.Code, p)
	}
	v := p.Details["field_violations"].([]any)
	paths := []string{v[0].(map[string]any)["path"].(string), v[1].(map[string]any)["path"].(string)}
	if strings.Join(paths, ",") != "/customer,/qty" {
		t.Fatalf("violations: %v", v)
	}
}

func TestDecodeErrors(t *testing.T) {
	e := boot(t, "")
	cases := []struct {
		body, ct string
		status   int
		code     string
	}{
		{`{"customer":"a","qty":"x"}`, "application/json", 400, "galleon.invalid_body"},
		{`{"customer":"a","extra":1}`, "application/json", 400, "galleon.invalid_body"},
		{`{"customer":"` + strings.Repeat("a", 300) + `"}`, "application/json", 400, "galleon.body_too_large"},
		{`<order/>`, "application/xml", 415, "galleon.unsupported_media_type"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/orgs/a/orders", strings.NewReader(c.body))
		r.Header.Set("Content-Type", c.ct)
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if p := problem(t, w); w.Code != c.status || p.Code != c.code {
			t.Errorf("%s: got %d %s, want %d %s", c.body, w.Code, p.Code, c.status, c.code)
		}
		if c.status == 415 && w.Header().Get("Accept") != "application/json" {
			t.Errorf("415 must list the accepted types, got %q", w.Header().Get("Accept"))
		}
	}
	if w := e.do("POST", "/orgs/a/orders?dry_run=perhaps", `{"customer":"a","qty":1}`); w.Code != 400 {
		t.Fatalf("a bad query parameter is a 400, got %d", w.Code)
	}
}

func TestNotAcceptable(t *testing.T) {
	w := boot(t, "").do("GET", "/orders", "", "Accept", "text/html")
	if p := problem(t, w); w.Code != 406 || p.Code != "galleon.not_acceptable" {
		t.Fatalf("got %d %+v", w.Code, p)
	}
}

func TestUnknownRoutesAndMethods(t *testing.T) {
	e := boot(t, "")
	w := e.do("GET", "/nope", "")
	if p := problem(t, w); w.Code != 404 || p.Code != "galleon.route_not_found" {
		t.Fatalf("got %d %+v", w.Code, p)
	}
	w = e.do("PUT", "/orders", "")
	if p := problem(t, w); w.Code != 405 || p.Code != "galleon.method_not_allowed" || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("got %d %+v, Allow %q", w.Code, p, w.Header().Get("Allow"))
	}
}

func TestInternalErrorsHideTheCause(t *testing.T) {
	e := boot(t, "")
	tp := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	w := e.do("GET", "/fail", "", "traceparent", tp)
	p := problem(t, w)
	if w.Code != 500 || p.Code != "galleon.internal" || strings.Contains(w.Body.String(), "hunter2") {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
	if p.Instance != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("the problem's instance is the trace ID from traceparent, got %q", p.Instance)
	}
	if !strings.Contains(e.logs.String(), "hunter2") {
		t.Fatal("the cause must be logged")
	}
}

func TestPanicsBecomeInternalErrors(t *testing.T) {
	e := boot(t, "")
	w := e.do("GET", "/panic", "")
	if p := problem(t, w); w.Code != 500 || p.Code != "galleon.internal" {
		t.Fatalf("got %d %+v", w.Code, p)
	}
	if !strings.Contains(e.logs.String(), "panic while serving") {
		t.Fatal("the panic must be logged")
	}
}

func TestRequestIDsAndSecurityHeaders(t *testing.T) {
	e := boot(t, "")
	w := e.do("DELETE", "/orders/1", "", "X-Request-Id", "abc-123")
	if w.Header().Get("X-Request-Id") != "abc-123" {
		t.Fatalf("a valid incoming request ID is kept, got %q", w.Header().Get("X-Request-Id"))
	}
	w = e.do("DELETE", "/orders/1", "", "X-Request-Id", "bad id\n")
	if id := w.Header().Get("X-Request-Id"); len(id) != 32 {
		t.Fatalf("an invalid request ID is replaced, got %q", id)
	}
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"} {
		if w.Header().Get(k) != v {
			t.Errorf("%s: %q", k, w.Header().Get(k))
		}
	}
}

func TestRateLimitUsesTheRealClientAddress(t *testing.T) {
	e := boot(t, "")
	get := func(remote, xff string) int {
		r := httptest.NewRequest("GET", "/orders", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		return w.Code
	}
	// Through the trusted proxy, two clients get their own limits; the
	// spoofed left-most hop is ignored.
	if get("10.1.1.1:5000", "1.2.3.4, 203.0.113.7") != 200 || get("10.1.1.1:5001", "9.9.9.9, 203.0.113.7") != 429 {
		t.Fatal("the right-most untrusted hop is the client")
	}
	if get("10.1.1.1:5002", "203.0.113.8") != 200 {
		t.Fatal("another client behind the proxy has its own limit")
	}
	// An untrusted peer can't choose its address with the header.
	if get("198.51.100.1:5000", "203.0.113.9") != 200 || get("198.51.100.1:5001", "203.0.113.10") != 429 {
		t.Fatal("X-Forwarded-For from an untrusted peer must be ignored")
	}
}

func TestModuleMiddlewareWrapsTheRouter(t *testing.T) {
	mw := galleon.ModuleFunc("cors", func(r *galleon.Registry) error {
		r.Middleware(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				next.ServeHTTP(w, r)
			})
		})
		return nil
	})
	w := boot(t, "", mw).do("GET", "/nope", "")
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("module middleware must run for every request")
	}
}

func TestListensWhenReadyAndDrains(t *testing.T) {
	e := boot(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for e.srv.Addr() == nil {
		if ctx.Err() != nil {
			t.Fatal("the server never listened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	url := "http://" + e.srv.Addr().String()
	res, err := http.Post(url+"/orgs/acme/orders", "application/json", strings.NewReader(`{"customer":"ann","qty":1}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), `"org":"acme"`) {
		t.Fatalf("got %d: %s", res.StatusCode, body)
	}
	if err := e.app.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(url + "/orders"); err == nil {
		t.Fatal("the listener must be closed after shutdown")
	}
}

func TestConfigValidation(t *testing.T) {
	for cfg, want := range map[string]string{
		`{"http":{"tls_cert":"a.pem"}}`:         "tls_cert and tls_key must be set together",
		`{"http":{"trusted_proxies":["nope"]}}`: "trusted_proxies",
		`{"http":{"addr":""}}`:                  "addr must be set",
		`{"http":{"body_limit":0}}`:             "must be positive",
	} {
		_, err := galleon.New(
			galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode),
			galleon.Logger(slog.New(slog.DiscardHandler)),
			galleon.ConfigJSON([]byte(cfg)), galleon.Modules(server.Module()),
		)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", cfg, want, err)
		}
	}
}

func TestOnlyTheWebRoleServes(t *testing.T) {
	app, err := galleon.New(
		galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode),
		galleon.Logger(slog.New(slog.DiscardHandler)),
		galleon.Roles("worker"), galleon.Modules(server.Module(), galleon.ModuleFunc("w", func(r *galleon.Registry) error {
			r.Active("worker")
			return nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	if _, err := galleon.Resolve[*server.Server](app); err == nil {
		t.Fatal("a worker process must not build the HTTP server")
	}
}
