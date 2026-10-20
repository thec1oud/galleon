package client

import (
	"context"
	"errors"
	"net/http"
	"slices"
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
	c     Credentials
	hosts []string
}

// WithCredentials authenticates requests to hosts, given as "host" (any
// port) or "host:port"; with none, to the BaseURL's host. Requests to other
// hosts, such as a redirect's target, go without them.
func WithCredentials(c Credentials, hosts ...string) Option {
	return func(s *settings) {
		if c == nil {
			s.err = errors.Join(s.err, errors.New("client: nil credentials"))
			return
		}
		s.creds = append(s.creds, credentials{c: c, hosts: hosts})
	}
}

type credentialed struct {
	next  http.RoundTripper
	creds []credentials
}

func (c *credentialed) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := false
	for _, cr := range c.creds {
		if !slices.Contains(cr.hosts, req.URL.Host) && !slices.Contains(cr.hosts, req.URL.Hostname()) {
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
