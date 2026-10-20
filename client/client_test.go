package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/client"
	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/errs"
)

type Item struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// local lets clients reach httptest servers, which the egress guard refuses.
var loopback = client.Configure(func(c *client.Config) {
	c.AllowNetworks = []string{"127.0.0.0/8", "::1/128"}
	c.RetryBackoff, c.RetryBackoffMax = time.Millisecond, 2*time.Millisecond
})

func newClient(t *testing.T, srv *httptest.Server, opts ...client.Option) *client.Client {
	t.Helper()
	c, err := client.New("test", append([]client.Option{client.BaseURL(srv.URL), loopback}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSendRoundTripsJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/items":
			var in Item
			_ = json.NewDecoder(r.Body).Decode(&in)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Item{ID: "1", Name: in.Name + "!"})
		case "/api/empty":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := client.New("test", client.BaseURL(srv.URL+"/api"), loopback)
	if err != nil {
		t.Fatal(err)
	}
	out, err := client.Send[Item](context.Background(), c, "POST", "/items", Item{Name: "pen"})
	if err != nil || out.ID != "1" || out.Name != "pen!" {
		t.Fatalf("got %+v, %v", out, err)
	}
	if out, err := client.Send[Item](context.Background(), c, "DELETE", "empty", nil); err != nil || *out != (Item{}) {
		t.Fatalf("204 gives a zero value: %+v, %v", out, err)
	}
}

func TestStatusErrorsCarryTheProblem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		errs.WriteProblem(w, errs.New(errs.NotFound, "orders.not_found", "no such order"), "")
	}))
	defer srv.Close()
	_, err := client.Send[Item](context.Background(), newClient(t, srv), "GET", "/x", nil)
	var se *client.StatusError
	if !errors.As(err, &se) || se.Status != 404 || se.Problem == nil || se.Problem.Code != "orders.not_found" || se.Kind() != errs.NotFound {
		t.Fatalf("got %#v", err)
	}
	if errs.KindOf(err) != errs.Internal {
		t.Fatal("an upstream's error must not become the caller's own kind by default")
	}
}

func TestResponseSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"name":"`+strings.Repeat("x", 100)+`"}`)
	}))
	defer srv.Close()
	c := newClient(t, srv, client.Configure(func(c *client.Config) { c.MaxResponseBytes = 50 }))
	if _, err := client.Send[Item](context.Background(), c, "GET", "/", nil); err == nil || !strings.Contains(err.Error(), "galleon.upstream_too_large") {
		t.Fatalf("got %v", err)
	}
}

func TestRetriesIdempotentRequests(t *testing.T) {
	var hits atomic.Int32
	var bodies sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		bodies.Store(n, string(b))
		if n%3 != 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"id":"ok"}`)
	}))
	defer srv.Close()
	c := newClient(t, srv)

	out, err := client.Send[Item](context.Background(), c, "PUT", "/", Item{Name: "same"})
	if err != nil || out.ID != "ok" || hits.Load() != 3 {
		t.Fatalf("got %+v, %v after %d attempts", out, err, hits.Load())
	}
	for i := int32(1); i <= 3; i++ {
		if b, _ := bodies.Load(i); !strings.Contains(b.(string), "same") {
			t.Fatalf("attempt %d got body %q; the body must be replayed", i, b)
		}
	}

	hits.Store(0)
	if _, err := client.Send[Item](context.Background(), c, "POST", "/", Item{}); err == nil || hits.Load() != 1 {
		t.Fatalf("a POST isn't retried: %v after %d attempts", err, hits.Load())
	}

	hits.Store(0)
	req, _ := http.NewRequest("POST", "/", strings.NewReader("{}"))
	req.Header.Set("Idempotency-Key", "k1")
	res, err := c.Do(req)
	if err != nil || res.StatusCode != 200 || hits.Load() != 3 {
		t.Fatalf("a POST with an Idempotency-Key is retried: %v %v after %d", res, err, hits.Load())
	}
	res.Body.Close()
}

