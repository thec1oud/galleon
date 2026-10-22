package security

import (
	"context"
	"fmt"
)

// Decider makes authorization decisions: may this subject do this action on
// this resource, in this context? Its query follows the OpenID AuthZEN
// Authorization API's subject, action, resource and context, so an AuthZEN
// engine plugs in through a thin adapter.
//
// An error means no decision, and the call is denied as Unavailable. A
// decider must be safe for concurrent use.
type Decider interface {
	Check(ctx context.Context, q Query) (Decision, error)
	// CheckMany decides every query, returning the decisions in the
	// queries' order, one for each.
	CheckMany(ctx context.Context, qs []Query) ([]Decision, error)
}

// Query is one authorization question.
type Query struct {
	Subject Principal
	// Action is what the subject asks to do, such as "orders:cancel".
	Action string
	// Resource is what the action is on. It is nil at pre-authorization,
	// which decides the action alone, before the input is decoded.
	Resource *Resource
	// Context is the operating context, or nil if the call has none.
	Context OperatingContext
}

// Resource is what an action is on.
type Resource struct {
	Type       string // such as "order"
	ID         string
	Attributes Attributes // such as an owner, for attribute-based decisions
}

// Decision is a decider's answer.
type Decision struct {
	Allow bool
	// Reason explains the decision, for logs and audits. It is never sent
	// to clients.
	Reason string
	// Obligations are what the decision holds only on condition of, such
	// as "audit". The module fulfils each with the handler registered for
	// its name (see Obligation), before the call goes on; one it has no
	// handler for, or whose handler fails, denies.
	Obligations []Obligation
}

// Obligation is a condition attached to a decision.
type Obligation struct {
	Name   string
	Params map[string]any
}

// Allow and Deny are decisions without obligations.
func Allow(reason string) Decision { return Decision{Allow: true, Reason: reason} }
func Deny(reason string) Decision  { return Decision{Reason: reason} }

// DeciderFunc adapts a function to Decider. Its CheckMany calls it once per
// query.
type DeciderFunc func(ctx context.Context, q Query) (Decision, error)

func (f DeciderFunc) Check(ctx context.Context, q Query) (Decision, error) { return f(ctx, q) }

func (f DeciderFunc) CheckMany(ctx context.Context, qs []Query) ([]Decision, error) {
	return checkEach(ctx, f, qs)
}

// checkEach decides qs one at a time.
func checkEach(ctx context.Context, d interface {
	Check(context.Context, Query) (Decision, error)
}, qs []Query) ([]Decision, error) {
	out := make([]Decision, len(qs))
	for i, q := range qs {
		dec, err := d.Check(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("query %d: %w", i, err)
		}
		out[i] = dec
	}
	return out, nil
}
