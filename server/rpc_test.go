package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/server"
)

type GreetIn struct {
	Name string `json:"name" validate:"required"`
}

type GreetOut struct {
	Greeting string `json:"greeting"`
}

var (
	greetRPC   = op.Unary[GreetIn, GreetOut]("greet.hello").RPC("greet.v1.Greeter", "Hello").HTTP("POST", "/hello").Public().BodyLimit(64)
	greetProto = op.Unary[wrapperspb.StringValue, wrapperspb.StringValue]("greet.proto").RPC("greet.v1.Greeter", "Proto").Public()
	failRPC    = op.Unary[GreetIn, GreetOut]("greet.fail").RPC("greet.v1.Greeter", "Fail").Public().AllowUnknownFields()
	limitedRPC = op.Unary[GreetIn, GreetOut]("greet.limited").RPC("greet.v1.Greeter", "Limited").Public().RateLimit(1, time.Hour)
)

// jsonCodec is a client-side codec for plain Go structs.
type jsonCodec struct{}

func (jsonCodec) Name() string                    { return "json" }
func (jsonCodec) Marshal(v any) ([]byte, error)   { return json.Marshal(v) }
func (jsonCodec) Unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// rawCodec sends bytes as they are, to put malformed or extra fields on the wire.
type rawCodec struct{}

func (rawCodec) Name() string { return "json" }
func (rawCodec) Marshal(v any) ([]byte, error) {
	return []byte(*v.(*string)), nil
}
func (rawCodec) Unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

type rpcEnv struct {
	c    *http.Client
	app  *galleon.App
	url  string
	logs *logBuffer
	tp   *recordingTP
}

func bootRPC(t *testing.T, roles []galleon.Role, extra ...galleon.Module) *rpcEnv {
	t.Helper()
	logs := &logBuffer{}
	tp := &recordingTP{}
	mod := galleon.ModuleFunc("greet", func(r *galleon.Registry) error {
		r.Operation(greetRPC, func(_ context.Context, in *GreetIn) (*GreetOut, error) {
			return &GreetOut{Greeting: "hello " + in.Name}, nil
		})
		r.Operation(greetProto, func(_ context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
			return wrapperspb.String("hello " + in.GetValue()), nil
		})
		r.Operation(failRPC, func(context.Context, *GreetIn) (*GreetOut, error) {
			return nil, errors.New("database password is hunter2")
		})
		r.Operation(limitedRPC, func(context.Context, *GreetIn) (*GreetOut, error) { return &GreetOut{}, nil })
		r.Operation(op.Unary[GreetIn, GreetOut]("greet.panic").RPC("greet.v1.Greeter", "Panic").Public(), func(context.Context, *GreetIn) (*GreetOut, error) {
			panic("boom")
		})
		r.Operation(op.Unary[GreetIn, GreetOut]("greet.down").RPC("greet.v1.Greeter", "Down").Public(), func(context.Context, *GreetIn) (*GreetOut, error) {
			return nil, errs.New(errs.Unavailable, "greet.down", "try later")
		})
		r.Operation(op.Unary[wrapperspb.StringValue, wrapperspb.StringValue]("greet.nothing").RPC("greet.v1.Greeter", "Nothing").Public(), func(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
			return nil, nil
		})
		return nil
	})
	opts := []galleon.Option{
		galleon.Name("greet"),
		galleon.Logger(slog.New(slog.NewTextHandler(logs, nil))),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
		galleon.DrainGrace(0),
		galleon.TracerProvider(tp),
		galleon.ConfigJSON([]byte(`{"http":{"addr":"127.0.0.1:0","trusted_proxies":["127.0.0.0/8"]}}`)),
		galleon.Modules(append([]galleon.Module{mod, server.Module()}, extra...)...),
	}
	if roles != nil {
		opts = append(opts, galleon.Roles(roles...))
	}
	app, err := galleon.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	e := &env{srv: galleon.MustResolve[*server.Server](app)}
	c := h2c()
	t.Cleanup(c.CloseIdleConnections) // runs before the shutdown above
	return &rpcEnv{c: c, app: app, url: "http://" + listening(t, e), logs: logs, tp: tp}
}