func TestRetryAfterAndItsLimit(t *testing.T) {
	var hits atomic.Int32
	after := "0"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", after)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := newClient(t, srv)
	if _, err := client.Send[Item](context.Background(), c, "GET", "/", nil); err != nil || hits.Load() != 2 {
		t.Fatalf("a 429 is retried: %v after %d", err, hits.Load())
	}
	hits.Store(0)
	after = "3600"
	_, err := client.Send[Item](context.Background(), c, "GET", "/", nil)
	var se *client.StatusError
	if !errors.As(err, &se) || se.Status != 429 || hits.Load() != 1 {
		t.Fatalf("a Retry-After past max_retry_after returns the response: %v after %d", err, hits.Load())
	}
	for _, v := range []string{"9300000000", "99999999999999999999999"} {
		hits.Store(0)
		after = v
		_, err = client.Send[Item](context.Background(), c, "GET", "/", nil)
		if !errors.As(err, &se) || se.Status != 429 || hits.Load() != 1 {
			t.Fatalf("Retry-After %s mustn't overflow into a short wait: %v after %d", v, err, hits.Load())
		}
	}
}

func TestRetriesStopAtTheCallersDeadline(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := newClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if _, err := client.Send[Item](ctx, c, "GET", "/", nil); err == nil || hits.Load() != 1 || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("a retry that can't finish in time isn't attempted: %v after %d in %v", err, hits.Load(), time.Since(start))
	}
}

func TestRetryBudget(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := newClient(t, srv, client.Configure(func(c *client.Config) { c.RetryBudget, c.RetryBudgetBurst = 0, 1 }))
	for range 3 {
		_, _ = client.Send[Item](context.Background(), c, "GET", "/", nil)
	}
	if hits.Load() != 4 {
		t.Fatalf("one retry in the budget: want 4 attempts for 3 requests, got %d", hits.Load())
	}
}

