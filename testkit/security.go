package testkit

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/security"
)

// Security is a scripted security plane for tests: an authenticator, a
// context resolver and a decider in one. It denies everything the test
// doesn't allow, and records every query it decides.
//
//	sec := testkit.NewSecurity().Allow("notes:list")
//	ann := sec.User("ann", "editor")
//	h := testkit.New(t, galleon.Modules(
//		security.Module(security.Authenticate(sec), security.Decide(sec)),
//		Notes{},
//	))
//	ops := testkit.Get[*galleon.Operations](h)
//	out, err := ops.Call(security.WithPrincipal(ctx, ann), "notes.list", &ListIn{})
//
// Over HTTP, its authenticator, of scheme "test", accepts the header
// "Authorization: Test <subject>" for a subject made with User.
type Security struct {
	mu      sync.Mutex
	users   map[string]security.Principal
	allow   []grant
	ctx     *security.ContextData
	err     error
	queries []security.Query
}

type grant struct{ subject, action string } // subject "" is anyone signed in

// NewSecurity returns a Security that allows nothing.
func NewSecurity() *Security { return &Security{users: map[string]security.Principal{}} }

// User returns a user principal with the roles given as its "roles" claim,
// and lets the authenticator accept it.
func (s *Security) User(subject string, roles ...string) security.Principal {
	p := security.Identity{Subject: subject, Kind: security.KindUser, Issuer: "testkit",
		Claims: security.NewValues(map[string]any{"roles": slices.Clone(roles)})}.Principal()
	s.mu.Lock()
	s.users[subject] = p
	s.mu.Unlock()
	return p
}

// Allow allows actions to every principal but the anonymous one. "*"
// allows every action.
func (s *Security) Allow(actions ...string) *Security { return s.AllowFor("", actions...) }

// AllowFor allows actions to the principal with subject alone.
func (s *Security) AllowFor(subject string, actions ...string) *Security {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range actions {
		s.allow = append(s.allow, grant{subject, a})
	}
	return s
}

// InContext puts every caller but the anonymous one in the context given,
// when Security is the context resolver.
func (s *Security) InContext(domain, id string) *Security {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctx = &security.ContextData{Domain: domain, ID: id}
	return s
}

// Fail makes every check fail with err, as a decider whose engine is down
// would; nil restores it.
func (s *Security) Fail(err error) *Security {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
	return s
}

// Queries returns the queries decided so far, in order.
func (s *Security) Queries() []security.Query {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.queries)
}

// Scheme is "test".
func (s *Security) Scheme() string { return "test" }

// Authenticate accepts "Authorization: Test <subject>" for a subject made
// with User.
func (s *Security) Authenticate(_ context.Context, r *op.Request) (security.Principal, error) {
	scheme, subject, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Test") {
		return nil, security.ErrNoCredentials
	}
	s.mu.Lock()
	p := s.users[subject]
	s.mu.Unlock()
	if p == nil {
		return nil, security.InvalidCredentials("", errors.New("testkit: no such user"))
	}
	return p, nil
}

// Resolve puts the caller in the context InContext set, or its own.
func (s *Security) Resolve(ctx context.Context, p security.Principal, r *op.Request, in security.Inputs) (security.OperatingContext, error) {
	s.mu.Lock()
	c := s.ctx
	s.mu.Unlock()
	if c == nil || security.IsAnonymous(p) {
		return security.IdentityResolver{}.Resolve(ctx, p, r, in)
	}
	return c.Context(), nil
}

// Check allows what Allow and AllowFor allowed.
func (s *Security) Check(_ context.Context, q security.Query) (security.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, q)
	if s.err != nil {
		return security.Decision{}, s.err
	}
	if security.IsAnonymous(q.Subject) {
		return security.Deny("anonymous"), nil
	}
	for _, g := range s.allow {
		if (g.subject == "" || g.subject == q.Subject.Subject()) && (g.action == "*" || g.action == q.Action) {
			return security.Allow("testkit"), nil
		}
	}
	return security.Deny("testkit: not allowed"), nil
}

// CheckMany decides each query as Check does.
func (s *Security) CheckMany(ctx context.Context, qs []security.Query) ([]security.Decision, error) {
	out := make([]security.Decision, len(qs))
	for i, q := range qs {
		d, err := s.Check(ctx, q)
		if err != nil {
			return nil, err
		}
		out[i] = d
	}
	return out, nil
}