// h2c is an HTTP client that speaks HTTP/2 without TLS, as gRPC needs.
func h2c() *http.Client {
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: tr}
}

func greet[In, Out any](e *rpcEnv, method string, in *In, opts ...connect.ClientOption) (*Out, http.Header, error) {
	c := connect.NewClient[In, Out](e.c, e.url+"/greet.v1.Greeter/"+method, opts...)
	res, err := c.CallUnary(context.Background(), connect.NewRequest(in))
	if err != nil {
		return nil, nil, err
	}
	return res.Msg, res.Header(), nil
}

func TestServesRPCOverEveryProtocol(t *testing.T) {
	e := bootRPC(t, nil)
	for name, opts := range map[string][]connect.ClientOption{
		"connect":  {connect.WithCodec(jsonCodec{})},
		"grpc":     {connect.WithCodec(jsonCodec{}), connect.WithGRPC()},
		"grpc-web": {connect.WithCodec(jsonCodec{}), connect.WithGRPCWeb()},
	} {
		out, _, err := greet[GreetIn, GreetOut](e, "Hello", &GreetIn{Name: "ann"}, opts...)
		if err != nil || out.Greeting != "hello ann" {
			t.Errorf("%s: got %+v, %v", name, out, err)
		}
	}
}

func TestProtobufMessages(t *testing.T) {
	e := bootRPC(t, nil)
	for name, opts := range map[string][]connect.ClientOption{
		"connect proto": nil,
		"connect json":  {connect.WithProtoJSON()},
		"grpc proto":    {connect.WithGRPC()},
		"grpc json":     {connect.WithGRPC(), connect.WithProtoJSON()},
	} {
		out, _, err := greet[wrapperspb.StringValue, wrapperspb.StringValue](e, "Proto", wrapperspb.String("bo"), opts...)
		if err != nil || out.GetValue() != "hello bo" {
			t.Errorf("%s: got %v, %v", name, out, err)
		}
	}
}

func TestAFormatTheOperationDoesntSpeakIsRefused(t *testing.T) {
	e := bootRPC(t, nil)
	// Plain structs have no protobuf encoding; send a protobuf body anyway.
	_, _, err := greet[wrapperspb.StringValue, wrapperspb.StringValue](e, "Hello", wrapperspb.String("x"), connect.WithGRPC())
	if connect.CodeOf(err) != connect.CodeInvalidArgument || errorCode(err) != "galleon.unsupported_media_type" {
		t.Fatalf("got %v (%s)", err, errorCode(err))
	}
}

func errorCode(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Meta().Get(server.ErrorCodeHeader)
	}
	return ""
}

func details(t *testing.T, err error) map[string]any {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("not a connect error: %v", err)
	}
	for _, d := range ce.Details() {
		v, err := d.Value()
		if err != nil {
			t.Fatal(err)
		}
		if st, ok := v.(*structpb.Struct); ok {
			return st.AsMap()
		}
	}
	t.Fatal("no problem detail")
	return nil
}

func TestRPCValidationErrors(t *testing.T) {
	e := bootRPC(t, nil)
	for name, opts := range map[string][]connect.ClientOption{
		"connect": {connect.WithCodec(jsonCodec{})},
		"grpc":    {connect.WithCodec(jsonCodec{}), connect.WithGRPC()},
	} {
		_, _, err := greet[GreetIn, GreetOut](e, "Hello", &GreetIn{}, opts...)
		if connect.CodeOf(err) != connect.CodeInvalidArgument || errorCode(err) != "galleon.invalid_input" {
			t.Fatalf("%s: got %v", name, err)
		}
		d := details(t, err)
		fv, _ := json.Marshal(d["details"])
		if d["code"] != "galleon.invalid_input" || !strings.Contains(string(fv), `"path":"/name"`) || !strings.Contains(string(fv), `"code":"required"`) {
			t.Fatalf("%s: details %v", name, d)
		}
	}
}

