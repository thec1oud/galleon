package jwt_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/security"
	"github.com/thec1oud/galleon/security/jwt"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// signer is a test issuer's key.
type signer struct {
	kid, alg string
	priv     crypto.Signer
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func pad(b []byte, n int) []byte { return append(make([]byte, n-len(b)), b...) }

func (s signer) jwk() map[string]any {
	switch k := s.priv.Public().(type) {
	case *rsa.PublicKey:
		return map[string]any{"kty": "RSA", "kid": s.kid, "use": "sig", "n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes())}
	case *ecdsa.PublicKey:
		size := (k.Curve.Params().BitSize + 7) / 8
		raw, _ := k.Bytes()
		return map[string]any{"kty": "EC", "kid": s.kid, "crv": k.Curve.Params().Name, "x": b64(raw[1 : 1+size]), "y": b64(raw[1+size:])}
	case ed25519.PublicKey:
		return map[string]any{"kty": "OKP", "kid": s.kid, "crv": "Ed25519", "x": b64(k)}
	}
	panic("unknown key")
}

func keySet(ss ...signer) string {
	var keys []map[string]any
	for _, s := range ss {
		keys = append(keys, s.jwk())
	}
	b, _ := json.Marshal(map[string]any{"keys": keys})
	return string(b)
}

func (s signer) sign(t *testing.T, header, claims map[string]any) string {
	t.Helper()
	h := map[string]any{"alg": s.alg, "kid": s.kid, "typ": "JWT"}
	for k, v := range header {
		h[k] = v
	}
	hb, _ := json.Marshal(h)
	cb, _ := json.Marshal(claims)
	input := b64(hb) + "." + b64(cb)
	var sig []byte
	var err error
	switch k := s.priv.(type) {
	case *rsa.PrivateKey:
		d := sha256.Sum256([]byte(input))
		if strings.HasPrefix(s.alg, "PS") {
			sig, err = rsa.SignPSS(rand.Reader, k, crypto.SHA256, d[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		} else {
			sig, err = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, d[:])
		}
	case *ecdsa.PrivateKey:
		d := sha256.Sum256([]byte(input))
		var r, ss *big.Int
		r, ss, err = ecdsa.Sign(rand.Reader, k, d[:])
		sig = append(pad(r.Bytes(), 32), pad(ss.Bytes(), 32)...)
	case ed25519.PrivateKey:
		sig = ed25519.Sign(k, []byte(input))
	}
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64(sig)
}

var (
	keysOnce        sync.Once
	rsaKey, rsaKey2 *rsa.PrivateKey
	ecKey           *ecdsa.PrivateKey
	edKey           ed25519.PrivateKey
)

func keys(t *testing.T) (rs, ps, es, ed, rotated signer) {
	keysOnce.Do(func() {
		rsaKey, _ = rsa.GenerateKey(rand.Reader, 2048)
		rsaKey2, _ = rsa.GenerateKey(rand.Reader, 2048)
		ecKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		_, edKey, _ = ed25519.GenerateKey(rand.Reader)
	})
	return signer{"rs", "RS256", rsaKey}, signer{"rs", "PS256", rsaKey}, signer{"es", "ES256", ecKey}, signer{"ed", "EdDSA", edKey}, signer{"rs2", "RS256", rsaKey2}
}

const issuer = "https://id.example.com"

func claims(extra map[string]any) map[string]any {
	c := map[string]any{"iss": issuer, "aud": "api", "sub": "u_1", "exp": now.Add(time.Hour).Unix(), "roles": []string{"editor"}}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func bearer(token string) *op.Request {
	return &op.Request{Transport: "http", Header: http.Header{"Authorization": {"Bearer " + token}}}
}

func newAuth(t *testing.T, cfg jwt.Config) *jwt.Authenticator {
	t.Helper()
	if cfg.Issuer == "" {
		cfg.Issuer = issuer
	}
	if cfg.Audience == nil {
		cfg.Audience = []string{"api"}
	}
	a, err := jwt.New(cfg, jwt.WithClock(clock.NewFake(now)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestValidTokens(t *testing.T) {
	rs, ps, es, ed, _ := keys(t)
	a := newAuth(t, jwt.Config{JWKS: keySet(rs, es, ed)})
	for _, s := range []signer{rs, ps, es, ed} {
		p, err := a.Authenticate(context.Background(), bearer(s.sign(t, nil, claims(map[string]any{"amr": []string{"pwd", "otp"}, "auth_time": now.Add(-time.Minute).Unix()}))))
		if err != nil {
			t.Fatalf("%s: %v", s.alg, err)
		}
		roles, _ := p.Claims().Strings("roles")
		if p.Subject() != "u_1" || p.Kind() != security.KindUser || p.Issuer() != issuer || len(roles) != 1 ||
			len(p.Assurance().Methods) != 2 || !p.Assurance().AuthTime.Equal(now.Add(-time.Minute)) {
			t.Fatalf("%s: principal %v %v %v %v %v", s.alg, p.Subject(), p.Kind(), p.Issuer(), roles, p.Assurance())
		}
	}
	// A client acting as itself is a service.
	p, err := a.Authenticate(context.Background(), bearer(rs.sign(t, nil, claims(map[string]any{"client_id": "u_1"}))))
	if err != nil || p.Kind() != security.KindService {
		t.Fatalf("client credentials: %v, %v", p, err)
	}
	// aud may be a list.
	if _, err := a.Authenticate(context.Background(), bearer(rs.sign(t, nil, claims(map[string]any{"aud": []string{"other", "api"}})))); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidTokens(t *testing.T) {
	rs, _, es, _, rotated := keys(t)
	a := newAuth(t, jwt.Config{JWKS: keySet(rs, es), Algorithms: []string{"RS256", "ES256"}})
	hs := func() string {
		hb, _ := json.Marshal(map[string]any{"alg": "HS256", "kid": "rs"})
		cb, _ := json.Marshal(claims(nil))
		input := b64(hb) + "." + b64(cb)
		// The classic confusion: the public key as an HMAC secret.
		m := hmac.New(sha256.New, []byte(keySet(rs)))
		m.Write([]byte(input))
		return input + "." + b64(m.Sum(nil))
	}
	none := func() string {
		hb, _ := json.Marshal(map[string]any{"alg": "none"})
		cb, _ := json.Marshal(claims(nil))
		return b64(hb) + "." + b64(cb) + "."
	}
	tamper := func() string {
		tok := rs.sign(t, nil, claims(nil))
		parts := strings.Split(tok, ".")
		cb, _ := json.Marshal(claims(map[string]any{"sub": "admin"}))
		return parts[0] + "." + b64(cb) + "." + parts[2]
	}
	cases := map[string]string{
		"expired":          rs.sign(t, nil, claims(map[string]any{"exp": now.Add(-2 * time.Minute).Unix()})),
		"no exp":           rs.sign(t, nil, claims(map[string]any{"exp": nil})),
		"exp a string":     rs.sign(t, nil, claims(map[string]any{"exp": "tomorrow"})),
		"not yet valid":    rs.sign(t, nil, claims(map[string]any{"nbf": now.Add(2 * time.Minute).Unix()})),
		"wrong issuer":     rs.sign(t, nil, claims(map[string]any{"iss": "https://evil.example.com"})),
		"wrong audience":   rs.sign(t, nil, claims(map[string]any{"aud": "other"})),
		"no audience":      rs.sign(t, nil, claims(map[string]any{"aud": nil})),
		"no subject":       rs.sign(t, nil, claims(map[string]any{"sub": nil})),
		"unknown key":      rotated.sign(t, nil, claims(nil)),
		"wrong kid":        signer{"es", "RS256", rsaKey}.sign(t, nil, claims(nil)),
		"alg not allowed":  signer{"rs", "PS256", rsaKey}.sign(t, nil, claims(nil)),
		"critical header":  rs.sign(t, map[string]any{"crit": []string{"exp"}}, claims(nil)),
		"hmac confusion":   hs(),
		"none":             none(),
		"tampered":         tamper(),
		"malformed":        "not.a.token",
		"two parts":        "abc.def",
		"five parts (JWE)": "a.b.c.d.e",
		"bad base64":       "a!.b.c",
		"trailing data":    b64([]byte(`{"alg":"RS256"} {}`)) + ".e30.c2ln",
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := a.Authenticate(context.Background(), bearer(tok))
			e := errs.From(err)
			if err == nil || e.Kind != errs.Unauthenticated || e.Code != security.CodeInvalidCredentials || errors.Is(err, security.ErrNoCredentials) {
				t.Fatalf("got %v", err)
			}
		})
	}
	// Leeway: a token a few seconds past exp still passes.
	if _, err := a.Authenticate(context.Background(), bearer(rs.sign(t, nil, claims(map[string]any{"exp": now.Add(-30 * time.Second).Unix()})))); err != nil {
		t.Fatalf("within leeway: %v", err)
	}
}

func TestOtherSchemesAreNoCredentials(t *testing.T) {
	rs, _, _, _, _ := keys(t)
	a := newAuth(t, jwt.Config{JWKS: keySet(rs)})
	for _, h := range []http.Header{nil, {}, {"Authorization": {"Basic dXNlcjpwYXNz"}}} {
		if _, err := a.Authenticate(context.Background(), &op.Request{Header: h}); !errors.Is(err, security.ErrNoCredentials) {
			t.Fatalf("%v: %v", h, err)
		}
	}
	two := http.Header{"Authorization": {"Bearer a.b.c", "Bearer d.e.f"}}
	if _, err := a.Authenticate(context.Background(), &op.Request{Header: two}); errs.From(err).Code != security.CodeInvalidCredentials {
		t.Fatalf("two headers: %v", err)
	}
}

func TestConfigValidates(t *testing.T) {
	cases := map[string]jwt.Config{
		"issuer must be set":              {Audience: []string{"api"}},
		"audience must name":              {Issuer: issuer},
		`"HS256" isn't supported`:         {Issuer: issuer, Audience: []string{"api"}, Algorithms: []string{"HS256"}},
		"must be an https URL":            {Issuer: "http://id.example.com", Audience: []string{"api"}},
		"jwks and jwks_url can't both":    {Issuer: issuer, Audience: []string{"api"}, JWKS: "{}", JWKSURL: "https://x"},
		"no key for verifying signatures": {Issuer: issuer, Audience: []string{"api"}, JWKS: `{"keys": []}`},
		"leeway must be":                  {Issuer: issuer, Audience: []string{"api"}, Leeway: time.Hour, RefreshInterval: time.Hour},
	}
	for want, cfg := range cases {
		if _, err := jwt.New(cfg); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	weak, _ := rsa.GenerateKey(rand.Reader, 1024)
	if _, err := jwt.New(jwt.Config{Issuer: issuer, Audience: []string{"api"}, JWKS: keySet(signer{"w", "RS256", weak})}); err == nil || !strings.Contains(err.Error(), "at least 2048") {
		t.Errorf("a weak key: %v", err)
	}
}

// idp is a fake OpenID provider whose keys can rotate.
type idp struct {
	srv      *httptest.Server
	mu       sync.Mutex
	jwks     string
	fetches  atomic.Int32
	failJWKS atomic.Bool
}

func newIDP(t *testing.T, ss ...signer) *idp {
	p := &idp{jwks: keySet(ss...)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"issuer": %q, "jwks_uri": %q}`, p.srv.URL, p.srv.URL+"/keys")
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		p.fetches.Add(1)
		if p.failJWKS.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		fmt.Fprint(w, p.jwks)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) rotate(ss ...signer) {
	p.mu.Lock()
	p.jwks = keySet(ss...)
	p.mu.Unlock()
}

func startAuth(t *testing.T, p *idp) *jwt.Authenticator {
	t.Helper()
	a, err := jwt.New(jwt.Config{Issuer: p.srv.URL, Audience: []string{"api"}}, jwt.WithClock(clock.NewFake(now)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	deadline := time.Now().Add(5 * time.Second)
	for a.Ready() != nil {
		if time.Now().After(deadline) {
			t.Fatal("the keys never loaded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return a
}

func TestDiscoveryAndRotation(t *testing.T) {
	rs, _, _, _, rotated := keys(t)
	p := newIDP(t, rs)
	a := startAuth(t, p)
	c := func() map[string]any { return claims(map[string]any{"iss": p.srv.URL}) }
	if _, err := a.Authenticate(context.Background(), bearer(rs.sign(t, nil, c()))); err != nil {
		t.Fatal(err)
	}
	// The issuer rotates to a new key: the first token signed with it
	// triggers one fetch, and a burst of them doesn't fail.
	p.rotate(rs, rotated)
	before := p.fetches.Load()
	var wg sync.WaitGroup
	var failed atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Authenticate(context.Background(), bearer(rotated.sign(t, nil, c()))); err != nil {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 || p.fetches.Load()-before != 1 {
		t.Fatalf("%d failed, %d fetches", failed.Load(), p.fetches.Load()-before)
	}
	// Another unknown key within the cooldown doesn't fetch again.
	unknown := signer{"nope", "ES256", ecKey}
	if _, err := a.Authenticate(context.Background(), bearer(unknown.sign(t, nil, c()))); errs.From(err).Code != security.CodeInvalidCredentials || p.fetches.Load()-before != 1 {
		t.Fatalf("unknown key: %v, %d fetches", err, p.fetches.Load()-before)
	}
}

func TestTheOldKeysStayWhenAFetchFails(t *testing.T) {
	rs, _, _, _, rotated := keys(t)
	p := newIDP(t, rs)
	a := startAuth(t, p)
	p.failJWKS.Store(true)
	if _, err := a.Authenticate(context.Background(), bearer(rotated.sign(t, nil, claims(map[string]any{"iss": p.srv.URL})))); errs.From(err).Code != security.CodeInvalidCredentials {
		t.Fatalf("unknown key while the issuer is down: %v", err)
	}
	if _, err := a.Authenticate(context.Background(), bearer(rs.sign(t, nil, claims(map[string]any{"iss": p.srv.URL})))); err != nil {
		t.Fatalf("a known key while the issuer is down: %v", err)
	}
}

func TestUnreadyUntilTheKeysLoad(t *testing.T) {
	rs, _, _, _, _ := keys(t)
	p := newIDP(t, rs)
	p.failJWKS.Store(true)
	a, err := jwt.New(jwt.Config{Issuer: p.srv.URL, Audience: []string{"api"}}, jwt.WithClock(clock.NewFake(now)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	if a.Ready() == nil {
		t.Fatal("ready without keys")
	}
	_, err = a.Authenticate(context.Background(), bearer(rs.sign(t, nil, claims(map[string]any{"iss": p.srv.URL}))))
	if errs.From(err).Kind != errs.Unavailable {
		t.Fatalf("a token before the keys load: %v", err)
	}
	p.failJWKS.Store(false)
	deadline := time.Now().Add(10 * time.Second)
	for a.Ready() != nil {
		if time.Now().After(deadline) {
			t.Fatal("the keys never loaded after the issuer recovered")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDiscoveryChecksTheIssuer(t *testing.T) {
	rs, _, _, _, _ := keys(t)
	p := newIDP(t, rs)
	// Configured with a different spelling of the issuer than the one the
	// document names.
	a, err := jwt.New(jwt.Config{Issuer: p.srv.URL + "/", Audience: []string{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	time.Sleep(100 * time.Millisecond)
	if a.Ready() == nil || p.fetches.Load() != 0 {
		t.Fatal("keys were loaded for a mismatched issuer")
	}
}

func TestBearerReadsTheJWTSection(t *testing.T) {
	rs, _, _, _, _ := keys(t)
	cfg, _ := json.Marshal(map[string]any{"jwt": map[string]any{"issuer": issuer, "audience": []string{"api"}, "jwks": keySet(rs)}})
	list := op.Unary[struct{}, struct{ Subject string }]("notes.list").HTTP("GET", "/notes").Authorize("notes:list")
	app, err := galleon.New(
		galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode), galleon.DrainGrace(0),
		galleon.Clock(clock.NewFake(now)),
		galleon.ConfigJSON(cfg),
		galleon.Modules(
			security.Module(jwt.Bearer(), security.Decide(&security.RoleTable{Grants: map[string][]string{"editor": {"notes:*"}}})),
			galleon.ModuleFunc("notes", func(r *galleon.Registry) error {
				r.Operation(list, func(ctx context.Context, _ *struct{}) (*struct{ Subject string }, error) {
					return &struct{ Subject string }{security.Caller(ctx).Subject()}, nil
				})
				return nil
			})))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	if err := app.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	ops := galleon.MustResolve[*galleon.Operations](app)
	out, err := ops.Lookup("notes.list").Invoke(context.Background(), &op.Call{Input: &struct{}{}, Request: bearer(rs.sign(t, nil, claims(nil)))})
	if err != nil || out.(*struct{ Subject string }).Subject != "u_1" {
		t.Fatalf("%v, %v", out, err)
	}

	_, err = galleon.New(galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode),
		galleon.Modules(security.Module(jwt.Bearer(), security.Decide(&security.RoleTable{}))))
	if err == nil || !strings.Contains(err.Error(), `config section "jwt" is invalid: issuer must be set`) {
		t.Fatalf("without the section: %v", err)
	}
}