func TestCircuitBreaker(t *testing.T) {
	var hits atomic.Int32
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !healthy.Load() {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	clk := clock.NewFake(time.Unix(0, 0))
	c := newClient(t, srv, client.Clock(clk), client.Configure(func(c *client.Config) {
		c.Retries, c.BreakerFailures, c.BreakerOpenFor = 0, 3, time.Minute
	}))
	get := func() error {
		_, err := client.Send[Item](context.Background(), c, "GET", "/", nil)
		return err
	}
	for range 3 {
		_ = get()
	}
	err := get()
	if e := errs.From(err); e.Kind != errs.Unavailable || e.Code != "galleon.circuit_open" || hits.Load() != 3 {
		t.Fatalf("an open circuit fails fast: %v after %d", err, hits.Load())
	}
	clk.Advance(time.Minute)
	healthy.Store(true)
	if err := get(); err != nil || hits.Load() != 4 {
		t.Fatalf("after the open period one probe goes through: %v after %d", err, hits.Load())
	}
	healthy.Store(false)
	for range 2 {
		_ = get()
	}
	if err := get(); errs.From(err).Code == "galleon.circuit_open" {
		t.Fatal("a success closes the circuit and resets the count")
	}
}

func TestMaxConcurrent(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	c := newClient(t, srv, client.Configure(func(c *client.Config) { c.MaxConcurrent = 1 }))
	go func() { _, _ = client.Send[Item](context.Background(), c, "GET", "/", nil) }()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.Send[Item](ctx, c, "GET", "/", nil); errs.KindOf(err) != errs.DeadlineExceeded {
		t.Fatalf("a request waiting for a slot gives up with its context: %v", err)
	}
}

func TestAttemptTimeoutCoversTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		if r.URL.Path == "/slow" {
			time.Sleep(300 * time.Millisecond)
		} else {
			time.Sleep(50 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "done")
	}))
	defer srv.Close()
	c := newClient(t, srv, client.Configure(func(c *client.Config) { c.Timeout, c.Retries = 150*time.Millisecond, 0 }))
	res, err := c.Do(must(http.NewRequest("GET", "/fast", nil)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || string(b) != "done" {
		t.Fatalf("the body stays readable after Do returns: %q %v", b, err)
	}
	res, err = c.Do(must(http.NewRequest("GET", "/slow", nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(res.Body)
	res.Body.Close()
	if err == nil {
		t.Fatal("reading the body is bounded by the attempt timeout")
	}
}

func must(r *http.Request, err error) *http.Request {
	if err != nil {
		panic(err)
	}
	return r
}

func TestEgressGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c, err := client.New("test")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)} {
		_, err := client.Send[Item](context.Background(), c, "GET", u, nil)
		if e := errs.From(err); e.Kind != errs.PermissionDenied || e.Code != "galleon.egress_denied" {
			t.Fatalf("%s: got %v", u, err)
		}
	}
	open, err := client.New("test", client.Configure(func(c *client.Config) { c.AllowPrivate = true }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send[Item](context.Background(), open, "GET", srv.URL, nil); err != nil {
		t.Fatalf("allow_private turns the guard off: %v", err)
	}
}

func TestEgressGuardRanges(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8":              true,
		"2606:4700::1111":      true,
		"127.0.0.1":            false,
		"10.1.2.3":             false,
		"172.16.0.1":           false,
		"192.168.1.1":          false,
		"169.254.169.254":      false,
		"100.64.0.1":           false,
		"0.0.0.0":              false,
		"255.255.255.255":      false,
		"224.0.0.1":            false,
		"::1":                  false,
		"fe80::1":              false,
		"fd00::1":              false,
		"::ffff:127.0.0.1":     false,
		"64:ff9b::a9fe:a9fe":   false, // 169.254.169.254 through NAT64
		"64:ff9b::808:808":     true,
		"64:ff9b:1::a9fe:a9fe": false, // local-use NAT64
		"2002:a9fe:a9fe::1":    false, // 169.254.169.254 through 6to4
		"2002:808:808::1":      true,
		"::a9fe:a9fe":          false, // IPv4-compatible
		"::808:808":            false,
		"fec0::1":              false, // site-local
	} {
		if got := client.Permits(netip.MustParseAddr(addr), nil); got != want {
			t.Errorf("%s: permitted %v, want %v", addr, got, want)
		}
	}
	if !client.Permits(netip.MustParseAddr("10.1.2.3"), []string{"10.1.0.0/16"}) {
		t.Error("allow_networks opens a range")
	}
	if !client.Permits(netip.MustParseAddr("fec0::1"), []string{"fec0::/10"}) {
		t.Error("allow_networks opens the newly blocked ranges too")
	}
}

func TestCredentialsStayWithTheirHost(t *testing.T) {
	var leaked atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization"))
	}))
	defer other.Close()
	var seen atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, other.URL+"/landing", http.StatusFound)
		}
	}))
	defer srv.Close()
	c := newClient(t, srv, client.WithCredentials(client.Bearer("s3cret")))
	if _, err := client.Send[Item](context.Background(), c, "GET", "/redirect", nil); err != nil {
		t.Fatal(err)
	}
	if seen.Load() != "Bearer s3cret" {
		t.Fatalf("the base host gets the credentials: %v", seen.Load())
	}
	if v := leaked.Load(); v != "" {
		t.Fatalf("a redirect to another host must not carry them: %v", v)
	}
	if _, err := client.New("test", client.WithCredentials(client.Bearer("x"))); err == nil {
		t.Fatal("credentials with no BaseURL and no hosts must be refused")
	}
}

