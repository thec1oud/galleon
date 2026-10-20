// Package client is Galleon's outbound HTTP client. A Client is a plain
// *http.Client underneath, so it works with anything that takes one,
// connect-go clients included, and every request through it gets the same
// layers, outermost first:
//
//	middleware        the module's own, shaped like the server's
//	credentials       only for the hosts they're configured for
//	concurrency limit MaxConcurrent requests in flight
//	retries           idempotent requests, with backoff, Retry-After and a budget
//	circuit breaker   per host
//	attempt timeout   per attempt; the body stays readable until closed
//	tracing           a client span per attempt, with the trace context injected
//	egress guard      refuses private and internal addresses at dial time
//
// The attempt timeout runs until the response body is closed, so a body
// streamed for longer than Timeout needs Timeout 0 or a longer one. The egress
// guard checks the address it dials, which with Proxy set is the proxy's.
//
//	c, err := clients.New("billing", client.BaseURL("https://billing.example.com"),
//		client.WithCredentials(client.Bearer(token)))
//	out, err := client.Send[Invoice](ctx, c, "GET", "/invoices/42", nil)
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/clock"
)

// Config is the "client" config section: the defaults for every client.
type Config struct {
	// Timeout bounds each attempt, from sending the request until the
	// response body is closed, so it covers reading the whole body: a
	// streamed response needs zero, which turns it off, or a longer one.
	Timeout time.Duration `json:"timeout"`
	// Retries is how many times a failed idempotent request is retried.
	Retries int `json:"retries"`
	// RetryBackoff is the first retry's backoff ceiling; each later retry
	// doubles it, up to RetryBackoffMax. The wait is a random fraction of it.
	RetryBackoff    time.Duration `json:"retry_backoff"`
	RetryBackoffMax time.Duration `json:"retry_backoff_max"`
	// MaxRetryAfter is the longest Retry-After a retry waits for; a longer
	// one ends the retries and returns the response.
	MaxRetryAfter time.Duration `json:"max_retry_after"`
	// RetryBudget is the share of requests that may be retries, so retries
	// can't multiply load on a struggling server. Each request earns this
	// many retry tokens, up to RetryBudgetBurst, and each retry spends one.
	RetryBudget      float64 `json:"retry_budget"`
	RetryBudgetBurst float64 `json:"retry_budget_burst"`
	// BreakerFailures consecutive failures to a host open its breaker for
	// BreakerOpenFor. Zero failures turns the breaker off.
	BreakerFailures int           `json:"breaker_failures"`
	BreakerOpenFor  time.Duration `json:"breaker_open_for"`
	// MaxConcurrent bounds requests in flight; zero is unbounded.
	MaxConcurrent int `json:"max_concurrent"`
	// AllowNetworks are CIDRs the egress guard lets through, such as an
	// internal service's range. AllowPrivate turns the guard off.
	AllowNetworks []string `json:"allow_networks"`
	AllowPrivate  bool     `json:"allow_private"`
	// MaxResponseBytes bounds a response body Send decodes.
	MaxResponseBytes int64 `json:"max_response_bytes"`
}

// Default sets the defaults: a 10s attempt timeout, two retries within a 20%
// budget, a breaker that opens for 30s after 5 failures, and the egress guard.
func (c *Config) Default() {
	*c = Config{
		Timeout:          10 * time.Second,
		Retries:          2,
		RetryBackoff:     100 * time.Millisecond,
		RetryBackoffMax:  2 * time.Second,
		MaxRetryAfter:    10 * time.Second,
		RetryBudget:      0.2,
		RetryBudgetBurst: 10,
		BreakerFailures:  5,
		BreakerOpenFor:   30 * time.Second,
		MaxResponseBytes: 10 << 20,
	}
}

