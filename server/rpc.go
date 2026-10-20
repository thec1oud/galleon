package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"runtime/debug"
	"strings"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/codec"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

// ErrorCodeHeader is the RPC response metadata key carrying an error's Galleon
// code, such as "galleon.invalid_input".
const ErrorCodeHeader = "Galleon-Error-Code"

// message carries an RPC payload through connect-go. The codecs below don't
// decode on the way in: Unmarshal keeps the bytes, so the pipeline decodes them
// at its decode stage, after pre-authorization, as it does for HTTP.
type message struct {
	decode func(into any) error // set by a codec's Unmarshal
	out    any                  // set by the handler, encoded by a codec's Marshal
}

// rpcCodec is a connect-go codec for one operation and one payload format.
type rpcCodec struct {
	name  string
	limit int64 // the operation's body limit
	dec   func(data []byte, into any) error
	enc   func(v any) ([]byte, error)
}

func (c *rpcCodec) Name() string { return c.name }

func (c *rpcCodec) Unmarshal(data []byte, m any) error {
	msg, ok := m.(*message)
	if !ok {
		return c.protocolMessage(m, func(pm proto.Message) error {
			if c.isJSON() {
				return protojson.Unmarshal(data, pm)
			}
			return proto.Unmarshal(data, pm)
		})
	}
	if int64(len(data)) > c.limit {
		err := errs.New(errs.InvalidArgument, "galleon.body_too_large", "the request body is too large",
			errs.FieldViolations{{Path: "", Code: "body_too_large", Params: map[string]any{"limit": c.limit}, MsgKey: "validation.body_too_large"}})
		msg.decode = func(any) error { return err }
		return nil
	}
	b := bytes.Clone(data) // connect-go reuses its buffers
	msg.decode = func(into any) error { return c.dec(b, into) }
	return nil
}

func (c *rpcCodec) Marshal(m any) ([]byte, error) {
	msg, ok := m.(*message)
	if !ok {
		var out []byte
		err := c.protocolMessage(m, func(pm proto.Message) (err error) {
			if c.isJSON() {
				out, err = protojson.Marshal(pm)
			} else {
				out, err = proto.Marshal(pm)
			}
			return err
		})
		return out, err
	}
	return c.enc(msg.out)
}

func (c *rpcCodec) isJSON() bool { return strings.HasPrefix(c.name, "json") }

// protocolMessage handles the protocol's own messages, such as the gRPC status
// connect-go encodes with the "proto" codec, whatever the operation speaks.
func (c *rpcCodec) protocolMessage(m any, fn func(proto.Message) error) error {
	pm, ok := m.(proto.Message)
	if !ok {
		return fmt.Errorf("rpc: unexpected message type %T", m)
	}
	return fn(pm)
}

var protoMessage = reflect.TypeFor[proto.Message]()

