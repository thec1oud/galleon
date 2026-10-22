// Package jwt authenticates bearer tokens: JSON Web Tokens signed by an
// OpenID Connect provider or any issuer that publishes a JWK set.
//
//	security.Module(
//		jwt.Bearer(), // reads the "jwt" config section
//		security.Decide(roles),
//	)
//
//	{"jwt": {"issuer": "https://id.example.com", "audience": ["api"]}}
//
// Its principals carry the token's claims, with numbers as json.Number.
// A token is accepted only if its signature verifies with a key of the
// issuer's, under an allowed asymmetric algorithm that suits the key, and
// its iss, aud, exp and nbf claims hold, with a bounded allowance for clock
// skew. The issuer's keys are fetched at start, found through OpenID
// Connect discovery unless jwks_url names them, and refreshed in the
// background; a token signed with a key not yet seen triggers at most one
// refresh a minute, so the issuer can rotate keys without failing a burst of
// requests.
package jwt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/security"
)

// Config is the "jwt" config section, which Bearer reads.
type Config struct {
	// Issuer is the token issuer: a token's iss claim must equal it. Unless
	// JWKSURL or JWKS is set, the issuer's keys are found through OpenID
	// Connect discovery, at Issuer + "/.well-known/openid-configuration".
	Issuer string `json:"issuer"`
	// Audience lists the audiences this service answers to: a token's aud
	// claim must name one of them.
	Audience []string `json:"audience"`
	// JWKSURL is where the issuer publishes its JWK set, if not found
	// through discovery. It must be https, except on a loopback host.
	JWKSURL string `json:"jwks_url"`
	// JWKS is a JWK set, as JSON, used instead of fetching one, for tests
	// and issuers whose keys never change.
	JWKS string `json:"jwks"`
	// Algorithms lists the signature algorithms accepted. The default is
	// every one supported: RS256, RS384, RS512, PS256, PS384, PS512, ES256,
	// ES384, ES512 and EdDSA.
	Algorithms []string `json:"algorithms"`
	// Leeway is the allowance for clock skew when checking exp and nbf. It
	// may be at most 5 minutes.
	Leeway time.Duration `json:"leeway"`
	// RefreshInterval is how often the issuer's keys are fetched again. It
	// must be at least a minute.
	RefreshInterval time.Duration `json:"refresh_interval"`
}

// Default sets a minute of leeway and an hourly refresh.
func (c *Config) Default() {
	c.Leeway = time.Minute
	c.RefreshInterval = time.Hour
}

// Validate checks the section.
func (c *Config) Validate() error {
	var problems []error
	if c.Issuer == "" {
		problems = append(problems, errors.New("issuer must be set"))
	}
	if len(c.Audience) == 0 || slices.Contains(c.Audience, "") {
		problems = append(problems, errors.New("audience must name at least one audience, and none empty"))
	}
	for _, a := range c.Algorithms {
		if _, ok := algorithms[a]; !ok {
			problems = append(problems, fmt.Errorf("algorithms: %q isn't supported (HMAC and none never are)", a))
		}
	}
	if c.Leeway < 0 || c.Leeway > 5*time.Minute {
		problems = append(problems, errors.New("leeway must be between 0 and 5m"))
	}
	if c.RefreshInterval < time.Minute {
		problems = append(problems, errors.New("refresh_interval must be at least 1m"))
	}
	switch {
	case c.JWKS != "" && c.JWKSURL != "":
		problems = append(problems, errors.New("jwks and jwks_url can't both be set"))
	case c.JWKS != "":
		if _, err := parseKeySet([]byte(c.JWKS)); err != nil {
			problems = append(problems, fmt.Errorf("jwks: %w", err))
		}
	case c.JWKSURL != "":
		if err := checkURL(c.JWKSURL); err != nil {
			problems = append(problems, fmt.Errorf("jwks_url: %w", err))
		}
	default:
		if err := checkURL(c.Issuer); c.Issuer != "" && err != nil {
			problems = append(problems, fmt.Errorf("issuer: %w, so its keys can't be discovered; set jwks_url or jwks", err))
		}
	}
	return errors.Join(problems...)
}

// checkURL requires an https URL, or http on a loopback host.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch {
	case u.Scheme == "https" && u.Host != "":
		return nil
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		return nil
	}
	return fmt.Errorf("%q must be an https URL (http only on a loopback host)", raw)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Option configures an Authenticator.
type Option func(*Authenticator)

