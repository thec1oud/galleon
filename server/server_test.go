package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		`{"http":{"read_header_timeout":0}}`:    "read_header_timeout must be positive",
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

type HeaderIn struct {
	Tenant string `header:"X-Tenant" json:"-"`
}

type BadOut struct {
	C chan int `json:"c"`
}

type textCodec struct{}

func (textCodec) MediaType() string { return "text/plain" }
func (textCodec) Encode(w io.Writer, v any) error {
	_, err := io.WriteString(w, v.(*CreateOut).Customer)
	return err
}
func (textCodec) Decode(r io.Reader, v any, _ op.UnknownFields) error {
	b, err := io.ReadAll(r)
	v.(*CreateIn).Customer, v.(*CreateIn).Qty = string(b), 1
	return err
}

func extraOps(release <-chan struct{}, started chan<- struct{}) galleon.Module {
	return galleon.ModuleFunc("extra", func(r *galleon.Registry) error {
		r.Operation(op.Unary[HeaderIn, CreateOut]("x.header").HTTP("GET", "/tenant").Public(), func(_ context.Context, in *HeaderIn) (*CreateOut, error) {
			return &CreateOut{Org: in.Tenant}, nil
		})
		r.Operation(op.Unary[Empty, BadOut]("x.bad").HTTP("GET", "/bad").Public(), func(context.Context, *Empty) (*BadOut, error) {
			return &BadOut{C: make(chan int)}, nil
		})
		r.Operation(op.Unary[Empty, Empty]("x.slow").HTTP("GET", "/slow").Public(), func(context.Context, *Empty) (*Empty, error) {
			started <- struct{}{}
			<-release
			return &Empty{}, nil
		})
		d := op.Unary[CreateIn, CreateOut]("x.text").HTTP("POST", "/text").Public().Describe()
		d.Input.Codecs = []op.Codec{textCodec{}}
		d.Output.Codecs = []op.Codec{textCodec{}}
		r.Operation(&d, func(_ context.Context, in *CreateIn) (*CreateOut, error) {
			return &CreateOut{Customer: strings.ToUpper(in.Customer)}, nil
		})
		return nil
	})
}