func TestRPCInternalErrorsHideTheCause(t *testing.T) {
	e := bootRPC(t, nil)
	_, _, err := greet[GreetIn, GreetOut](e, "Fail", &GreetIn{Name: "x"}, connect.WithCodec(jsonCodec{}), connect.WithGRPC())
	if connect.CodeOf(err) != connect.CodeInternal || strings.Contains(err.Error(), "hunter2") || errorCode(err) != "galleon.internal" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(e.logs.String(), "hunter2") {
		t.Fatal("the cause must be logged")
	}
}

func TestRPCDecodeHonoursPolicies(t *testing.T) {
	e := bootRPC(t, nil)
	raw := func(method, body string) error {
		_, _, err := greet[string, GreetOut](e, method, &body, connect.WithCodec(rawCodec{}))
		return err
	}
	if err := raw("Hello", `{"name":"a","extra":1}`); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown fields are rejected by default: %v", err)
	}
	if err := raw("Fail", `{"name":"a","extra":1}`); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("AllowUnknownFields lets them through to the handler: %v", err)
	}
	if err := raw("Hello", `{"name":"`+strings.Repeat("a", 100)+`"}`); connect.CodeOf(err) != connect.CodeInvalidArgument || errorCode(err) != "galleon.body_too_large" {
		t.Fatalf("the operation's body limit applies, with the same error as over HTTP: %v", err)
	}
	if err := raw("Hello", `{"name":`); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("malformed JSON: %v", err)
	}
}

func TestDecodeWaitsForTheDecodeStage(t *testing.T) {
	deny := galleon.ModuleFunc("deny", func(r *galleon.Registry) error {
		r.Intercept(op.Before(op.StageDecode), func(next op.Invoker) op.Invoker {
			return func(ctx context.Context, call *op.Call) (any, error) {
				if call.Request.Header.Get("X-Deny") != "" {
					return nil, errors.New("stopped before decode")
				}
				return next(ctx, call)
			}
		})
		return nil
	})
	e := bootRPC(t, nil, deny)
	body := `{"name":`
	c := connect.NewClient[string, GreetOut](e.c, e.url+"/greet.v1.Greeter/Hello", connect.WithCodec(rawCodec{}))
	req := connect.NewRequest(&body)
	req.Header().Set("X-Deny", "1")
	_, err := c.CallUnary(context.Background(), req)
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("a malformed body must not fail before the decode stage runs: %v", err)
	}
}

func TestRPCPeerComesFromTrustedProxies(t *testing.T) {
	e := bootRPC(t, nil)
	call := func(xff string) error {
		c := connect.NewClient[GreetIn, GreetOut](e.c, e.url+"/greet.v1.Greeter/Limited", connect.WithCodec(jsonCodec{}), connect.WithGRPC())
		req := connect.NewRequest(&GreetIn{Name: "a"})
		req.Header().Set("X-Forwarded-For", xff)
		_, err := c.CallUnary(context.Background(), req)
		return err
	}
	if call("203.0.113.1") != nil || call("203.0.113.2") != nil {
		t.Fatal("each forwarded client has its own limit")
	}
	if err := call("203.0.113.1"); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("the second call from one client is limited: %v", err)
	}
}