// WithClock sets the clock tokens' times are checked against. Bearer uses
// the app's.
func WithClock(c clock.Clock) Option { return func(a *Authenticator) { a.clock = c } }

// WithLogger sets the logger key refreshes report to. Bearer uses the app's.
func WithLogger(l *slog.Logger) Option { return func(a *Authenticator) { a.log = l } }

// WithHTTPClient sets the client that fetches the issuer's keys.
func WithHTTPClient(c *http.Client) Option { return func(a *Authenticator) { a.http = c } }

// WithScheme sets the scheme name, "bearer" by default, for an app with
// more than one issuer.
func WithScheme(s string) Option { return func(a *Authenticator) { a.scheme = s } }

// Bearer returns a security module option that authenticates bearer tokens
// as the "jwt" config section describes, under the scheme "bearer". The
// node isn't ready until the issuer's keys are loaded.
func Bearer(opts ...Option) security.Option {
	return func(s *security.Setup) {
		r := s.Registry()
		cfg := galleon.Config[Config](r, "jwt")
		var a *Authenticator
		r.Gate("jwt keys", func(context.Context) error {
			if a == nil {
				return errors.New("jwt: not started")
			}
			return a.Ready()
		})
		s.Apply(security.AuthenticateWith(schemeOf(opts), func(c clock.Clock, log *slog.Logger) (security.Authenticator, error) {
			var err error
			a, err = New(*cfg, append([]Option{WithClock(c), WithLogger(log)}, opts...)...)
			return a, err
		}))
	}
}

func schemeOf(opts []Option) string {
	a := &Authenticator{scheme: "bearer"}
	for _, o := range opts {
		o(a)
	}
	return a.scheme
}

// Authenticator verifies bearer tokens. Start loads the issuer's keys and
// keeps them fresh; the security module starts it.
type Authenticator struct {
	cfg    Config
	scheme string
	algs   []string
	clock  clock.Clock
	log    *slog.Logger
	http   *http.Client

	mu           sync.RWMutex
	keys         []key
	loaded       bool
	fetchMu      sync.Mutex // one fetch at a time
	lastOnDemand time.Time  // of a fetch for an unknown key, by the monotonic clock
	jwksURL      string     // found by discovery, if not configured

	stop chan struct{}
	done chan struct{}
}

// refreshCooldown bounds how often a token signed with an unknown key can
// trigger a fetch.
const refreshCooldown = time.Minute

// maxTokenSize bounds a token's length, and maxDocSize a fetched document's.
const (
	maxTokenSize = 16 << 10
	maxDocSize   = 1 << 20
)

// New returns an Authenticator for cfg, which must be valid. A cfg whose
// Leeway and RefreshInterval are both zero gets Default's.
func New(cfg Config, opts ...Option) (*Authenticator, error) {
	if cfg.Leeway == 0 && cfg.RefreshInterval == 0 {
		var d Config
		d.Default()
		cfg.Leeway, cfg.RefreshInterval = d.Leeway, d.RefreshInterval
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("jwt: %w", err)
	}
	a := &Authenticator{
		cfg:    cfg,
		scheme: "bearer",
		algs:   cfg.Algorithms,
		clock:  clock.System(),
		log:    slog.New(slog.DiscardHandler),
		http:   &http.Client{Timeout: 10 * time.Second},
	}
	if len(a.algs) == 0 {
		for alg := range algorithms {
			a.algs = append(a.algs, alg)
		}
		slices.Sort(a.algs)
	}
	for _, o := range opts {
		o(a)
	}
	if cfg.JWKS != "" {
		keys, err := parseKeySet([]byte(cfg.JWKS))
		if err != nil {
			return nil, fmt.Errorf("jwt: jwks: %w", err)
		}
		a.keys, a.loaded = keys, true
	}
	return a, nil
}

func (a *Authenticator) Scheme() string    { return a.scheme }
func (a *Authenticator) Challenge() string { return "Bearer" }

// Ready reports whether the issuer's keys are loaded.
func (a *Authenticator) Ready() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.loaded {
		return errors.New("jwt: the issuer's keys aren't loaded yet")
	}
	return nil
}

// Start fetches the issuer's keys, unless they were given, and refreshes
// them in the background. A failed first fetch is logged and retried, and
// the node stays unready, rather than failing start.
func (a *Authenticator) Start(ctx context.Context) error {
	if a.cfg.JWKS != "" || a.stop != nil {
		return nil
	}
	a.stop, a.done = make(chan struct{}), make(chan struct{})
	go a.refreshLoop()
	return nil
}

