package op

import (
	"context"
	"time"
)

// Builder declares a unary operation in Go. Each method returns the builder,
// so a declaration reads as one expression:
//
//	var CreateOrder = op.Unary[CreateOrderIn, CreateOrderOut]("orders.create").
//		HTTP("POST", "/orders").
//		RPC("orders.v1.Orders", "Create").
//		Authorize("orders:create", op.Resource("org", "org_id")).
//		Timeout(5 * time.Second)
type Builder[In, Out any] struct{ d Descriptor }

// Unary starts the declaration of a unary operation with the given ID.
func Unary[In, Out any](id string) *Builder[In, Out] {
	return &Builder[In, Out]{d: Descriptor{
		ID:     id,
		Kind:   KindUnary,
		Input:  TypeOf[In](),
		Output: TypeOf[Out](),
		Adapt:  Adapter[In, Out](),
	}}
}

// Describe returns a copy of the descriptor built so far.
func (b *Builder[In, Out]) Describe() Descriptor {
	d := b.d
	d.Bindings = append([]Binding(nil), d.Bindings...)
	d.Errors = append([]string(nil), d.Errors...)
	d.Security.Schemes = append([]string(nil), d.Security.Schemes...)
	d.Security.Contexts = append([]string(nil), d.Security.Contexts...)
	if d.Meta != nil {
		m := make(map[string]any, len(d.Meta))
		for k, v := range d.Meta {
			m[k] = v
		}
		d.Meta = m
	}
	return d
}

func (b *Builder[In, Out]) HTTP(method, path string) *Builder[In, Out] {
	b.d.Bindings = append(b.d.Bindings, HTTP{Method: method, Path: path})
	return b
}

func (b *Builder[In, Out]) RPC(service, method string) *Builder[In, Out] {
	b.d.Bindings = append(b.d.Bindings, RPC{Service: service, Method: method})
	return b
}

func (b *Builder[In, Out]) Version(v string) *Builder[In, Out] { b.d.Version = v; return b }

func (b *Builder[In, Out]) Deprecated(d Deprecation) *Builder[In, Out] {
	b.d.Deprecated = &d
	return b
}

// Errors lists the error catalog codes the operation can return.
func (b *Builder[In, Out]) Errors(codes ...string) *Builder[In, Out] {
	b.d.Errors = append(b.d.Errors, codes...)
	return b
}

// Public declares that anyone may call the operation.
func (b *Builder[In, Out]) Public() *Builder[In, Out] { b.d.Security.Public = true; return b }

// Schemes restricts the authentication schemes the operation accepts.
func (b *Builder[In, Out]) Schemes(s ...string) *Builder[In, Out] {
	b.d.Security.Schemes = append(b.d.Security.Schemes, s...)
	return b
}

// Contexts restricts the operating context kinds the operation accepts.
func (b *Builder[In, Out]) Contexts(kinds ...string) *Builder[In, Out] {
	b.d.Security.Contexts = append(b.d.Security.Contexts, kinds...)
	return b
}

// Authorize requires permission for action, checked before decode, and for the
// resource, if one is given, checked after decode.
func (b *Builder[In, Out]) Authorize(action string, resource ...ResourceRef) *Builder[In, Out] {
	b.d.Security.Action = action
	if len(resource) > 0 {
		r := resource[0]
		b.d.Security.Resource = &r
	}
	return b
}

// Resource names the resource kind and the input field that holds its ID.
func Resource(kind, field string) ResourceRef { return ResourceRef{Kind: kind, Field: field} }

func (b *Builder[In, Out]) Timeout(d time.Duration) *Builder[In, Out] {
	b.d.Policies.Timeout = d
	return b
}

func (b *Builder[In, Out]) BodyLimit(n int64) *Builder[In, Out] {
	b.d.Policies.BodyLimit = n
	return b
}

// RateLimit allows limit calls per window for each caller.
func (b *Builder[In, Out]) RateLimit(limit int64, window time.Duration) *Builder[In, Out] {
	b.d.Policies.RateLimit = &RateLimit{Limit: limit, Window: window}
	return b
}

// RateLimitBy is RateLimit with a custom key.
func (b *Builder[In, Out]) RateLimitBy(limit int64, window time.Duration, key func(*Call) string) *Builder[In, Out] {
	b.d.Policies.RateLimit = &RateLimit{Limit: limit, Window: window, Key: key}
	return b
}

func (b *Builder[In, Out]) Idempotent() *Builder[In, Out] { b.d.Policies.Idempotent = true; return b }

// AllowUnknownFields makes decoding ignore input fields the type doesn't have,
// instead of rejecting them.
func (b *Builder[In, Out]) AllowUnknownFields() *Builder[In, Out] {
	b.d.Policies.UnknownFields = IgnoreUnknown
	return b
}

func (b *Builder[In, Out]) Docs(d Docs) *Builder[In, Out] { b.d.Docs = d; return b }

// Meta sets an adapter-specific value.
func (b *Builder[In, Out]) Meta(key string, v any) *Builder[In, Out] {
	if b.d.Meta == nil {
		b.d.Meta = map[string]any{}
	}
	b.d.Meta[key] = v
	return b
}

// Validate adds a shape validator (stage 7). It must be pure: no I/O.
func (b *Builder[In, Out]) Validate(v Validator[In]) *Builder[In, Out] {
	b.d.Shape = chainValidators(b.d.Shape, func(ctx context.Context, in any) *ValidationError {
		return v.Validate(ctx, in.(*In))
	})
	return b
}

// ValidateMeaning adds a meaning validator (stage 9), which may read through
// the read transaction in ctx but must not write.
func (b *Builder[In, Out]) ValidateMeaning(v MeaningValidator[In]) *Builder[In, Out] {
	b.d.Meaning = chainValidators(b.d.Meaning, func(ctx context.Context, in any) *ValidationError {
		return v.ValidateMeaning(ctx, in.(*In))
	})
	return b
}

// chainValidators runs a then b and merges their violations.
func chainValidators(a, b func(context.Context, any) *ValidationError) func(context.Context, any) *ValidationError {
	if a == nil {
		return b
	}
	return func(ctx context.Context, in any) *ValidationError {
		return a(ctx, in).Merge(b(ctx, in))
	}
}