func TestRPCSpans(t *testing.T) {
	e := bootRPC(t, nil)
	if _, _, err := greet[GreetIn, GreetOut](e, "Hello", &GreetIn{Name: "a"}, connect.WithCodec(jsonCodec{}), connect.WithGRPC()); err != nil {
		t.Fatal(err)
	}
	s := e.tp.find("greet.v1.Greeter/Hello")
	if s == nil {
		t.Fatalf("no span named after the method: %v", e.tp.names())
	}
	want := map[attribute.Key]string{
		"rpc.system.name":          "grpc",
		"rpc.method":               "greet.v1.Greeter/Hello",
		"rpc.response.status_code": "OK",
		"galleon.operation":        "greet.hello",
	}
	for k, v := range want {
		if got := s.attr(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestRolesChooseTheBindings(t *testing.T) {
	rpcOnly := bootRPC(t, []galleon.Role{server.RoleRPC})
	if _, _, err := greet[GreetIn, GreetOut](rpcOnly, "Hello", &GreetIn{Name: "a"}, connect.WithCodec(jsonCodec{})); err != nil {
		t.Fatalf("the rpc role serves RPC: %v", err)
	}
	if res, err := rpcOnly.c.Post(rpcOnly.url+"/hello", "application/json", strings.NewReader(`{"name":"a"}`)); err != nil || res.StatusCode != 404 {
		t.Fatalf("the rpc role doesn't serve HTTP bindings: %v %v", res, err)
	}
	webOnly := bootRPC(t, []galleon.Role{server.RoleWeb})
	if _, _, err := greet[GreetIn, GreetOut](webOnly, "Hello", &GreetIn{Name: "a"}, connect.WithCodec(jsonCodec{})); connect.CodeOf(err) != connect.CodeNotFound && connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("the web role doesn't serve RPC: %v", err)
	}
	if res, err := webOnly.c.Post(webOnly.url+"/hello", "application/json", strings.NewReader(`{"name":"a"}`)); err != nil || res.StatusCode != 200 {
		t.Fatalf("the web role serves HTTP: %v %v", res, err)
	}
}

func TestAnHTTPBindingOnAProcedurePathFailsBoot(t *testing.T) {
	clash := galleon.ModuleFunc("clash", func(r *galleon.Registry) error {
		r.Operation(op.Unary[GreetIn, GreetOut]("greet.clash").HTTP("POST", "/greet.v1.Greeter/Hello").Public(), func(context.Context, *GreetIn) (*GreetOut, error) { return nil, nil })
		return nil
	})
	app, err := galleon.New(
		galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode),
		galleon.Logger(slog.New(slog.DiscardHandler)),
		galleon.Modules(galleon.ModuleFunc("greet", func(r *galleon.Registry) error {
			r.Operation(greetRPC, func(context.Context, *GreetIn) (*GreetOut, error) { return nil, nil })
			return nil
		}), clash, server.Module()),
	)
	if err == nil {
		err = app.Start(context.Background())
		defer app.Shutdown(context.Background())
	}
	if err == nil || !strings.Contains(err.Error(), "the path of an RPC procedure") {
		t.Fatalf("got %v", err)
	}
}

// recordingTP records span names and attributes.
type recordingTP struct {
	embedded.TracerProvider
	mu    sync.Mutex
	spans []*recSpan
}

func (p *recordingTP) Tracer(string, ...trace.TracerOption) trace.Tracer { return recTracer{p: p} }

func (p *recordingTP) find(name string) *recSpan {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.spans {
		if s.name() == name {
			return s
		}
	}
	return nil
}

func (p *recordingTP) names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, s := range p.spans {
		out = append(out, s.name())
	}
	return out
}

type recTracer struct {
	embedded.Tracer
	p *recordingTP
}

func (t recTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	s := &recSpan{n: name, attrs: map[attribute.Key]string{}}
	cfg := trace.NewSpanStartConfig(opts...)
	for _, kv := range cfg.Attributes() {
		s.attrs[kv.Key] = kv.Value.Emit()
	}
	t.p.mu.Lock()
	t.p.spans = append(t.p.spans, s)
	t.p.mu.Unlock()
	return trace.ContextWithSpan(ctx, s), s
}

type recSpan struct {
	noop.Span
	mu    sync.Mutex
	n     string
	attrs map[attribute.Key]string
}

func (s *recSpan) SetName(n string) { s.mu.Lock(); s.n = n; s.mu.Unlock() }
func (s *recSpan) SetAttributes(kv ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range kv {
		s.attrs[a.Key] = a.Value.Emit()
	}
}
func (s *recSpan) IsRecording() bool { return true }
func (s *recSpan) name() string      { s.mu.Lock(); defer s.mu.Unlock(); return s.n }
func (s *recSpan) attr(k attribute.Key) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attrs[k]
}

func TestRPCPanicsAreInternalErrors(t *testing.T) {
	e := bootRPC(t, nil)
	for name, opts := range map[string][]connect.ClientOption{
		"connect":  {connect.WithCodec(jsonCodec{})},
		"grpc":     {connect.WithCodec(jsonCodec{}), connect.WithGRPC()},
		"grpc-web": {connect.WithCodec(jsonCodec{}), connect.WithGRPCWeb()},
	} {
		_, _, err := greet[GreetIn, GreetOut](e, "Panic", &GreetIn{Name: "a"}, opts...)
		if connect.CodeOf(err) != connect.CodeInternal || errorCode(err) != "galleon.internal" {
			t.Errorf("%s: got %v (%s)", name, err, errorCode(err))
		}
	}
	if !strings.Contains(e.logs.String(), "panic while serving an rpc") {
		t.Fatal("the panic must be logged")
	}
}