func TestMiddlewareWrapsTheBuiltInLayers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Saw", r.Header.Get("X-Mw")+"/"+r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	var auth string
	mw := func(next http.RoundTripper) http.RoundTripper {
		return rtFunc(func(r *http.Request) (*http.Response, error) {
			auth = r.Header.Get("Authorization")
			r = r.Clone(r.Context())
			r.Header.Set("X-Mw", "1")
			return next.RoundTrip(r)
		})
	}
	c := newClient(t, srv, client.Use(mw), client.WithCredentials(client.Bearer("t")))
	res, err := c.HTTP().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("X-Saw") != "1/Bearer t" || auth != "" {
		t.Fatalf("middleware runs outside credentials: saw %q, middleware saw auth %q", res.Header.Get("X-Saw"), auth)
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTracing(t *testing.T) {
	var hits atomic.Int32
	var traceparent atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent.Store(r.Header.Get("Traceparent"))
		if hits.Add(1) == 1 {
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	tp := &recordingTP{}
	c := newClient(t, srv, client.TracerProvider(tp))
	if _, err := client.Send[Item](context.Background(), c, "GET", "/items?token=abc", nil); err != nil {
		t.Fatal(err)
	}
	if v, _ := traceparent.Load().(string); v == "" {
		t.Fatal("the trace context must be sent")
	}
	spans := tp.all()
	if len(spans) != 2 {
		t.Fatalf("one span per attempt, got %d", len(spans))
	}
	first, second := spans[0], spans[1]
	if first.attr("http.response.status_code") != "503" || first.attr("error.type") != "503" ||
		!strings.Contains(first.attr("url.full"), "token=REDACTED") || strings.Contains(first.attr("url.full"), "abc") ||
		first.attr("http.request.method") != "GET" || first.attr("server.address") != "127.0.0.1" {
		t.Fatalf("first attempt: %v", first.attrs)
	}
	if second.attr("http.request.resend_count") != "1" || second.attr("http.response.status_code") != "200" {
		t.Fatalf("second attempt: %v", second.attrs)
	}

	// A refused address, a status error and a body read by the caller.
	_, _ = client.Send[Item](context.Background(), c, "GET", "http://169.254.169.254/", nil)
	hits.Store(0)
	_, _ = client.Send[Item](context.Background(), c, "GET", "/", nil)
	res, err := c.Do(must(http.NewRequest("GET", "/", nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(res.Body)
	res.Body.Close()
	spans = tp.all()
	if len(spans) != 6 {
		t.Fatalf("want 6 spans, got %d", len(spans))
	}
	for i, s := range spans {
		if !s.isEnded() {
			t.Errorf("span %d was started and never ended: %v", i, s.attrs)
		}
	}
}

func TestModule(t *testing.T) {
	app, err := galleon.New(
		galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode),
		galleon.Logger(slog.New(slog.DiscardHandler)),
		galleon.DrainGrace(0),
		galleon.ConfigJSON([]byte(`{"client":{"retries":1,"allow_networks":["127.0.0.0/8"]}}`)),
		galleon.Modules(client.Module()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	cs := galleon.MustResolve[*client.Clients](app)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c, err := cs.New("billing", client.BaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send[Item](context.Background(), c, "GET", "/", nil); err != nil {
		t.Fatalf("the section's allow_networks applies: %v", err)
	}
	_, err = galleon.New(
		galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode),
		galleon.Logger(slog.New(slog.DiscardHandler)),
		galleon.ConfigJSON([]byte(`{"client":{"retries":-1}}`)),
		galleon.Modules(client.Module()),
	)
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("an invalid section fails boot: %v", err)
	}
}

// recordingTP records the spans' attributes.
type recordingTP struct {
	embedded.TracerProvider
	mu    sync.Mutex
	spans []*recSpan
}

func (p *recordingTP) Tracer(string, ...trace.TracerOption) trace.Tracer { return recTracer{p: p} }

func (p *recordingTP) all() []*recSpan {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*recSpan(nil), p.spans...)
}

type recTracer struct {
	embedded.Tracer
	p *recordingTP
}

func (t recTracer) Start(ctx context.Context, _ string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	s := &recSpan{attrs: map[attribute.Key]string{}}
	cfg := trace.NewSpanStartConfig(opts...)
	s.SetAttributes(cfg.Attributes()...)
	t.p.mu.Lock()
	t.p.spans = append(t.p.spans, s)
	t.p.mu.Unlock()
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{byte(len(t.p.spans))}, TraceFlags: trace.FlagsSampled})
	s.sc = sc
	return trace.ContextWithSpan(ctx, s), s
}

type recSpan struct {
	noop.Span
	mu    sync.Mutex
	sc    trace.SpanContext
	attrs map[attribute.Key]string
	ended bool
}

func (s *recSpan) End(...trace.SpanEndOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended = true
}

func (s *recSpan) isEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

func (s *recSpan) SpanContext() trace.SpanContext { return s.sc }
func (s *recSpan) IsRecording() bool              { return true }
func (s *recSpan) SetAttributes(kv ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range kv {
		s.attrs[a.Key] = a.Value.Emit()
	}
}
func (s *recSpan) attr(k attribute.Key) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attrs[k]
}

func TestNonReplayableBodyIsNotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := newClient(t, srv)
	// No GetBody: the body can only be sent once.
	req := must(http.NewRequest("PUT", "/", io.NopCloser(strings.NewReader("{}"))))
	res, err := c.Do(req)
	if err != nil || res.StatusCode != 503 || hits.Load() != 1 {
		t.Fatalf("a body that can't be replayed isn't retried: %v %v after %d", res, err, hits.Load())
	}
	res.Body.Close()
}

