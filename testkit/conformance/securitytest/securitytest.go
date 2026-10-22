// Package securitytest is the conformance suite for the security plane's
// seams: deciders, and the filters a decider that is also a
// security.Filterer returns, and authenticators. Every implementation runs
// it in its own tests:
//
//	func TestConformance(t *testing.T) {
//		securitytest.Decider(t, mydecider.New(...), securitytest.Cases{
//			Checks: []securitytest.Check{
//				{Query: security.Query{Subject: editor, Action: "notes:edit"}, Allow: true},
//				{Query: security.Query{Subject: viewer, Action: "notes:edit"}, Allow: false},
//			},
//		})
//	}
//
// A decider that is a Filterer claims that capability, which is a suite to
// pass: Cases must then have Filters, whose soundness it checks.
package securitytest

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"

	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/security"
)

// Cases are the queries a decider's suite asks, with the answers its
// policy should give.
type Cases struct {
	// Checks are queries and the decisions they should get. Include both
	// allowed and denied ones.
	Checks []Check
	// Filters are list questions, each with a sample of resources. Every
	// resource a filter matches must be one Check allows.
	Filters []FilterCase
}

// Check is one query and whether it should be allowed.
type Check struct {
	Query security.Query
	Allow bool
}

// FilterCase is one list question and resources to check its filter on.
type FilterCase struct {
	Query     security.FilterQuery
	Resources []security.Resource
}

// Decider checks d against its policy's answers, and the decider seam's
// rules: CheckMany answers in its queries' order, one decision each, as
// Check does; it is safe for concurrent use; and, if d is a Filterer, its
// filters are sound.
func Decider(t *testing.T, d security.Decider, c Cases) {
	t.Helper()
	if len(c.Checks) == 0 {
		t.Fatal("securitytest: Cases has no Checks")
	}
	ctx := context.Background()
	t.Run("Check", func(t *testing.T) {
		for i, ch := range c.Checks {
			dec, err := d.Check(ctx, ch.Query)
			if err != nil {
				t.Errorf("check %d (%s): %v", i, describe(ch.Query), err)
				continue
			}
			if dec.Allow != ch.Allow {
				t.Errorf("check %d (%s): allow %v (%s), want %v", i, describe(ch.Query), dec.Allow, dec.Reason, ch.Allow)
			}
		}
	})
	t.Run("CheckManyKeepsOrder", func(t *testing.T) {
		// Ask in reverse as well, so an order-blind implementation fails.
		for _, reverse := range []bool{false, true} {
			qs := make([]security.Query, len(c.Checks))
			want := make([]bool, len(c.Checks))
			for i, ch := range c.Checks {
				j := i
				if reverse {
					j = len(c.Checks) - 1 - i
				}
				qs[j], want[j] = ch.Query, ch.Allow
			}
			decs, err := d.CheckMany(ctx, qs)
			if err != nil {
				t.Fatal(err)
			}
			if len(decs) != len(qs) {
				t.Fatalf("CheckMany returned %d decisions for %d queries", len(decs), len(qs))
			}
			for i := range decs {
				if decs[i].Allow != want[i] {
					t.Errorf("CheckMany (reverse %v) [%d] (%s): allow %v, want %v", reverse, i, describe(qs[i]), decs[i].Allow, want[i])
				}
			}
		}
		decs, err := d.CheckMany(ctx, nil)
		if err != nil || len(decs) != 0 {
			t.Errorf("CheckMany(nil): %d decisions, %v", len(decs), err)
		}
	})
	t.Run("Concurrent", func(t *testing.T) {
		var wg sync.WaitGroup
		errc := make(chan error, 8*len(c.Checks))
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i, ch := range c.Checks {
					dec, err := d.Check(ctx, ch.Query)
					if err == nil && dec.Allow != ch.Allow {
						err = fmt.Errorf("allow %v, want %v", dec.Allow, ch.Allow)
					}
					if err != nil {
						errc <- fmt.Errorf("check %d: %w", i, err)
					}
				}
			}()
		}
		wg.Wait()
		close(errc)
		for err := range errc {
			t.Error(err)
		}
	})
	f, ok := d.(security.Filterer)
	t.Run("FilterIsSound", func(t *testing.T) {
		switch {
		case !ok:
			t.Skip("not a Filterer")
		case len(c.Filters) == 0:
			t.Fatal("the decider is a Filterer, so Cases needs Filters to check it against")
		}
		for i, fc := range c.Filters {
			filter, err := f.Filter(ctx, fc.Query)
			if err != nil {
				t.Errorf("filter %d: %v", i, err)
				continue
			}
			for _, r := range fc.Resources {
				if !filter.Match(r.Attributes) {
					continue
				}
				r := r
				q := security.Query{Subject: fc.Query.Subject, Action: fc.Query.Action, Resource: &r, Context: fc.Query.Context}
				dec, err := d.Check(ctx, q)
				if err != nil || !dec.Allow {
					t.Errorf("filter %d (%v) matches %s %q, which Check denies (%v)", i, filter, r.Type, r.ID, err)
				}
			}
		}
	})
}

