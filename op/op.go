// Package op is the operation model: every API operation, however it was
// authored, becomes a Descriptor, and every call to it runs the same fixed
// pipeline of stages whatever transport it arrived on.
package op

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"time"
)

// Kind is the call shape of an operation.
type Kind uint8

const (
	KindUnary Kind = iota + 1
	KindServerStream
	KindClientStream
	KindBidiStream
	KindEvent
)

func (k Kind) String() string {
	switch k {
	case KindUnary:
		return "unary"
	case KindServerStream:
		return "server_stream"
	case KindClientStream:
		return "client_stream"
	case KindBidiStream:
		return "bidi_stream"
	case KindEvent:
		return "event"
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// Binding is one way an operation is reached: an HTTP route, an RPC method or
// an event topic.
type Binding interface{ binding() }

// HTTP binds an operation to a method and a net/http ServeMux path pattern,
// such as "/orders/{id}".
type HTTP struct{ Method, Path string }

// RPC binds an operation to a fully qualified service and method, such as
// "orders.v1.Orders" and "Create".
type RPC struct{ Service, Method string }

// Topic binds an event operation to a topic.
type Topic struct{ Name string }

func (HTTP) binding()  {}
func (RPC) binding()   {}
func (Topic) binding() {}

func (b HTTP) String() string  { return b.Method + " " + b.Path }
func (b RPC) String() string   { return "/" + b.Service + "/" + b.Method }
func (b Topic) String() string { return "topic " + b.Name }

// TypeInfo describes an operation's input or output type.
type TypeInfo struct {
	Type reflect.Type // the struct type; values travel as pointers to it
	New  func() any   // returns a new *T
	// Codecs are codecs made for this type, such as generated reflection-free
	// ones. Transports prefer them over the app's codecs for their media types.
	Codecs []Codec
}

// Codec reads and writes one media type. It is the payload format seam; the
// codec package has the built-in JSON codec and content negotiation.
type Codec interface {
	// MediaType is the type the codec handles, such as "application/json".
	MediaType() string
	// Decode reads one value from r into v, a pointer. Bad input is reported
	// as errs.InvalidArgument with field violations where possible.
	Decode(r io.Reader, v any, unknown UnknownFields) error
	// Encode writes v.
	Encode(w io.Writer, v any) error
}

// Security declares who may call an operation. An operation must either be
// Public or name an Action; boot fails otherwise.
type Security struct {
	Public   bool
	Schemes  []string     // accepted authentication schemes; empty means any
	Contexts []string     // accepted operating context kinds; empty means any
	Action   string       // checked before decode (stage 5b)
	Resource *ResourceRef // checked after decode (stage 8)
}

// ResourceRef names the resource an operation acts on: its kind, and the JSON
// field of the input that holds its ID.
type ResourceRef struct{ Kind, Field string }

// Policies are per-operation limits, enforced by the pipeline.
type Policies struct {
	Timeout       time.Duration // 0 means no per-operation timeout
	BodyLimit     int64         // bytes; 0 means the transport default
	RateLimit     *RateLimit
	Idempotent    bool
	UnknownFields UnknownFields
}

// UnknownFields says what decoding does with input fields the type doesn't have.
type UnknownFields uint8

const (
	RejectUnknown UnknownFields = iota // the default
	IgnoreUnknown
)

// RateLimit allows Limit calls per Window for each key. Key defaults to the
// caller's IP address, and a call without one isn't limited; a custom Key can
// return "" to skip the limit for a call.
type RateLimit struct {
	Limit  int64
	Window time.Duration
	Key    func(*Call) string
}

// ReadConsistency is a read consistency of the data plane, such as
// data.ReadYourWrites, which an operation declares as its reads' default.
// The operation plane only carries it; the data module reads it. One with
// a Validate() error method, as data.Level has, is validated at
// registration, so a bad one fails boot.
type ReadConsistency interface{ ConsistencyLevel() string }

// Deprecation marks an operation as deprecated.
type Deprecation struct {
	Since   string
	Sunset  time.Time
	Message string
}

// Docs is human documentation carried into exported contracts.
type Docs struct {
	Summary     string
	Description string
	Tags        []string
}

// Descriptor is the transport-neutral description of one operation. Routing,
// contracts, docs, authorization and limits are all derived from it.
type Descriptor struct {
	ID         string // stable and unique per app, e.g. "orders.create"
	Version    string
	Deprecated *Deprecation
	Kind       Kind
	Bindings   []Binding
	Input      TypeInfo
	Output     TypeInfo
	Errors     []string // error catalog codes the operation can return
	Security   Security
	Policies   Policies
	// Consistency is the default read consistency of the operation's
	// reads, such as data.ReadYourWrites; nil leaves it to the data
	// module's config. A read that asks for a level itself wins over it.
	Consistency ReadConsistency
	Docs        Docs
	Meta        map[string]any // extension point for adapters

	// Adapt turns a handler into a Handler without reflection. op.Unary sets
	// it; generated or hand-written descriptors set it with Adapter.
	Adapt func(h any) (Handler, error)
	// Shape and Meaning are the validators for stages 7 and 9, if any.
	Shape   func(ctx context.Context, in any) *ValidationError
	Meaning func(ctx context.Context, in any) *ValidationError
	// Generated means the codecs and validators were generated, so the
	// kernel adds none of its built-in ones (validate tags, Validate methods).
	Generated bool
}

// Definition is anything that yields a descriptor: a Descriptor itself, or a
// builder from Unary.
type Definition interface{ Describe() Descriptor }

// Describe returns d, so a Descriptor is its own Definition.
func (d Descriptor) Describe() Descriptor { return d }

// Handler is an adapted operation handler: it takes the decoded *In and
// returns the *Out.
type Handler func(ctx context.Context, in any) (any, error)

// Adapter returns an Adapt function for handlers of type
// func(context.Context, *In) (*Out, error).
func Adapter[In, Out any]() func(h any) (Handler, error) {
	return func(h any) (Handler, error) {
		f, ok := h.(func(context.Context, *In) (*Out, error))
		if !ok {
			return nil, fmt.Errorf("handler is %T, want func(context.Context, *%s) (*%s, error)", h, reflect.TypeFor[In](), reflect.TypeFor[Out]())
		}
		if f == nil {
			return nil, fmt.Errorf("handler is a nil %T", h)
		}
		return func(ctx context.Context, in any) (any, error) {
			out, err := f(ctx, in.(*In))
			if err != nil || out == nil {
				return nil, err // a nil *Out comes back as a plain nil
			}
			return out, nil
		}, nil
	}
}

// TypeOf returns the TypeInfo for T.
func TypeOf[T any]() TypeInfo {
	return TypeInfo{Type: reflect.TypeFor[T](), New: func() any { return new(T) }}
}

var (
	idPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	methodPattern = regexp.MustCompile(`^[A-Z]+$`)
	rpcService    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	rpcMethod     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Check reports what is wrong with d on its own, before it is compared with
// other operations or a handler.
func (d *Descriptor) Check() []error {
	var errs []error
	if !idPattern.MatchString(d.ID) {
		errs = append(errs, fmt.Errorf("operation ID %q must be dotted lower snake case, such as \"orders.create\"", d.ID))
	}
	if d.Kind != KindUnary {
		errs = append(errs, fmt.Errorf("operation %q: %s operations are not supported yet", d.ID, d.Kind))
	}
	if d.Input.Type == nil || d.Input.New == nil || d.Output.Type == nil || d.Output.New == nil {
		errs = append(errs, fmt.Errorf("operation %q: input and output types must be set (use op.TypeOf)", d.ID))
	}
	if d.Input.Type != nil && d.Input.Type.Kind() != reflect.Struct {
		errs = append(errs, fmt.Errorf("operation %q: the input type must be a struct, not %v", d.ID, d.Input.Type))
	}
	if d.Adapt == nil {
		errs = append(errs, fmt.Errorf("operation %q has no handler adapter: build it with op.Unary, or set Adapt to op.Adapter[In, Out]()", d.ID))
	}
	if v, ok := d.Consistency.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("operation %q: its read consistency: %w", d.ID, err))
		}
	}
	s := d.Security
	switch {
	case !s.Public && s.Action == "":
		errs = append(errs, fmt.Errorf("operation %q declares no security: call Public(), Authorize(action) or AuthorizeResource(action, resource)", d.ID))
	case s.Public && (s.Action != "" || s.Resource != nil || len(s.Contexts) > 0):
		errs = append(errs, fmt.Errorf("operation %q is Public but also declares authorization or contexts", d.ID))
	}
	for _, b := range d.Bindings {
		switch b := b.(type) {
		case HTTP:
			if !methodPattern.MatchString(b.Method) || len(b.Path) == 0 || b.Path[0] != '/' {
				errs = append(errs, fmt.Errorf("operation %q: HTTP binding needs an upper-case method and a path starting with /, got %q %q", d.ID, b.Method, b.Path))
			}
		case RPC:
			if !rpcService.MatchString(b.Service) || !rpcMethod.MatchString(b.Method) {
				errs = append(errs, fmt.Errorf("operation %q: RPC binding needs a protobuf service name, such as orders.v1.Orders, and a method name, got %q %q", d.ID, b.Service, b.Method))
			}
		case Topic:
			if b.Name == "" {
				errs = append(errs, fmt.Errorf("operation %q: topic binding needs a name", d.ID))
			}
		case nil:
			errs = append(errs, fmt.Errorf("operation %q has a nil binding", d.ID))
		default:
			errs = append(errs, fmt.Errorf("operation %q: binding of type %T is not supported; use op.HTTP, op.RPC or op.Topic values", d.ID, b))
		}
	}
	if rl := d.Policies.RateLimit; rl != nil && (rl.Limit <= 0 || rl.Window <= 0) {
		errs = append(errs, fmt.Errorf("operation %q: rate limit needs a positive limit and window", d.ID))
	}
	return errs
}