func TestRetriesAConnectionFailure(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		_, _ = io.WriteString(w, `{"id":"ok"}`)
	}))
	defer srv.Close()
	out, err := client.Send[Item](context.Background(), newClient(t, srv), "GET", "/", nil)
	if err != nil || out.ID != "ok" || hits.Load() != 2 {
		t.Fatalf("a dropped connection is retried: %+v, %v after %d", out, err, hits.Load())
	}
}

func TestHalfOpenLetsOneProbeThrough(t *testing.T) {
	var hits atomic.Int32
	arrived, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(500)
			return
		}
		arrived <- struct{}{}
		<-release
	}))
	defer srv.Close()
	clk := clock.NewFake(time.Unix(0, 0))
	c := newClient(t, srv, client.Clock(clk), client.Configure(func(c *client.Config) {
		c.Retries, c.BreakerFailures, c.BreakerOpenFor = 0, 1, time.Minute
	}))
	_, _ = client.Send[Item](context.Background(), c, "GET", "/", nil)
	clk.Advance(time.Minute)
	probe := make(chan error, 1)
	go func() {
		_, err := client.Send[Item](context.Background(), c, "GET", "/", nil)
		probe <- err
	}()
	<-arrived
	if _, err := client.Send[Item](context.Background(), c, "GET", "/", nil); errs.From(err).Code != "galleon.circuit_open" {
		t.Fatalf("while the probe is out, other requests fail fast: %v", err)
	}
	close(release)
	if err := <-probe; err != nil || hits.Load() != 2 {
		t.Fatalf("the probe goes through: %v after %d", err, hits.Load())
	}
}

func TestEgressGuardAppliesToRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()
	_, err := client.Send[Item](context.Background(), newClient(t, srv), "GET", "/", nil)
	if e := errs.From(err); e.Kind != errs.PermissionDenied || e.Code != "galleon.egress_denied" {
		t.Fatalf("a redirect to a refused address is refused: %v", err)
	}
}

func TestRedirectsResolveAgainstTheRequestURL(t *testing.T) {
	var seen sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/abs":
			http.Redirect(w, r, "/login", http.StatusFound)
		case "/api/a/rel":
			http.Redirect(w, r, "../x", http.StatusFound)
		case "/api/a/sib":
			http.Redirect(w, r, "y", http.StatusFound)
		default:
			seen.Store(r.URL.Path, true)
		}
	}))
	defer srv.Close()
	c, err := client.New("test", client.BaseURL(srv.URL+"/api"), loopback)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"/abs": "/login", "/a/rel": "/api/x", "a/sib": "/api/a/y"} {
		seen.Clear()
		if _, err := client.Send[Item](context.Background(), c, "GET", path, nil); err != nil {
			t.Fatal(err)
		}
		if _, ok := seen.Load(want); !ok {
			t.Errorf("Do: %s redirected, want %s; server saw %v", path, want, keys(&seen))
		}
		seen.Clear()
		req := must(http.NewRequest("GET", path, nil))
		res, err := c.HTTP().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if _, ok := seen.Load(want); !ok {
			t.Errorf("HTTP(): %s redirected, want %s; server saw %v", path, want, keys(&seen))
		}
		if req.URL.String() != path {
			t.Errorf("the caller's request was changed to %s", req.URL)
		}
	}
}