func TestBinaryProtobufRejectsUnknownFields(t *testing.T) {
	e := bootRPC(t, nil)
	// Field 1 is the value; field 99 is unknown to StringValue.
	body := string([]byte{0x0a, 0x02, 'b', 'o', 0xf8, 0x06, 0x01})
	c := connect.NewClient[string, wrapperspb.StringValue](e.c, e.url+"/greet.v1.Greeter/Proto", connect.WithCodec(rawProto{}), connect.WithGRPC())
	_, err := c.CallUnary(context.Background(), connect.NewRequest(&body))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("got %v", err)
	}
}

type rawProto struct{}

func (rawProto) Name() string                    { return "proto" }
func (rawProto) Marshal(v any) ([]byte, error)   { return []byte(*v.(*string)), nil }
func (rawProto) Unmarshal(b []byte, v any) error { return proto.Unmarshal(b, v.(proto.Message)) }

func TestNilOutputOfAWellKnownType(t *testing.T) {
	e := bootRPC(t, nil)
	for name, opts := range map[string][]connect.ClientOption{
		"proto":     {connect.WithGRPC()},
		"protojson": {connect.WithGRPC(), connect.WithProtoJSON()},
		"connect":   {connect.WithProtoJSON()},
	} {
		out, _, err := greet[wrapperspb.StringValue, wrapperspb.StringValue](e, "Nothing", wrapperspb.String("x"), opts...)
		if err != nil || out.GetValue() != "" {
			t.Errorf("%s: got %v, %v", name, out, err)
		}
	}
}