// Stop ends the background refresh.
func (a *Authenticator) Stop(ctx context.Context) error {
	if a.stop == nil {
		return nil
	}
	close(a.stop)
	select {
	case <-a.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Authenticator) refreshLoop() {
	defer close(a.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-a.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	retry := time.Second
	for {
		wait := a.cfg.RefreshInterval
		if err := a.refresh(ctx, false); err != nil {
			if ctx.Err() != nil {
				return
			}
			a.log.Warn("jwt: fetching the issuer's keys failed; retrying", "issuer", a.cfg.Issuer, "error", err)
			if a.Ready() != nil {
				wait, retry = retry, min(retry*2, time.Minute)
			}
		} else {
			retry = time.Second
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-a.stop:
			t.Stop()
			return
		}
	}
}

// refresh fetches the key set. A fetch on demand, for a token signed with
// an unknown key, is skipped if another was made less than the cooldown
// ago; scheduled ones always run. The old keys stay until a fetch succeeds.
func (a *Authenticator) refresh(ctx context.Context, onDemand bool) error {
	a.fetchMu.Lock()
	defer a.fetchMu.Unlock()
	if onDemand {
		if !a.lastOnDemand.IsZero() && time.Since(a.lastOnDemand) < refreshCooldown {
			return nil
		}
		a.lastOnDemand = time.Now()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := a.cfg.JWKSURL
	if u == "" {
		u = a.jwksURL
	}
	if u == "" {
		var err error
		if u, err = a.discover(ctx); err != nil {
			return err
		}
		a.jwksURL = u
	}
	body, err := a.get(ctx, u)
	if err != nil {
		return err
	}
	keys, err := parseKeySet(body)
	if err != nil {
		return fmt.Errorf("%s: %w", u, err)
	}
	a.mu.Lock()
	a.keys, a.loaded = keys, true
	a.mu.Unlock()
	return nil
}

// discover reads the issuer's OpenID Connect discovery document for its
// JWK set's URL.
func (a *Authenticator) discover(ctx context.Context) (string, error) {
	u := strings.TrimSuffix(a.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	body, err := a.get(ctx, u)
	if err != nil {
		return "", err
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("%s: %w", u, err)
	}
	if doc.Issuer != a.cfg.Issuer {
		return "", fmt.Errorf("%s names the issuer %q, not %q", u, doc.Issuer, a.cfg.Issuer)
	}
	if err := checkURL(doc.JWKSURI); err != nil {
		return "", fmt.Errorf("%s: jwks_uri: %w", u, err)
	}
	return doc.JWKSURI, nil
}

func (a *Authenticator) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d", u, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxDocSize+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", u, err)
	}
	if len(body) > maxDocSize {
		return nil, fmt.Errorf("%s: the document is over %d bytes", u, maxDocSize)
	}
	return body, nil
}

// Authenticate verifies the request's bearer token. A request without an
// Authorization header, or with one of another scheme, has no credentials
// for it.
func (a *Authenticator) Authenticate(ctx context.Context, r *op.Request) (security.Principal, error) {
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		return nil, security.ErrNoCredentials
	}
	scheme, token, _ := strings.Cut(values[0], " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return nil, security.ErrNoCredentials
	}
	if len(values) > 1 {
		return nil, security.InvalidCredentials("the request has more than one Authorization header", nil)
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxTokenSize {
		return nil, security.InvalidCredentials("the bearer token is malformed", nil)
	}
	return a.Verify(ctx, token)
}

// Verify checks a token and returns its principal. It returns an
// Unauthenticated error, as security.InvalidCredentials does, for a token
// that doesn't verify, and an Unavailable one when the issuer's keys
// haven't been loaded.
func (a *Authenticator) Verify(ctx context.Context, token string) (security.Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, security.InvalidCredentials("the bearer token is malformed", errors.New("not a compact JWS"))
	}
	var header struct {
		Alg  string          `json:"alg"`
		Kid  string          `json:"kid"`
		Crit json.RawMessage `json:"crit"`
	}
	if err := decodePart(parts[0], &header); err != nil {
		return nil, security.InvalidCredentials("the bearer token is malformed", fmt.Errorf("header: %w", err))
	}
	if header.Crit != nil {
		return nil, security.InvalidCredentials("the bearer token is not accepted", errors.New("it has critical header parameters"))
	}
	if !slices.Contains(a.algs, header.Alg) {
		return nil, security.InvalidCredentials("the bearer token is not accepted", fmt.Errorf("the algorithm %q isn't allowed", header.Alg))
	}
	sig, err := b64(parts[2])
	if err != nil {
		return nil, security.InvalidCredentials("the bearer token is malformed", fmt.Errorf("signature: %w", err))
	}
	input := []byte(parts[0] + "." + parts[1])
	if err := a.checkSignature(ctx, header.Alg, header.Kid, input, sig); err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := decodePart(parts[1], &claims); err != nil || claims == nil {
		return nil, security.InvalidCredentials("the bearer token is malformed", fmt.Errorf("claims: %v", err))
	}
	return a.principal(claims)
}