// rpcCodecs lists the payload formats an operation speaks over RPC: each codec
// both sets have under an "application/<name>" media type, by that name, plus
// protobuf when the input and output are protobuf messages, in which case JSON
// is protojson. Formats connect-go knows but the operation doesn't speak are
// replaced by codecs that refuse them at the decode stage.
func rpcCodecs(d *op.Descriptor, in, out *codec.Set, limit int64) []connect.Codec {
	unknown := d.Policies.UnknownFields
	inProto := reflect.PointerTo(d.Input.Type).Implements(protoMessage)
	outProto := reflect.PointerTo(d.Output.Type).Implements(protoMessage)
	var cs []connect.Codec
	speaks := map[string]bool{}
	for _, mt := range in.MediaTypes() {
		name, ok := strings.CutPrefix(mt, "application/")
		if !ok || name == "" || strings.ContainsAny(name, " ;") {
			continue
		}
		dc, ec := in.ForContentType(mt), out.ForContentType(mt)
		if ec == nil || ec.MediaType() != mt {
			continue
		}
		c := &rpcCodec{
			name: name,
			dec:  func(data []byte, into any) error { return dc.Decode(bytes.NewReader(data), into, unknown) },
			enc: func(v any) ([]byte, error) {
				if v == nil {
					v = struct{}{}
				}
				return codec.Marshal(ec, v)
			},
		}
		if name == "json" {
			if inProto {
				opts := protojson.UnmarshalOptions{DiscardUnknown: unknown == op.IgnoreUnknown}
				c.dec = func(data []byte, into any) error {
					if len(data) == 0 {
						return nil
					}
					return protoError(opts.Unmarshal(data, into.(proto.Message)))
				}
			}
			if outProto {
				c.enc = func(v any) ([]byte, error) {
					if v == nil {
						// Well-known types such as wrappers aren't JSON
						// objects, so encode the type's zero message.
						v = d.Output.New()
					}
					return protojson.Marshal(v.(proto.Message))
				}
			}
		}
		speaks[name] = true
		cs = append(cs, c)
	}
	if inProto && outProto {
		speaks["proto"] = true
		cs = append(cs, &rpcCodec{
			name: "proto",
			dec: func(data []byte, into any) error {
				m := into.(proto.Message)
				if err := (proto.UnmarshalOptions{DiscardUnknown: unknown == op.IgnoreUnknown}).Unmarshal(data, m); err != nil {
					return protoError(err)
				}
				if unknown == op.RejectUnknown && hasUnknown(m.ProtoReflect()) {
					return errs.New(errs.InvalidArgument, "galleon.invalid_body", "the body has a field the operation doesn't accept",
						errs.FieldViolations{{Path: "", Code: "unknown_field", MsgKey: "validation.unknown_field"}})
				}
				return nil
			},
			enc: func(v any) ([]byte, error) {
				if v == nil {
					return nil, nil
				}
				return proto.Marshal(v.(proto.Message))
			},
		})
	}
	if speaks["json"] {
		// Connect clients may send "application/json; charset=utf-8".
		for _, c := range cs {
			if c.Name() == "json" {
				j := *c.(*rpcCodec)
				j.name = "json; charset=utf-8"
				cs = append(cs, &j)
				speaks[j.name] = true
				break
			}
		}
	}
	for _, name := range []string{"proto", "json", "json; charset=utf-8"} {
		if !speaks[name] {
			cs = append(cs, refuse(name))
		}
	}
	for _, c := range cs {
		c.(*rpcCodec).limit = limit
	}
	return cs
}

// refuse is a codec for a format connect-go accepts by default but the
// operation doesn't speak. Decoding fails with InvalidArgument, the same kind
// as HTTP's 415.
func refuse(name string) connect.Codec {
	err := errs.New(errs.InvalidArgument, "galleon.unsupported_media_type", "the operation doesn't accept the "+name+" format")
	return &rpcCodec{
		name: name,
		dec:  func([]byte, any) error { return err },
		enc:  func(any) ([]byte, error) { return nil, err },
	}
}

// hasUnknown reports whether m, or a message inside it, kept fields its type
// doesn't declare.
func hasUnknown(m protoreflect.Message) bool {
	if len(m.GetUnknown()) > 0 {
		return true
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					found = hasUnknown(mv.Message())
					return !found
				})
			}
		case fd.IsList():
			if fd.Message() != nil {
				for i := 0; i < v.List().Len() && !found; i++ {
					found = hasUnknown(v.List().Get(i).Message())
				}
			}
		case fd.Message() != nil:
			found = hasUnknown(v.Message())
		}
		return !found
	})
	return found
}

func protoError(err error) error {
	if err == nil {
		return nil
	}
	return errs.Wrap(err, errs.InvalidArgument, "galleon.invalid_body", "the body could not be read",
		errs.FieldViolations{{Path: "", Code: "syntax", MsgKey: "validation.syntax"}})
}