func TestHeaderParameters(t *testing.T) {
	e := boot(t, "", extraOps(nil, nil))
	w := e.do("GET", "/tenant", "", "X-Tenant", "acme")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"org":"acme"`) {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
}

func TestEncodingFailureIsACleanInternalError(t *testing.T) {
	e := boot(t, "", extraOps(nil, nil))
	w := e.do("GET", "/bad", "")
	if p := problem(t, w); w.Code != 500 || p.Code != "galleon.internal" {
		t.Fatalf("got %d %+v", w.Code, p)
	}
}

func TestAnOperationsOwnCodecs(t *testing.T) {
	e := boot(t, "", extraOps(nil, nil))
	r := httptest.NewRequest("POST", "/text", strings.NewReader("ann"))
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Accept", "text/plain")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "ANN" || w.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("got %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	if w := e.do("POST", "/orgs/a/orders", "ann", "Content-Type", "text/plain"); w.Code != 415 {
		t.Fatalf("another operation doesn't get the codec: %d", w.Code)
	}
}

func listening(t *testing.T, e *env) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.srv.Addr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the server never listened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return e.srv.Addr().String()
}

func TestDrainWaitsForInFlightRequests(t *testing.T) {
	release, started := make(chan struct{}), make(chan struct{})
	e := boot(t, "", extraOps(release, started))
	url := "http://" + listening(t, e)
	got := make(chan int, 1)
	go func() {
		res, err := http.Get(url + "/slow")
		if err != nil {
			got <- 0
			return
		}
		res.Body.Close()
		got <- res.StatusCode
	}()
	<-started
	done := make(chan error, 1)
	go func() { done <- e.app.Shutdown(context.Background()) }()
	select {
	case <-done:
		t.Fatal("shutdown returned while a request was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if code := <-got; code != 200 {
		t.Fatalf("the in-flight request got %d", code)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPanicAfterTheResponseStartedCutsTheConnection(t *testing.T) {
	mw := galleon.ModuleFunc("streamer", func(r *galleon.Registry) error {
		r.Middleware(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/stream" {
					next.ServeHTTP(w, r)
					return
				}
				w.Header().Set("Set-Cookie", "a=b")
				_, _ = io.WriteString(w, "part1")
				w.(http.Flusher).Flush()
				panic("boom")
			})
		})
		return nil
	})
	e := boot(t, "", mw)
	res, err := http.Get("http://" + listening(t, e) + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if _, err := io.ReadAll(res.Body); err == nil {
		t.Fatal("a response cut by a panic must not read as complete")
	}
}

func TestPanicResponseDropsTheHandlersHeaders(t *testing.T) {
	mw := galleon.ModuleFunc("cookie", func(r *galleon.Registry) error {
		r.Middleware(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Set-Cookie", "a=b")
				w.Header().Set("Content-Encoding", "gzip")
				next.ServeHTTP(w, r)
			})
		})
		return nil
	})
	w := boot(t, "", mw).do("GET", "/panic", "")
	if w.Code != 500 || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Content-Encoding") != "" || w.Header().Get("X-Request-Id") == "" {
		t.Fatalf("got %d %v", w.Code, w.Header())
	}
}

func TestUnixSocketReplacesAStaleFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "gs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.sock")
	// A listener closed without unlinking leaves the file behind, as a crash does.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()

	e := boot(t, `{"http":{"addr":"unix:`+path+`","trusted_proxies":["unix"]}}`)
	listening(t, e)
	c := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	get := func(xff string) int {
		r, _ := http.NewRequest("GET", "http://x/orders", nil)
		r.Header.Set("X-Forwarded-For", xff)
		res, err := c.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	// Behind a proxy trusted as "unix", each forwarded client has its own limit.
	if get("203.0.113.1") != 200 || get("203.0.113.2") != 200 || get("203.0.113.1") != 429 {
		t.Fatal("the forwarded address must key the rate limit")
	}
}

func TestPanicAfterEarlyHintsIsStillAnError(t *testing.T) {
	mw := galleon.ModuleFunc("hints", func(r *galleon.Registry) error {
		r.Middleware(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Link", "</style.css>; rel=preload")
				w.WriteHeader(http.StatusEarlyHints)
				panic("boom")
			})
		})
		return nil
	})
	e := boot(t, "", mw)
	res, err := http.Get("http://" + listening(t, e) + "/orders")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 500 {
		t.Fatalf("got %d", res.StatusCode)
	}
}

func TestUnixSocketInUseIsLeftAlone(t *testing.T) {
	dir, err := os.MkdirTemp("", "gs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	app, err := galleon.New(
		galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode),
		galleon.Logger(slog.New(slog.DiscardHandler)), galleon.DrainGrace(0),
		galleon.ConfigJSON([]byte(`{"http":{"addr":"unix:`+path+`"}}`)), galleon.Modules(server.Module()),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Run(ctx); err == nil || !strings.Contains(err.Error(), "in use by another process") {
		t.Fatalf("got %v", err)
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("the other process's socket must survive: %v", err)
	}
	c.Close()
}

// serveRecovering serves r and returns what the server panicked with, if
// anything; a real server would cut the connection.
func serveRecovering(h http.Handler, r *http.Request) (p any) {
	defer func() { p = recover() }()
	h.ServeHTTP(httptest.NewRecorder(), r)
	return nil
}

func TestAbortBeforeAnyWriteHasNoStatus(t *testing.T) {
	mw := galleon.ModuleFunc("abort", func(r *galleon.Registry) error {
		r.Middleware(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) })
		})
		return nil
	})
	e := boot(t, accessLog, mw)
	if p := serveRecovering(e.h, httptest.NewRequest("GET", "/orders", nil)); p != http.ErrAbortHandler {
		t.Fatalf("panicked with %v", p)
	}
	if l := e.logs.String(); !strings.Contains(l, "status=0") || !strings.Contains(l, "aborted=true") {
		t.Fatalf("an aborted request must not log as a success: %s", l)
	}
}

func TestSwitchingProtocolsIsAFinalStatus(t *testing.T) {
	mw := galleon.ModuleFunc("upgrade", func(r *galleon.Registry) error {
		r.Middleware(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusSwitchingProtocols)
				panic("boom")
			})
		})
		return nil
	})
	e := boot(t, accessLog, mw)
	if p := serveRecovering(e.h, httptest.NewRequest("GET", "/orders", nil)); p != http.ErrAbortHandler {
		t.Fatalf("a panic after 101 must cut the connection, got %v", p)
	}
	if l := e.logs.String(); !strings.Contains(l, "status=101") {
		t.Fatalf("got %s", l)
	}
}

func TestClientLeavingMidBodyIsNotABadRequest(t *testing.T) {
	e := boot(t, accessLog)
	c, err := net.Dial("tcp", listening(t, e))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(c, "POST /orgs/a/orders HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"customer\":")
	time.Sleep(50 * time.Millisecond)
	c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(e.logs.String(), "http request") {
		if time.Now().After(deadline) {
			t.Fatal("the request was never logged")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if l := e.logs.String(); !strings.Contains(l, "status=499") {
		t.Fatalf("a client that left is not a 400: %s", l)
	}
}

const accessLog = `{"http":{"addr":"127.0.0.1:0","access_log":true}}`