// checkSignature verifies sig with the issuer's keys that could have made
// it, fetching the keys again, within the cooldown, when none could.
func (a *Authenticator) checkSignature(ctx context.Context, alg, kid string, input, sig []byte) error {
	for attempt := 0; ; attempt++ {
		a.mu.RLock()
		loaded := a.loaded
		var candidates []key
		for _, k := range a.keys {
			if (kid == "" || k.kid == kid) && (k.alg == "" || k.alg == alg) && compatible(alg, k.pub) {
				candidates = append(candidates, k)
			}
		}
		a.mu.RUnlock()
		for _, k := range candidates {
			if verifySignature(alg, k.pub, input, sig) == nil {
				return nil
			}
		}
		if len(candidates) > 0 {
			return security.InvalidCredentials("the bearer token's signature doesn't verify", errSignature)
		}
		if attempt > 0 || a.cfg.JWKS != "" {
			if !loaded {
				return errs.New(errs.Unavailable, security.CodeUnavailable, "the token issuer's keys aren't available")
			}
			return security.InvalidCredentials("the bearer token is signed with an unknown key", fmt.Errorf("no key with kid %q for %s", kid, alg))
		}
		if err := a.refresh(ctx, true); err != nil {
			a.log.WarnContext(ctx, "jwt: fetching the issuer's keys for an unknown key failed", "issuer", a.cfg.Issuer, "kid", kid, "error", err)
		}
	}
}

// principal checks the claims and makes the principal.
func (a *Authenticator) principal(claims map[string]any) (security.Principal, error) {
	invalid := func(why string) error {
		return security.InvalidCredentials("the bearer token is not accepted", errors.New(why))
	}
	if iss, _ := claims["iss"].(string); iss != a.cfg.Issuer {
		return nil, invalid(fmt.Sprintf("it was issued by %q", iss))
	}
	if !a.audienceMatches(claims["aud"]) {
		return nil, invalid("it isn't for this audience")
	}
	now := a.clock.Now()
	exp, ok := numericDate(claims["exp"])
	switch {
	case !ok:
		return nil, invalid("it has no valid exp")
	case !now.Before(exp.Add(a.cfg.Leeway)):
		return nil, security.InvalidCredentials("the bearer token has expired", errors.New("expired"))
	}
	if v, has := claims["nbf"]; has {
		nbf, ok := numericDate(v)
		if !ok {
			return nil, invalid("its nbf is invalid")
		}
		if now.Add(a.cfg.Leeway).Before(nbf) {
			return nil, security.InvalidCredentials("the bearer token isn't valid yet", errors.New("not yet valid"))
		}
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil, invalid("it has no sub")
	}
	kind := security.KindUser
	if cid, _ := claims["client_id"].(string); cid != "" && cid == sub {
		kind = security.KindService // a client acting as itself (RFC 9068 §2.2)
	}
	var as security.Assurance
	if t, ok := numericDate(claims["auth_time"]); ok {
		as.AuthTime = t
	}
	as.Methods, _ = security.NewValues(map[string]any{"amr": claims["amr"]}).Strings("amr")
	return security.Identity{Subject: sub, Kind: kind, Issuer: a.cfg.Issuer, Claims: security.NewValues(claims), Assurance: as}.Principal(), nil
}

func (a *Authenticator) audienceMatches(v any) bool {
	var auds []string
	switch x := v.(type) {
	case string:
		auds = []string{x}
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				auds = append(auds, s)
			}
		}
	}
	for _, aud := range auds {
		if slices.Contains(a.cfg.Audience, aud) {
			return true
		}
	}
	return false
}

// numericDate reads a JWT NumericDate: seconds since the epoch, possibly
// fractional.
func numericDate(v any) (time.Time, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || f < 0 || f > 1<<40 {
		return time.Time{}, false
	}
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9)), true
}

// decodePart decodes a base64url JSON part of a token, keeping numbers as
// json.Number and refusing trailing data.
func decodePart(s string, v any) error {
	b, err := b64(s)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}