// rpcHandler builds the connect-go handler for one RPC binding.
func (s *Server) rpcHandler(x *galleon.Operation, b op.RPC, in, out *codec.Set) http.Handler {
	limit := x.Desc.Policies.BodyLimit
	if limit <= 0 {
		limit = s.cfg.BodyLimit
	}
	// connect-go reads the whole message before the handler runs. Its own
	// limit, at twice the operation's, bounds that read, compressed bodies
	// included; the operation's limit itself is checked at the decode stage,
	// after pre-authorization, with the same error as over HTTP.
	opts := []connect.HandlerOption{connect.WithReadMaxBytes(int(min(max(2*limit, limit+64<<10), int64(math.MaxInt))))}
	for _, c := range rpcCodecs(x.Desc, in, out, limit) {
		opts = append(opts, connect.WithCodec(c))
	}
	method := b.Service + "/" + b.Method
	return connect.NewUnaryHandler(procedure(b), func(ctx context.Context, req *connect.Request[message]) (*connect.Response[message], error) {
		span := trace.SpanFromContext(ctx)
		system := semconv.RPCSystemNameConnectrpc
		if p := req.Peer().Protocol; p == connect.ProtocolGRPC || p == connect.ProtocolGRPCWeb {
			system = semconv.RPCSystemNameGRPC
		}
		span.SetName(method)
		span.SetAttributes(system, semconv.RPCMethod(method), attribute.String("galleon.operation", x.Desc.ID))

		call := &op.Call{Request: &op.Request{
			Transport: "rpc",
			Binding:   b,
			Header:    req.Header(),
			Peer:      peerFrom(ctx),
			Decode: func(into any) error {
				if req.Msg.decode == nil {
					return nil
				}
				return req.Msg.decode(into)
			},
		}}
		res, err := s.invokeRPC(ctx, x, call, method)
		if err != nil {
			ce := s.rpcError(ctx, method, err)
			code := strings.ToUpper(ce.Code().String())
			span.SetAttributes(semconv.RPCResponseStatusCode(code))
			if serverFault(ce.Code()) {
				// gRPC answers HTTP 200 even for errors, so the outer
				// middleware can't tell; mark the span here.
				span.SetAttributes(semconv.ErrorTypeKey.String(code))
				span.SetStatus(codes.Error, code)
			}
			return nil, ce
		}
		span.SetAttributes(semconv.RPCResponseStatusCode("OK"))
		return connect.NewResponse(&message{out: res}), nil
	}, opts...)
}

// invokeRPC runs the pipeline, turning a panic into an Internal error: an RPC
// client must get an RPC error, not the HTTP 500 the outer middleware writes.
func (s *Server) invokeRPC(ctx context.Context, x *galleon.Operation, call *op.Call, method string) (out any, err error) {
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				panic(p)
			}
			s.log.ErrorContext(ctx, "panic while serving an rpc", "method", method, "panic", p, "stack", string(debug.Stack()))
			out, err = nil, errs.New(errs.Internal, "galleon.panic", "")
		}
	}()
	return x.Invoke(ctx, call)
}

// serverFault reports whether a code is the server's failure, as the RPC
// semantic conventions count them.
func serverFault(c connect.Code) bool {
	switch c {
	case connect.CodeUnknown, connect.CodeDeadlineExceeded, connect.CodeUnimplemented,
		connect.CodeInternal, connect.CodeUnavailable, connect.CodeDataLoss:
		return true
	}
	return false
}

func procedure(b op.RPC) string { return "/" + b.Service + "/" + b.Method }

// rpcError renders err as a connect-go error with the gRPC code of its kind,
// the Galleon code in metadata and the problem details as a
// google.protobuf.Struct detail. An Internal error is logged and shows clients
// nothing but its kind.
func (s *Server) rpcError(ctx context.Context, method string, err error) *connect.Error {
	e := errs.From(err)
	span := trace.SpanFromContext(ctx)
	traceID := ""
	if sc := span.SpanContext(); sc.HasTraceID() {
		traceID = sc.TraceID().String()
	}
	if e.Kind == errs.Internal {
		s.log.ErrorContext(ctx, "rpc failed", "method", method, "error", errorText(e))
		span.SetStatus(codes.Error, e.Code)
	}
	p := errs.ToProblem(e, traceID)
	ce := connect.NewError(connect.Code(e.Kind.GRPCCode()), errors.New(p.Title))
	ce.Meta().Set(ErrorCodeHeader, p.Code)
	fields := map[string]any{"code": p.Code}
	if traceID != "" {
		fields["instance"] = traceID
	}
	if len(p.Details) > 0 {
		var details map[string]any
		if b, err := json.Marshal(p.Details); err == nil && json.Unmarshal(b, &details) == nil {
			fields["details"] = details
		}
	}
	if st, err := structpb.NewStruct(fields); err == nil {
		if d, err := connect.NewErrorDetail(st); err == nil {
			ce.AddDetail(d)
		}
	}
	return ce
}