func TestJSONWithACharset(t *testing.T) {
	e := bootRPC(t, nil)
	res, err := e.c.Post(e.url+"/greet.v1.Greeter/Hello", "application/json; charset=utf-8", strings.NewReader(`{"name":"ann"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out GreetOut
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != 200 || out.Greeting != "hello ann" {
		t.Fatalf("got %d %+v %v", res.StatusCode, out, err)
	}
}

func TestServerErrorsMarkTheSpanOverGRPC(t *testing.T) {
	e := bootRPC(t, nil)
	_, _, err := greet[GreetIn, GreetOut](e, "Down", &GreetIn{Name: "a"}, connect.WithCodec(jsonCodec{}), connect.WithGRPC())
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatal(err)
	}
	s := e.tp.find("greet.v1.Greeter/Down")
	if s == nil || s.attr("error.type") != "UNAVAILABLE" || s.attr("rpc.response.status_code") != "UNAVAILABLE" {
		t.Fatalf("got %v", s)
	}
}

func TestGRPCWebErrorDetails(t *testing.T) {
	e := bootRPC(t, nil)
	_, _, err := greet[GreetIn, GreetOut](e, "Hello", &GreetIn{}, connect.WithCodec(jsonCodec{}), connect.WithGRPCWeb())
	if d := details(t, err); d["code"] != "galleon.invalid_input" {
		t.Fatalf("got %v", d)
	}
}

func TestHTTPCatchAllsLeaveProceduresToRPC(t *testing.T) {
	catchAll := galleon.ModuleFunc("web", func(r *galleon.Registry) error {
		r.Operation(op.Unary[Empty, GreetOut]("web.index").HTTP("GET", "/{path...}").Public(), func(context.Context, *Empty) (*GreetOut, error) {
			return &GreetOut{Greeting: "index"}, nil
		})
		r.Operation(op.Unary[GreetIn, GreetOut]("web.any").HTTP("POST", "/greet.v1.Greeter/{m}").Public(), func(context.Context, *GreetIn) (*GreetOut, error) {
			return &GreetOut{Greeting: "http"}, nil
		})
		return nil
	})
	e := bootRPC(t, nil, catchAll)
	if out, _, err := greet[GreetIn, GreetOut](e, "Hello", &GreetIn{Name: "ann"}, connect.WithCodec(jsonCodec{})); err != nil || out.Greeting != "hello ann" {
		t.Fatalf("the procedure must win at its own path: %+v, %v", out, err)
	}
	res, err := e.c.Post(e.url+"/greet.v1.Greeter/Other", "application/json", strings.NewReader(`{"name":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if b, _ := io.ReadAll(res.Body); !strings.Contains(string(b), `"http"`) {
		t.Fatalf("other paths stay with the HTTP route: %d %s", res.StatusCode, b)
	}
}

type unencodable struct {
	C chan int `json:"c"`
}

func TestAnUnencodableResponseHidesItsCause(t *testing.T) {
	bad := galleon.ModuleFunc("bad", func(r *galleon.Registry) error {
		r.Operation(op.Unary[GreetIn, unencodable]("greet.bad").RPC("greet.v1.Greeter", "Bad").Public(), func(context.Context, *GreetIn) (*unencodable, error) {
			return &unencodable{C: make(chan int)}, nil
		})
		return nil
	})
	e := bootRPC(t, nil, bad)
	_, _, err := greet[GreetIn, GreetOut](e, "Bad", &GreetIn{Name: "ann"}, connect.WithCodec(jsonCodec{}), connect.WithGRPC())
	if connect.CodeOf(err) != connect.CodeInternal || strings.Contains(err.Error(), "chan") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(e.logs.String(), "rpc response could not be encoded") {
		t.Fatalf("the failure must be logged: %s", e.logs)
	}
}

func TestAPanicIsLoggedOnce(t *testing.T) {
	e := bootRPC(t, nil)
	_, _, _ = greet[GreetIn, GreetOut](e, "Panic", &GreetIn{Name: "ann"}, connect.WithCodec(jsonCodec{}))
	if n := strings.Count(e.logs.String(), "level=ERROR"); n != 1 {
		t.Fatalf("logged %d errors: %s", n, e.logs)
	}
}

// upperCodec is a server codec under application/x-upper: the body is the
// name, and the reply is the greeting in capitals.
type upperCodec struct{}

func (upperCodec) MediaType() string { return "application/x-upper" }
func (upperCodec) Encode(w io.Writer, v any) error {
	_, err := io.WriteString(w, strings.ToUpper(v.(*GreetOut).Greeting))
	return err
}
func (upperCodec) Decode(r io.Reader, v any, _ op.UnknownFields) error {
	b, err := io.ReadAll(r)
	v.(*GreetIn).Name = string(b)
	return err
}

type rawName struct{}

func (rawName) Name() string                    { return "x-upper" }
func (rawName) Marshal(v any) ([]byte, error)   { return []byte(*v.(*string)), nil }
func (rawName) Unmarshal(b []byte, v any) error { *v.(*string) = string(b); return nil }

func TestServerCodecsServeRPC(t *testing.T) {
	app, err := galleon.New(
		galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode),
		galleon.Logger(slog.New(slog.DiscardHandler)), galleon.DrainGrace(0),
		galleon.ConfigJSON([]byte(`{"http":{"addr":"127.0.0.1:0"}}`)),
		galleon.Modules(galleon.ModuleFunc("greet", func(r *galleon.Registry) error {
			r.Operation(greetRPC, func(_ context.Context, in *GreetIn) (*GreetOut, error) {
				return &GreetOut{Greeting: "hello " + in.Name}, nil
			})
			return nil
		}), server.Module(server.Codecs(upperCodec{}))),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	url := "http://" + listening(t, &env{srv: galleon.MustResolve[*server.Server](app)})
	hc := h2c()
	defer hc.CloseIdleConnections()
	c := connect.NewClient[string, string](hc, url+"/greet.v1.Greeter/Hello", connect.WithCodec(rawName{}), connect.WithGRPC())
	name := "ann"
	res, err := c.CallUnary(context.Background(), connect.NewRequest(&name))
	if err != nil || *res.Msg != "HELLO ANN" {
		t.Fatalf("got %v, %v", res, err)
	}
}
