package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Credentials authenticate requests.
type Credentials interface {
	Apply(ctx context.Context, req *http.Request) error
}

// CredentialsFunc is a function as Credentials.
type CredentialsFunc func(ctx context.Context, req *http.Request) error

func (f CredentialsFunc) Apply(ctx context.Context, req *http.Request) error { return f(ctx, req) }

// Bearer sends a fixed bearer token.
func Bearer(token string) Credentials {
	return CredentialsFunc(func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	})
}

// BearerFunc sends the token fn returns, for tokens that rotate.
func BearerFunc(fn func(ctx context.Context) (string, error)) Credentials {
	return CredentialsFunc(func(ctx context.Context, req *http.Request) error {
		tok, err := fn(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return nil
	})
}

// HeaderValue sends a fixed header, such as an API key.
func HeaderValue(name, value string) Credentials {
	return CredentialsFunc(func(_ context.Context, req *http.Request) error {
		req.Header.Set(name, value)
		return nil
	})
}

type credentials struct {
	c       Credentials
	hosts   []string
	origins []origin
}

// origin is where credentials may go: a scheme and host, and a port unless
// it is zero, for any port.
type origin struct {
	scheme, host string
	port         int
}

// WithCredentials authenticates requests to hosts. A host is "host" (any
// port) or "host:port", and takes the credentials over https only; one given
// as a URL, such as "http://host:8080", takes them over that scheme instead.
// With no hosts, the credentials go to the BaseURL's scheme, host and port
// (the scheme's default port when it names none). Requests anywhere else, such
// as a redirect to another host or from https to http, go without them.
func WithCredentials(c Credentials, hosts ...string) Option {
	return func(s *settings) {
		if c == nil {
			s.err = errors.Join(s.err, errors.New("client: nil credentials"))
			return
		}
		s.creds = append(s.creds, credentials{c: c, hosts: hosts})
	}
}

// baseOrigin is the BaseURL's origin, with its port made explicit.
func baseOrigin(u *url.URL) origin {
	return origin{scheme: u.Scheme, host: strings.ToLower(u.Hostname()), port: port(u)}
}

// parseOrigin parses a WithCredentials host.
func parseOrigin(h string) (origin, error) {
	o := origin{scheme: "https"}
	if strings.Contains(h, "://") {
		u, err := url.Parse(h)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || strings.Trim(u.Path, "/") != "" {
			return origin{}, fmt.Errorf("client: credentials host %q must be a host, host:port or an http(s) URL with no path", h)
		}
		o.scheme, h = u.Scheme, u.Host
	}
	if a, err := netip.ParseAddr(h); err == nil && a.Is6() {
		h = "[" + h + "]" // a bare IPv6 address, which has no port
	}
	u := &url.URL{Host: h}
	if u.Hostname() == "" || strings.ContainsAny(h, "/?#@") {
		return origin{}, fmt.Errorf("client: credentials host %q must be a host, host:port or an http(s) URL with no path", h)
	}
	o.host = strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return origin{}, fmt.Errorf("client: credentials host %q has an invalid port", h)
		}
		o.port = n
	}
	return o, nil
}

func (o origin) matches(u *url.URL) bool {
	return u.Scheme == o.scheme && strings.EqualFold(u.Hostname(), o.host) && (o.port == 0 || port(u) == o.port)
}

type credentialed struct {
	next  http.RoundTripper
	creds []credentials
}

func (c *credentialed) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := false
	for _, cr := range c.creds {
		if !slices.ContainsFunc(cr.origins, func(o origin) bool { return o.matches(req.URL) }) {
			continue
		}
		if !cloned {
			req = req.Clone(req.Context())
			cloned = true
		}
		if err := cr.c.Apply(req.Context(), req); err != nil {
			closeBody(req)
			return nil, err
		}
	}
	return c.next.RoundTrip(req)
}