func keys(m *sync.Map) []any {
	var ks []any
	m.Range(func(k, _ any) bool { ks = append(ks, k); return true })
	return ks
}

func TestCredentialsAreBoundToSchemeAndPort(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	fake := rtFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		got[r.URL.String()] = r.Header.Get("Authorization")
		mu.Unlock()
		res := &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, Request: r}
		if r.URL.Path == "/downgrade" {
			res.StatusCode = http.StatusFound
			res.Header.Set("Location", "http://api.example.com/landing")
		}
		return res, nil
	})
	c, err := client.New("test", client.BaseURL("https://api.example.com"), client.Transport(fake),
		client.WithCredentials(client.Bearer("base")),
		client.WithCredentials(client.Bearer("files"), "files.example.com"),
		client.WithCredentials(client.Bearer("legacy"), "http://legacy.example.com:8080"),
		client.WithCredentials(client.Bearer("v6"), "2001:db8::10"))
	if err != nil {
		t.Fatal(err)
	}
	for u, want := range map[string]string{
		"https://api.example.com/downgrade": "Bearer base",
		"https://api.example.com:443/x":     "Bearer base",
		"https://api.example.com:8443/x":    "",
		"http://api.example.com/x":          "",
		"https://files.example.com/x":       "Bearer files",
		"https://files.example.com:8443/x":  "Bearer files",
		"http://files.example.com/x":        "",
		"http://legacy.example.com:8080/x":  "Bearer legacy",
		"http://legacy.example.com:8081/x":  "",
		"https://legacy.example.com:8080/x": "",
		"https://[2001:db8::10]/x":          "Bearer v6",
	} {
		res, err := c.Do(must(http.NewRequest("GET", u, nil)))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		mu.Lock()
		if got[u] != want {
			t.Errorf("%s: got %q, want %q", u, got[u], want)
		}
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	if v, ok := got["http://api.example.com/landing"]; !ok || v != "" {
		t.Errorf("a redirect from https to http on the same host must not carry the credentials: %q (followed: %v)", v, ok)
	}
	for _, h := range []string{"ftp://x", "https://x/path", "https://u@x"} {
		if _, err := client.New("test", client.Transport(fake), client.WithCredentials(client.Bearer("x"), h)); err == nil {
			t.Errorf("credentials host %q must be refused", h)
		}
	}
}

func TestMaxResponseBytesMustBePositive(t *testing.T) {
	_, err := client.New("test", client.Configure(func(c *client.Config) { c.MaxResponseBytes = 0 }))
	if err == nil || !strings.Contains(err.Error(), "max_response_bytes") {
		t.Fatalf("got %v", err)
	}
}

func TestRedirectChainsThroughHTTPKeepHeadersOnlyOnTheSameDomain(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	next := map[string]string{
		"/api/evil1": "/api/evil2", "/api/evil2": "https://evil.com./steal",
		"/api/same1": "/api/same2", "/api/same2": "/api/same3",
		"/api/away1": "https://other.example.org/x", "/x": "https://api.example.com/api/back",
	}
	fake := rtFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		got[r.URL.Host+r.URL.Path] = r.Header.Get("Authorization") + "|" + r.Header.Get("Cookie")
		mu.Unlock()
		res := &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, Request: r}
		if to, ok := next[r.URL.Path]; ok {
			res.StatusCode = http.StatusFound
			res.Header.Set("Location", to)
		}
		return res, nil
	})
	c, err := client.New("test", client.BaseURL("https://api.example.com/api"), client.Transport(fake))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/evil1", "/same1", "/away1"} {
		req := must(http.NewRequest("GET", path, nil))
		req.Header.Set("Authorization", "Bearer caller")
		req.Header.Set("Cookie", "s=1")
		res, err := c.HTTP().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	for at, want := range map[string]string{
		"evil.com./steal":           "|",
		"api.example.com/api/same3": "Bearer caller|s=1",
		"other.example.org/x":       "|",
		"api.example.com/api/back":  "|", // once stripped, stays stripped
	} {
		if v, ok := got[at]; !ok || v != want {
			t.Errorf("%s: got %q (reached: %v), want %q", at, v, ok, want)
		}
	}
}
