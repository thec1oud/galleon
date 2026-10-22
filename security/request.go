package security

import (
	"context"
	"fmt"
	"sync"

	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

// state is what the security stages know of one call. The principal and
// context are set before the handler runs and never change after; the
// decision cache may be used from goroutines the handler starts.
type state struct {
	e         *engine
	call      *op.Call
	chain     []int // the indexes of the operation's authenticators
	principal Principal
	scheme    string // the scheme that authenticated it, if one did
	context   OperatingContext

	mu    sync.Mutex
	cache map[cacheKey]Decision
}

// cacheKey identifies a query within a call, whose subject and context are
// fixed.
type cacheKey struct {
	action, typ, id string
	resource        bool
}

type stateKey struct{}
type principalKey struct{}

func stateFrom(ctx context.Context) *state {
	st, _ := ctx.Value(stateKey{}).(*state)
	return st
}

// inherited returns the principal a call made in process carries over: the
// principal of the call ctx belongs to, or one set with WithPrincipal.
func inherited(ctx context.Context) Principal {
	if st := stateFrom(ctx); st != nil {
		return st.principal
	}
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}

// WithPrincipal returns a context carrying p, for calls made in process, as
// galleon.Operations.Call makes them: such a call runs as p, and is
// authorized as p, instead of authenticating a request. A call made in
// process from within another call runs as that call's principal without
// it. Calls that arrive over a network, over HTTP or RPC, never use it: only
// their own credentials count. Calls made in process aren't subject to the
// schemes an operation names, which restrict how a request presents
// credentials.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// Caller returns the principal of the call ctx belongs to. Outside a call
// the security module serves, it returns the principal set with
// WithPrincipal, or the anonymous one. It never returns nil.
func Caller(ctx context.Context) Principal {
	if p := inherited(ctx); p != nil {
		return p
	}
	return Anonymous()
}

// Context returns the operating context of the call ctx belongs to, or nil
// if it has none.
func Context(ctx context.Context) OperatingContext {
	if st := stateFrom(ctx); st != nil {
		return st.context
	}
	return nil
}

// Scheme returns the scheme that authenticated the call ctx belongs to, or
// "" if none did, as for an anonymous caller or a call made in process.
func Scheme(ctx context.Context) string {
	if st := stateFrom(ctx); st != nil {
		return st.scheme
	}
	return ""
}

// Can reports whether the caller of the call ctx belongs to may do action,
// on r if it isn't nil, in the call's context. The decision is cached for
// the rest of the call, unless r has attributes or it carries obligations.
// If no decision could be made, it returns false and an Unavailable error.
func Can(ctx context.Context, action string, r *Resource) (bool, error) {
	st := stateFrom(ctx)
	if st == nil {
		return false, noCall()
	}
	if st.e.decider == nil {
		return false, nil
	}
	dec, err := st.decide(ctx, Query{Subject: st.principal, Action: action, Resource: r, Context: st.context})
	if err != nil {
		return false, err
	}
	return dec.Allow, nil
}

// Require returns nil if Can allows, and otherwise the error to return to
// the caller: Unauthenticated for an anonymous caller, PermissionDenied for
// any other, or Unavailable if no decision could be made.
func Require(ctx context.Context, action string, r *Resource) error {
	st := stateFrom(ctx)
	if st == nil {
		return noCall()
	}
	return st.require(ctx, action, r)
}

// CanEach reports, for each resource in rs, whether the caller may do
// action on it, as Can does, deciding the ones not already cached in one
// batch.
func CanEach(ctx context.Context, action string, rs []Resource) ([]bool, error) {
	st := stateFrom(ctx)
	if st == nil {
		return nil, noCall()
	}
	out := make([]bool, len(rs))
	if st.e.decider == nil {
		return out, nil
	}
	var qs []Query
	var at []int
	for i := range rs {
		q := Query{Subject: st.principal, Action: action, Resource: &rs[i], Context: st.context}
		if dec, ok := st.cached(q); ok {
			out[i] = dec.Allow
			continue
		}
		qs = append(qs, q)
		at = append(at, i)
	}
	if len(qs) == 0 {
		return out, nil
	}
	decs, err := st.e.decider.CheckMany(ctx, qs)
	if err == nil && len(decs) != len(qs) {
		err = fmt.Errorf("the decider returned %d decisions for %d queries", len(decs), len(qs))
	}
	if err != nil {
		return nil, st.e.unavailable(ctx, "authorization", st.call, err)
	}
	for j, q := range qs {
		dec, err := st.settle(ctx, q, decs[j])
		if err != nil {
			return nil, err
		}
		out[at[j]] = dec.Allow
	}
	return out, nil
}

func noCall() error {
	return errs.New(errs.Internal, CodeNoCall, "authorization was asked for outside a call")
}

// require decides q and turns a denial into the caller's error.
func (st *state) require(ctx context.Context, action string, r *Resource) error {
	if st.e.decider == nil {
		return st.deny(Deny("no decider"), action)
	}
	dec, err := st.decide(ctx, Query{Subject: st.principal, Action: action, Resource: r, Context: st.context})
	if err != nil {
		return err
	}
	if !dec.Allow {
		return st.deny(dec, action)
	}
	return nil
}

func (st *state) deny(dec Decision, action string) error {
	st.e.log.Debug("security: denied", "operation", st.call.Desc.ID, "action", action, "subject", st.principal.Subject(), "reason", dec.Reason)
	if IsAnonymous(st.principal) {
		return st.unauthenticated()
	}
	return errs.New(errs.PermissionDenied, CodeDenied, "permission denied")
}

// unauthenticated is the error for an anonymous caller who needs to
// authenticate. Over HTTP it carries the challenges of the operation's
// authenticators.
func (st *state) unauthenticated() error {
	for _, i := range st.chain {
		st.challenge(i)
	}
	return errs.New(errs.Unauthenticated, CodeUnauthenticated, "authentication is required")
}

// challenge adds authenticator i's challenge, if it has one, to the call's
// response.
func (st *state) challenge(i int) {
	c := st.e.challenges[i]
	if c == "" || st.call.Response == nil || st.call.Response.Header == nil {
		return
	}
	h := st.call.Response.Header
	for _, v := range h.Values("WWW-Authenticate") {
		if v == c {
			return
		}
	}
	h.Add("WWW-Authenticate", c)
}

func keyOf(q Query) (cacheKey, bool) {
	if q.Resource == nil {
		return cacheKey{action: q.Action}, true
	}
	if q.Resource.Attributes.Len() > 0 {
		return cacheKey{}, false
	}
	return cacheKey{action: q.Action, typ: q.Resource.Type, id: q.Resource.ID, resource: true}, true
}

func (st *state) cached(q Query) (Decision, bool) {
	k, ok := keyOf(q)
	if !ok {
		return Decision{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	dec, ok := st.cache[k]
	return dec, ok
}

// decide answers q from the cache, or asks the decider.
func (st *state) decide(ctx context.Context, q Query) (Decision, error) {
	if dec, ok := st.cached(q); ok {
		return dec, nil
	}
	dec, err := st.e.decider.Check(ctx, q)
	if err != nil {
		return Decision{}, st.e.unavailable(ctx, "authorization", st.call, err)
	}
	return st.settle(ctx, q, dec)
}

// settle fulfils an allowing decision's obligations, denying if one has no
// handler or its handler fails, and caches a decision that carried none.
func (st *state) settle(ctx context.Context, q Query, dec Decision) (Decision, error) {
	if dec.Allow {
		for _, o := range dec.Obligations {
			fn := st.e.obligations[o.Name]
			if fn == nil {
				st.e.log.WarnContext(ctx, "security: a decision carries an obligation with no handler, so it denies", "operation", st.call.Desc.ID, "action", q.Action, "obligation", o.Name)
				return Deny("no handler for the obligation " + o.Name), nil
			}
			if err := fn(ctx, q, o); err != nil {
				if ctx.Err() != nil {
					return Decision{}, errs.From(ctx.Err())
				}
				st.e.log.WarnContext(ctx, "security: fulfilling an obligation failed, so the decision denies", "operation", st.call.Desc.ID, "action", q.Action, "obligation", o.Name, "error", err)
				return Deny("the obligation " + o.Name + " failed"), nil
			}
		}
	}
	if len(dec.Obligations) == 0 {
		if k, ok := keyOf(q); ok {
			st.mu.Lock()
			if st.cache == nil {
				st.cache = map[cacheKey]Decision{}
			}
			st.cache[k] = dec
			st.mu.Unlock()
		}
	}
	return dec, nil
}