func describe(q security.Query) string {
	s := "anonymous"
	if q.Subject != nil && !security.IsAnonymous(q.Subject) {
		s = q.Subject.Subject()
	}
	s += " " + q.Action
	if q.Resource != nil {
		s += " " + q.Resource.Type + " " + q.Resource.ID
	}
	return s
}

// Credentials are requests to check an authenticator with.
type Credentials struct {
	// None carry no credentials for the scheme. Include one with no
	// headers at all, and one with another scheme's credentials.
	None []*op.Request
	// Invalid carry credentials of the scheme that don't verify.
	Invalid []*op.Request
	// Valid carry credentials that verify, each as the subject given.
	Valid []Valid
}

// Valid is a request whose credentials prove Subject.
type Valid struct {
	Request *op.Request
	Subject string
}

var schemePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Authenticator checks a against the identity seam's rules: a request
// without credentials gets ErrNoCredentials, so the next scheme is tried;
// invalid credentials get an Unauthenticated error, never ErrNoCredentials;
// valid ones get their principal; and it is safe for concurrent use.
func Authenticator(t *testing.T, a security.Authenticator, c Credentials) {
	t.Helper()
	if len(c.None) == 0 || len(c.Invalid) == 0 || len(c.Valid) == 0 {
		t.Fatal("securitytest: Credentials needs None, Invalid and Valid requests")
	}
	ctx := context.Background()
	t.Run("Scheme", func(t *testing.T) {
		if !schemePattern.MatchString(a.Scheme()) {
			t.Errorf("the scheme %q isn't lower case", a.Scheme())
		}
	})
	t.Run("NoCredentials", func(t *testing.T) {
		for i, r := range c.None {
			if _, err := a.Authenticate(ctx, r); !errors.Is(err, security.ErrNoCredentials) {
				t.Errorf("request %d: want ErrNoCredentials, got %v", i, err)
			}
		}
	})
	t.Run("InvalidCredentials", func(t *testing.T) {
		for i, r := range c.Invalid {
			_, err := a.Authenticate(ctx, r)
			if err == nil || errors.Is(err, security.ErrNoCredentials) || errs.From(err).Kind != errs.Unauthenticated {
				t.Errorf("request %d: want an Unauthenticated error, got %v", i, err)
			}
		}
	})
	t.Run("ValidCredentials", func(t *testing.T) {
		for i, v := range c.Valid {
			p, err := a.Authenticate(ctx, v.Request)
			switch {
			case err != nil:
				t.Errorf("request %d: %v", i, err)
			case security.IsAnonymous(p):
				t.Errorf("request %d: the principal is anonymous", i)
			case p.Subject() != v.Subject:
				t.Errorf("request %d: subject %q, want %q", i, p.Subject(), v.Subject)
			}
		}
	})
	t.Run("Concurrent", func(t *testing.T) {
		var wg sync.WaitGroup
		errc := make(chan error, 8*len(c.Valid))
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, v := range c.Valid {
					if p, err := a.Authenticate(ctx, v.Request); err != nil || p == nil || p.Subject() != v.Subject {
						errc <- fmt.Errorf("%v, %v", p, err)
					}
				}
			}()
		}
		wg.Wait()
		close(errc)
		for err := range errc {
			t.Error(err)
		}
	})
}
