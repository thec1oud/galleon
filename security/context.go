package security

import (
	"context"
	"time"

	"github.com/thec1oud/galleon/op"
)

// OperatingContext is the context a caller acts in, such as an organization,
// a team or a locality: context-based access control's slot. Operations name
// the domains they accept with op.Builder.Contexts, and decisions get it in
// their Query.
type OperatingContext interface {
	// Domain is the kind of context, such as "org", "team" or "user".
	Domain() string
	// ID identifies the context within its domain.
	ID() string
	// Attributes are inputs for decisions: device, location, time, risk.
	Attributes() Attributes
	// Proof is the context's proof, verifiable without I/O, such as a
	// signed token, or nil. Its format belongs to the resolver.
	Proof() []byte
	// ExpiresAt is when the context stops being valid; zero is never. A
	// call whose context has expired fails as Unauthenticated.
	ExpiresAt() time.Time
}

// ContextData is the data of an operating context. Its Context method makes
// one, for resolvers and tests.
type ContextData struct {
	Domain     string
	ID         string
	Attributes Attributes
	Proof      []byte
	ExpiresAt  time.Time
}

// Context returns an OperatingContext with the data's values. Later changes
// to the Proof slice don't reach it.
func (d ContextData) Context() OperatingContext {
	d.Proof = append([]byte(nil), d.Proof...)
	return contextData{d}
}

type contextData struct{ d ContextData }

func (c contextData) Domain() string         { return c.d.Domain }
func (c contextData) ID() string             { return c.d.ID }
func (c contextData) Attributes() Attributes { return c.d.Attributes }
func (c contextData) Proof() []byte          { return append([]byte(nil), c.d.Proof...) }
func (c contextData) ExpiresAt() time.Time   { return c.d.ExpiresAt }

// ContextResolver works out the operating context of a call, after its
// principal is known. It must not do network I/O when the request carries a
// valid proof of its context; one that must is Slow.
type ContextResolver interface {
	// Resolve returns the call's context, or nil for none. in has the
	// attributes of the providers the operation names, and no others. An
	// errs.Error is returned to the caller as it is; any other error fails
	// the call as Unavailable.
	Resolve(ctx context.Context, p Principal, r *op.Request, in Inputs) (OperatingContext, error)
}

// Slow is a ContextResolver that may do I/O per call, such as a lookup of
// the caller's membership. The module lists slow resolvers at start.
type Slow interface {
	Slow() bool
}

// ResolverFunc adapts a function to ContextResolver.
type ResolverFunc func(ctx context.Context, p Principal, r *op.Request, in Inputs) (OperatingContext, error)

func (f ResolverFunc) Resolve(ctx context.Context, p Principal, r *op.Request, in Inputs) (OperatingContext, error) {
	return f(ctx, p, r, in)
}

// ContextProvider supplies attributes that resolving a context may use,
// such as the request's network, a billing plan or a risk score. Providers
// are named, and a resolver sees only those the operation names with
// op.Builder.Inputs, each evaluated at most once per call, when first asked
// for.
type ContextProvider interface {
	// Name names the provider, such as "request" or "billing".
	Name() string
	Provide(ctx context.Context, p Principal, r *op.Request) (Attributes, error)
}

// Inputs gives a resolver the attributes of the providers its operation
// names.
type Inputs interface {
	// Names lists the providers the operation names, in its order.
	Names() []string
	// Get returns the attributes of the provider named name, evaluating it
	// on first use. Asking for a provider the operation doesn't name is an
	// error.
	Get(ctx context.Context, name string) (Attributes, error)
}

// Gather returns the attributes of every provider in in, each prefixed with
// its provider's name and a dot, such as "request.ip".
func Gather(ctx context.Context, in Inputs) (Attributes, error) {
	if in == nil || len(in.Names()) == 0 {
		return Attributes{}, nil
	}
	m := map[string]any{}
	for _, name := range in.Names() {
		a, err := in.Get(ctx, name)
		if err != nil {
			return Attributes{}, err
		}
		for _, k := range a.Names() {
			m[name+"."+k], _ = a.Get(k)
		}
	}
	return Values{m: m}, nil
}

// IdentityResolver is the resolver a module uses when it is given none: a
// caller's context is its own. Its domain is the principal's kind ("user",
// "service" or "api_key") and its ID the principal's subject; an anonymous
// caller has no context. Its attributes are those of the operation's
// inputs, as Gather returns them.
//
// With it, an operation that names contexts it accepts fails boot, since no
// caller could prove one.
type IdentityResolver struct{}

func (IdentityResolver) Resolve(ctx context.Context, p Principal, _ *op.Request, in Inputs) (OperatingContext, error) {
	return ownContext(ctx, p, in)
}

func ownContext(ctx context.Context, p Principal, in Inputs) (OperatingContext, error) {
	if IsAnonymous(p) {
		return nil, nil
	}
	attrs, err := Gather(ctx, in)
	if err != nil {
		return nil, err
	}
	return ContextData{Domain: p.Kind().String(), ID: p.Subject(), Attributes: attrs}.Context(), nil
}

// ClaimResolver reads the context from a claim of the principal, which its
// authenticator has verified, so it needs no I/O: a token whose "org_id"
// claim is "acme" puts the caller in the context org "acme".
//
//	security.ResolveContext(security.ClaimResolver{Domain: "org", Claim: "org_id"})
//
// A principal without the claim, as a string, is in its own context, as
// IdentityResolver resolves it. The context's attributes are those of the
// operation's inputs, as Gather returns them.
type ClaimResolver struct {
	Domain string // the domain of the context, such as "org"
	Claim  string // the claim holding its ID, such as "org_id"
}

func (c ClaimResolver) Resolve(ctx context.Context, p Principal, _ *op.Request, in Inputs) (OperatingContext, error) {
	if IsAnonymous(p) {
		return nil, nil
	}
	id, ok := p.Claims().String(c.Claim)
	if !ok || id == "" {
		return ownContext(ctx, p, in)
	}
	attrs, err := Gather(ctx, in)
	if err != nil {
		return nil, err
	}
	return ContextData{Domain: c.Domain, ID: id, Attributes: attrs}.Context(), nil
}