// Validate checks the section.
func (c *Config) Validate() error {
	var errs []error
	if c.Timeout < 0 || c.RetryBackoff < 0 || c.RetryBackoffMax < c.RetryBackoff || c.MaxRetryAfter < 0 || c.BreakerOpenFor < 0 {
		errs = append(errs, errors.New("durations must not be negative, and retry_backoff_max must be at least retry_backoff"))
	}
	if c.Retries < 0 || c.BreakerFailures < 0 || c.MaxConcurrent < 0 {
		errs = append(errs, errors.New("retries, breaker_failures and max_concurrent must not be negative"))
	}
	if c.MaxResponseBytes <= 0 {
		errs = append(errs, errors.New("max_response_bytes must be positive"))
	}
	if c.RetryBudget < 0 || c.RetryBudgetBurst < 0 {
		errs = append(errs, errors.New("retry_budget and retry_budget_burst must not be negative"))
	}
	if c.BreakerFailures > 0 && c.BreakerOpenFor <= 0 {
		errs = append(errs, errors.New("breaker_open_for must be positive when the breaker is on"))
	}
	for _, n := range c.AllowNetworks {
		if _, err := netip.ParsePrefix(n); err != nil {
			errs = append(errs, fmt.Errorf("allow_networks: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Middleware wraps a client's transport, as server middleware wraps a handler.
type Middleware func(http.RoundTripper) http.RoundTripper

// Option configures a client.
type Option func(*settings)

type settings struct {
	cfg        Config
	base       *url.URL
	transport  http.RoundTripper
	middleware []Middleware
	creds      []credentials
	proxy      func(*http.Request) (*url.URL, error)
	tp         trace.TracerProvider
	log        *slog.Logger
	clock      clock.Clock
	err        error
}

// BaseURL resolves relative request URLs sent with Do against u, and is where credentials
// go when no host is named for them.
func BaseURL(u string) Option {
	return func(s *settings) {
		p, err := url.Parse(u)
		if err != nil || p.Scheme == "" || p.Host == "" {
			s.err = errors.Join(s.err, fmt.Errorf("client: base URL %q must be absolute", u))
			return
		}
		s.base = p
	}
}

// Transport replaces the network transport, as a test does with a fake. The
// egress guard is part of the default transport, so it doesn't apply to one
// given here.
func Transport(rt http.RoundTripper) Option { return func(s *settings) { s.transport = rt } }

// Use adds middleware, outermost first.
func Use(mw ...Middleware) Option {
	return func(s *settings) { s.middleware = append(s.middleware, mw...) }
}

// Proxy sends requests through a proxy. Clients ignore HTTP_PROXY and its
// relatives, since the egress guard can only check the address it dials: with
// a proxy that is the proxy's, not the target's, so the proxy must do its own
// checks.
func Proxy(fn func(*http.Request) (*url.URL, error)) Option {
	return func(s *settings) { s.proxy = fn }
}

// Configure changes this client's copy of the config section.
func Configure(fn func(*Config)) Option { return func(s *settings) { fn(&s.cfg) } }

// TracerProvider, Logger and Clock replace the defaults (no tracing, the
// default logger, the system clock); the module sets them from the app.
func TracerProvider(tp trace.TracerProvider) Option { return func(s *settings) { s.tp = tp } }
func Logger(l *slog.Logger) Option                  { return func(s *settings) { s.log = l } }
func Clock(c clock.Clock) Option                    { return func(s *settings) { s.clock = c } }

// Client is an outbound HTTP client with Galleon's layers.
type Client struct {
	name string
	cfg  Config
	base *url.URL
	hc   *http.Client
}

// New builds a client. name labels its spans and logs.
func New(name string, opts ...Option) (*Client, error) {
	s := settings{}
	s.cfg.Default()
	for _, o := range opts {
		o(&s)
	}
	if s.err != nil {
		return nil, s.err
	}
	if err := s.cfg.Validate(); err != nil {
		return nil, fmt.Errorf("client %q: %w", name, err)
	}
	if s.tp == nil {
		s.tp = noop.NewTracerProvider()
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.clock == nil {
		s.clock = clock.System()
	}
	for i := range s.creds {
		cr := &s.creds[i]
		if len(cr.hosts) == 0 {
			if s.base == nil {
				return nil, fmt.Errorf("client %q: credentials need a BaseURL or the hosts they're for", name)
			}
			cr.origins = []origin{baseOrigin(s.base)}
		}
		for _, h := range cr.hosts {
			o, err := parseOrigin(h)
			if err != nil {
				return nil, fmt.Errorf("client %q: %w", name, err)
			}
			cr.origins = append(cr.origins, o)
		}
	}

	rt := s.transport
	if rt == nil {
		t, err := defaultTransport(&s)
		if err != nil {
			return nil, fmt.Errorf("client %q: %w", name, err)
		}
		rt = t
	}
	rt = &tracing{next: rt, tracer: s.tp.Tracer("github.com/thec1oud/galleon/client"), name: name}
	rt = &attemptTimeout{next: rt, d: s.cfg.Timeout}
	if s.cfg.BreakerFailures > 0 {
		rt = &breakers{next: rt, failures: s.cfg.BreakerFailures, openFor: s.cfg.BreakerOpenFor, clock: s.clock, hosts: map[string]*breaker{}}
	}
	if s.cfg.Retries > 0 {
		rt = &retrying{next: rt, cfg: s.cfg, clock: s.clock, budget: newBudget(s.cfg.RetryBudget, s.cfg.RetryBudgetBurst)}
	}
	if s.cfg.MaxConcurrent > 0 {
		rt = &limit{next: rt, slots: make(chan struct{}, s.cfg.MaxConcurrent)}
	}
	if len(s.creds) > 0 {
		rt = &credentialed{next: rt, creds: s.creds}
	}
	for i := len(s.middleware) - 1; i >= 0; i-- {
		rt = s.middleware[i](rt)
	}
	rt = &absolute{next: rt}
	c := &Client{name: name, cfg: s.cfg, base: s.base}
	c.hc = &http.Client{Transport: rt}
	return c, nil
}

// HTTP returns the client as an *http.Client, for connect-go clients and
// other libraries. Its requests need absolute URLs; only Do resolves relative
// ones. Its Timeout is unset: attempts have their own, and the
// caller's context bounds the whole call.
func (c *Client) HTTP() *http.Client { return c.hc }

// Do sends req through the client's layers. A relative URL is resolved
// before the request is sent, so redirects resolve against where it went.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req.URL != nil && !req.URL.IsAbs() {
		var err error
		switch {
		case req.URL.Host != "" || req.URL.User != nil:
			// "//host/path" names another host, which isn't relative to
			// the base in any useful sense.
			err = fmt.Errorf("client: %q has a host but no scheme", req.URL.Redacted())
		case c.base == nil:
			err = fmt.Errorf("client: %q is relative and the client has no BaseURL", req.URL.Redacted())
		}
		if err != nil {
			closeBody(req)
			return nil, err
		}
		req = req.Clone(req.Context())
		req.URL = resolve(c.base, req.URL)
		req.Host = ""
	}
	return c.hc.Do(req)
}

// Name is the name the client was built with.
func (c *Client) Name() string { return c.name }

// absolute refuses relative URLs, which only Do resolves. http.Client
// resolves a redirect's Location against the request's URL and decides which
// headers follow it by that URL's host, so it must see where the request went.
type absolute struct {
	next http.RoundTripper
}

func (a *absolute) RoundTrip(req *http.Request) (*http.Response, error) {
	if !req.URL.IsAbs() {
		closeBody(req)
		return nil, fmt.Errorf("client: %q is not absolute; HTTP() needs absolute URLs, and only Client.Do resolves relative paths against a BaseURL", req.URL.Redacted())
	}
	return a.next.RoundTrip(req)
}

// resolve resolves a relative URL under the base URL's path: with base
// https://h/api, both "/items" and "items" are https://h/api/items.
func resolve(base, u *url.URL) *url.URL {
	ref := *u
	ref.Path = strings.TrimPrefix(ref.Path, "/")
	ref.RawPath = strings.TrimPrefix(ref.RawPath, "/")
	b := *base
	if !strings.HasSuffix(b.Path, "/") {
		b.Path += "/"
		if b.RawPath != "" {
			b.RawPath += "/"
		}
	}
	return b.ResolveReference(&ref)
}

// limit bounds requests in flight.
type limit struct {
	next  http.RoundTripper
	slots chan struct{}
}

func (l *limit) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case l.slots <- struct{}{}:
	case <-req.Context().Done():
		closeBody(req)
		return nil, req.Context().Err()
	}
	res, err := l.next.RoundTrip(req)
	if err != nil {
		<-l.slots
		return nil, err
	}
	res.Body = &onClose{ReadCloser: res.Body, fn: func() { <-l.slots }}
	return res, nil
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

func defaultTransport(s *settings) (*http.Transport, error) {
	g, err := newGuard(s.cfg)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: g.control}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = s.proxy
	t.DialContext = d.DialContext
	return t, nil
}

// Clients builds clients from the "client" config section, with the app's
// tracer, logger and clock.
type Clients struct {
	cfg  *Config
	opts []Option
}

// New builds a client named name. Options given here apply after the
// section's settings.
func (cs *Clients) New(name string, opts ...Option) (*Client, error) {
	base := append([]Option{func(s *settings) {
		s.cfg = *cs.cfg
		s.cfg.AllowNetworks = slices.Clone(cs.cfg.AllowNetworks)
	}}, cs.opts...)
	return New(name, append(base, opts...)...)
}

// Module provides *Clients.
func Module() galleon.Module {
	return galleon.ModuleFunc("client", func(r *galleon.Registry) error {
		cfg := galleon.Config[Config](r, "client")
		galleon.Provide[*Clients](r, func(tp trace.TracerProvider, log *slog.Logger, clk clock.Clock) *Clients {
			return &Clients{cfg: cfg, opts: []Option{TracerProvider(tp), Logger(log), Clock(clk)}}
		})
		return nil
	})
}

// onClose runs fn once when the body is closed.
type onClose struct {
	io.ReadCloser
	fn   func()
	once sync.Once
}

func (b *onClose) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.fn)
	return err
}

// attemptTimeout bounds each attempt. The deadline covers reading the body,
// so it is only released when the body is closed.
type attemptTimeout struct {
	next http.RoundTripper
	d    time.Duration
}

func (a *attemptTimeout) RoundTrip(req *http.Request) (*http.Response, error) {
	if a.d <= 0 {
		return a.next.RoundTrip(req)
	}
	ctx, cancel := context.WithTimeout(req.Context(), a.d)
	res, err := a.next.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	res.Body = &onClose{ReadCloser: res.Body, fn: cancel}
	return res, nil
}
