package security_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/security"
)

// headerAuth authenticates the X-User header as a user, with the roles in
// X-Roles; "bad" doesn't verify.
type headerAuth struct{ scheme string }

func (a headerAuth) Scheme() string { return a.scheme }

func (a headerAuth) Challenge() string { return "Test realm=" + a.scheme }

func (a headerAuth) Authenticate(_ context.Context, r *op.Request) (security.Principal, error) {
	user := r.Header.Get("X-User-" + a.scheme)
	switch user {
	case "":
		return nil, security.ErrNoCredentials
	case "bad":
		return nil, security.InvalidCredentials("", errors.New("bad user"))
	case "broken":
		return nil, errors.New("the user store is down")
	}
	claims := map[string]any{}
	if roles := r.Header.Get("X-Roles"); roles != "" {
		claims["roles"] = strings.Split(roles, ",")
	}
	if org := r.Header.Get("X-Org"); org != "" {
		claims["org_id"] = org
	}
	return security.Identity{Subject: user, Kind: security.KindUser, Issuer: a.scheme, Claims: security.NewValues(claims)}.Principal(), nil
}

type NoteIn struct {
	ID   string `path:"id" json:"-"`
	Text string `json:"text"`
}
type NoteOut struct {
	Subject, Context string
}

func noteHandler(ctx context.Context, in *NoteIn) (*NoteOut, error) {
	out := &NoteOut{Subject: security.Caller(ctx).Subject()}
	if c := security.Context(ctx); c != nil {
		out.Context = c.Domain() + ":" + c.ID()
	}
	return out, nil
}

var (
	readNote   = op.Unary[NoteIn, NoteOut]("notes.read").HTTP("GET", "/notes/{id}").AuthorizeResource("notes:read", op.Resource("note", "id"))
	listNotes  = op.Unary[NoteIn, NoteOut]("notes.list").HTTP("GET", "/notes").Authorize("notes:list")
	publicNote = op.Unary[NoteIn, NoteOut]("notes.public").HTTP("GET", "/public").Public()
)

var roles = &security.RoleTable{Grants: map[string][]string{
	"reader": {"notes:*"},
	"lister": {"notes:list"},
}}

func notesModule(defs ...op.Definition) galleon.Module {
	return galleon.ModuleFunc("notes", func(r *galleon.Registry) error {
		for _, d := range defs {
			r.Operation(d, noteHandler)
		}
		return nil
	})
}

func newApp(t *testing.T, opts ...galleon.Option) (*galleon.App, error) {
	t.Helper()
	app, err := galleon.New(append([]galleon.Option{
		galleon.Name("test"),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
		galleon.DrainGrace(0),
	}, opts...)...)
	if err != nil {
		return nil, err
	}
	if err := app.Start(context.Background()); err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	return app, nil
}

func boot(t *testing.T, sec galleon.Module, defs ...op.Definition) *galleon.Operations {
	t.Helper()
	app, err := newApp(t, galleon.Modules(sec, notesModule(defs...)))
	if err != nil {
		t.Fatal(err)
	}
	return galleon.MustResolve[*galleon.Operations](app)
}

func bootFails(t *testing.T, want string, sec galleon.Module, defs ...op.Definition) {
	t.Helper()
	_, err := galleon.New(galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode), galleon.Modules(sec, notesModule(defs...)))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want a boot error containing %q, got %v", want, err)
	}
}

// call invokes id over a fake HTTP request with headers h, and returns the
// output, the error and the response headers.
func call(ops *galleon.Operations, id string, in *NoteIn, h map[string]string) (*NoteOut, error, http.Header) {
	hdr := http.Header{}
	for k, v := range h {
		hdr.Set(k, v)
	}
	c := &op.Call{Input: in, Request: &op.Request{Transport: "http", Header: hdr, Peer: "192.0.2.1"}, Response: &op.Response{Header: http.Header{}}}
	out, err := ops.Lookup(id).Invoke(context.Background(), c)
	if out == nil {
		return nil, err, c.Response.Header
	}
	return out.(*NoteOut), err, c.Response.Header
}

func wantCode(t *testing.T, err error, kind errs.Kind, code string) {
	t.Helper()
	e := errs.From(err)
	if err == nil || e.Kind != kind || e.Code != code {
		t.Fatalf("want %v %s, got %v", kind, code, err)
	}
}

func TestDefaultDeny(t *testing.T) {
	ops := boot(t, security.Module(security.Authenticate(headerAuth{"test"}), security.Decide(roles)), readNote, listNotes, publicNote)
	in := &NoteIn{ID: "n1"}

	if out, err, _ := call(ops, "notes.public", in, nil); err != nil || out.Subject != "" {
		t.Fatalf("public, anonymous: %+v, %v", out, err)
	}
	_, err, h := call(ops, "notes.list", in, nil)
	wantCode(t, err, errs.Unauthenticated, security.CodeUnauthenticated)
	if got := h.Get("WWW-Authenticate"); got != "Test realm=test" {
		t.Fatalf("challenge: %q", got)
	}
	_, err, _ = call(ops, "notes.list", in, map[string]string{"X-User-test": "ann"})
	wantCode(t, err, errs.PermissionDenied, security.CodeDenied)
	out, err, _ := call(ops, "notes.list", in, map[string]string{"X-User-test": "ann", "X-Roles": "lister"})
	if err != nil || out.Subject != "ann" || out.Context != "user:ann" {
		t.Fatalf("allowed: %+v, %v", out, err)
	}
	// The lister may list but not read a note.
	_, err, _ = call(ops, "notes.read", in, map[string]string{"X-User-test": "ann", "X-Roles": "lister"})
	wantCode(t, err, errs.PermissionDenied, security.CodeDenied)
	if _, err, _ := call(ops, "notes.read", in, map[string]string{"X-User-test": "ann", "X-Roles": "reader"}); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidCredentialsFailEvenAPublicOperation(t *testing.T) {
	ops := boot(t, security.Module(security.Authenticate(headerAuth{"test"}), security.Decide(roles)), publicNote)
	_, err, h := call(ops, "notes.public", &NoteIn{}, map[string]string{"X-User-test": "bad"})
	wantCode(t, err, errs.Unauthenticated, security.CodeInvalidCredentials)
	if h.Get("WWW-Authenticate") == "" {
		t.Fatal("no challenge")
	}
	_, err, _ = call(ops, "notes.public", &NoteIn{}, map[string]string{"X-User-test": "broken"})
	wantCode(t, err, errs.Unavailable, security.CodeUnavailable)
	if strings.Contains(errs.From(err).Message, "store") {
		t.Fatalf("the cause leaked into the message: %q", errs.From(err).Message)
	}
}

func TestTheChainTriesTheNextSchemeOnlyWithoutCredentials(t *testing.T) {
	ops := boot(t, security.Module(security.Authenticate(headerAuth{"one"}, headerAuth{"two"}), security.Decide(roles)), listNotes)
	out, err, _ := call(ops, "notes.list", &NoteIn{}, map[string]string{"X-User-two": "bob", "X-Roles": "lister"})
	if err != nil || out.Subject != "bob" {
		t.Fatalf("second scheme: %+v, %v", out, err)
	}
	_, err, _ = call(ops, "notes.list", &NoteIn{}, map[string]string{"X-User-one": "bad", "X-User-two": "bob", "X-Roles": "lister"})
	wantCode(t, err, errs.Unauthenticated, security.CodeInvalidCredentials)
}

func TestAnOperationAcceptsOnlyTheSchemesItNames(t *testing.T) {
	onlyTwo := op.Unary[NoteIn, NoteOut]("notes.two").Schemes("two").Authorize("notes:list")
	ops := boot(t, security.Module(security.Authenticate(headerAuth{"one"}, headerAuth{"two"}), security.Decide(roles)), onlyTwo)
	_, err, h := call(ops, "notes.two", &NoteIn{}, map[string]string{"X-User-one": "ann", "X-Roles": "lister"})
	wantCode(t, err, errs.Unauthenticated, security.CodeUnauthenticated)
	if got := h.Values("WWW-Authenticate"); len(got) != 1 || got[0] != "Test realm=two" {
		t.Fatalf("challenges: %q", got)
	}
	bootFails(t, `accepts the scheme "three", but the security module has no authenticator for it (it has one, two)`,
		security.Module(security.Authenticate(headerAuth{"one"}, headerAuth{"two"}), security.Decide(roles)),
		op.Unary[NoteIn, NoteOut]("notes.three").Schemes("three").Authorize("notes:list"))
}

func TestBootCatchesDeclarationMistakes(t *testing.T) {
	auth := security.Authenticate(headerAuth{"test"})
	bootFails(t, `requires permission for "notes:list", but the security module has no decider`, security.Module(auth), listNotes)
	bootFails(t, "accepts only the contexts org, but the security module has no context resolver",
		security.Module(auth, security.Decide(roles)), op.Unary[NoteIn, NoteOut]("notes.org").Contexts("org").Authorize("notes:list"))
	bootFails(t, `names the input "billing", but the security module has no context provider of that name`,
		security.Module(auth, security.Decide(roles)), op.Unary[NoteIn, NoteOut]("notes.billing").Inputs("billing").Authorize("notes:list"))
	bootFails(t, `resource ID field "owner" isn't a field of security_test.NoteIn`,
		security.Module(auth, security.Decide(roles)), op.Unary[NoteIn, NoteOut]("notes.owner").AuthorizeResource("notes:read", op.Resource("note", "owner")))
	bootFails(t, `scheme "test" already has an authenticator`, security.Module(auth, auth, security.Decide(roles)), listNotes)
	bootFails(t, `authentication scheme "Test" must be lower case`, security.Module(security.Authenticate(headerAuth{"Test"}), security.Decide(roles)), listNotes)
	bootFails(t, `role "x" grants "notes*"`, security.Module(auth, security.Decide(&security.RoleTable{Grants: map[string][]string{"x": {"notes*"}}})), listNotes)
	bootFails(t, "the decider is already set", security.Module(auth, security.Decide(roles), security.Decide(roles)), listNotes)
	bootFails(t, `context provider name "request" is a built-in provider's`, security.Module(auth, security.Decide(roles), security.Input(namedProvider("request"))), listNotes)
	bootFails(t, "the decider's constructor returns string, which isn't a security.Decider",
		security.Module(auth, security.DecideWith(func() string { return "" })), listNotes)
}

type namedProvider string

func (p namedProvider) Name() string { return string(p) }
func (p namedProvider) Provide(context.Context, security.Principal, *op.Request) (security.Attributes, error) {
	return security.NewValues(map[string]any{"plan": "pro"}), nil
}

func TestTheResourceCheckGetsTheInputsID(t *testing.T) {
	var got []security.Query
	var mu sync.Mutex
	d := security.DeciderFunc(func(_ context.Context, q security.Query) (security.Decision, error) {
		mu.Lock()
		got = append(got, q)
		mu.Unlock()
		return security.Allow("ok"), nil
	})
	ops := boot(t, security.Module(security.Authenticate(headerAuth{"test"}), security.Decide(d)), readNote)
	if _, err, _ := call(ops, "notes.read", &NoteIn{ID: "n7"}, map[string]string{"X-User-test": "ann"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Resource != nil || got[0].Action != "notes:read" ||
		got[1].Resource == nil || got[1].Resource.Type != "note" || got[1].Resource.ID != "n7" || got[1].Subject.Subject() != "ann" {
		t.Fatalf("queries: %+v", got)
	}
	_, err, _ := call(ops, "notes.read", &NoteIn{}, map[string]string{"X-User-test": "ann"})
	wantCode(t, err, errs.InvalidArgument, "galleon.invalid_input")
}

func TestAnIntegerResourceID(t *testing.T) {
	type In struct {
		Number int64 `json:"number"`
	}
	d := op.Unary[In, NoteOut]("notes.number").AuthorizeResource("notes:read", op.Resource("note", "number"))
	var id string
	app, err := newApp(t, galleon.Modules(
		security.Module(security.Authenticate(headerAuth{"test"}), security.Decide(security.DeciderFunc(func(_ context.Context, q security.Query) (security.Decision, error) {
			if q.Resource != nil {
				id = q.Resource.ID
			}
			return security.Allow(""), nil
		}))),
		galleon.ModuleFunc("n", func(r *galleon.Registry) error {
			r.Operation(d, func(context.Context, *In) (*NoteOut, error) { return &NoteOut{}, nil })
			return nil
		})))
	if err != nil {
		t.Fatal(err)
	}
	ctx := security.WithPrincipal(context.Background(), security.Identity{Subject: "ann", Kind: security.KindUser}.Principal())
	if _, err := galleon.MustResolve[*galleon.Operations](app).Call(ctx, "notes.number", &In{Number: 42}); err != nil || id != "42" {
		t.Fatalf("id %q, %v", id, err)
	}
}

func TestContexts(t *testing.T) {
	orgOnly := op.Unary[NoteIn, NoteOut]("notes.org").Contexts("org").Authorize("notes:list")
	ops := boot(t, security.Module(
		security.Authenticate(headerAuth{"test"}),
		security.ResolveContext(security.ClaimResolver{Domain: "org", Claim: "org_id"}),
		security.Decide(roles)), orgOnly)
	out, err, _ := call(ops, "notes.org", &NoteIn{}, map[string]string{"X-User-test": "ann", "X-Roles": "lister", "X-Org": "acme"})
	if err != nil || out.Context != "org:acme" {
		t.Fatalf("in an org: %+v, %v", out, err)
	}
	_, err, _ = call(ops, "notes.org", &NoteIn{}, map[string]string{"X-User-test": "ann", "X-Roles": "lister"})
	wantCode(t, err, errs.PermissionDenied, security.CodeContextNotAccepted)
	_, err, _ = call(ops, "notes.org", &NoteIn{}, nil)
	wantCode(t, err, errs.Unauthenticated, security.CodeUnauthenticated)
}

func TestAnExpiredContextFails(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	resolver := security.ResolverFunc(func(context.Context, security.Principal, *op.Request, security.Inputs) (security.OperatingContext, error) {
		return security.ContextData{Domain: "org", ID: "acme", ExpiresAt: now}.Context(), nil
	})
	app, err := newApp(t, galleon.Clock(clock.NewFake(now)), galleon.Modules(
		security.Module(security.Authenticate(headerAuth{"test"}), security.ResolveContext(resolver), security.Decide(roles)),
		notesModule(listNotes)))
	if err != nil {
		t.Fatal(err)
	}
	_, err, _ = call(galleon.MustResolve[*galleon.Operations](app), "notes.list", &NoteIn{}, map[string]string{"X-User-test": "ann", "X-Roles": "lister"})
	wantCode(t, err, errs.Unauthenticated, security.CodeContextExpired)
}

func TestTheResolverSeesOnlyTheInputsItsOperationNames(t *testing.T) {
	var names []string
	var attrs security.Attributes
	resolver := security.ResolverFunc(func(ctx context.Context, p security.Principal, _ *op.Request, in security.Inputs) (security.OperatingContext, error) {
		names = in.Names()
		var err error
		attrs, err = security.Gather(ctx, in)
		if _, err := in.Get(ctx, "time"); err == nil {
			t.Error("an input the operation doesn't name was given")
		}
		return nil, err
	})
	withInputs := op.Unary[NoteIn, NoteOut]("notes.inputs").Inputs("request", "billing").Authorize("notes:list")
	ops := boot(t, security.Module(security.Authenticate(headerAuth{"test"}), security.Input(namedProvider("billing")),
		security.ResolveContext(resolver), security.Decide(roles)), withInputs)
	if _, err, _ := call(ops, "notes.inputs", &NoteIn{}, map[string]string{"X-User-test": "ann", "X-Roles": "lister", "User-Agent": "curl"}); err != nil {
		t.Fatal(err)
	}
	ip, _ := attrs.String("request.ip")
	ua, _ := attrs.String("request.user_agent")
	plan, _ := attrs.String("billing.plan")
	if len(names) != 2 || ip != "192.0.2.1" || ua != "curl" || plan != "pro" {
		t.Fatalf("names %v, attributes %v", names, attrs.Names())
	}
}

func TestAnEngineErrorDenies(t *testing.T) {
	d := security.DeciderFunc(func(context.Context, security.Query) (security.Decision, error) {
		return security.Decision{Allow: true}, errors.New("policy store down")
	})
	ops := boot(t, security.Module(security.Authenticate(headerAuth{"test"}), security.Decide(d)), listNotes)
	_, err, _ := call(ops, "notes.list", &NoteIn{}, map[string]string{"X-User-test": "ann"})
	wantCode(t, err, errs.Unavailable, security.CodeUnavailable)
}

func TestObligations(t *testing.T) {
	var audited atomic.Int32
	d := security.DeciderFunc(func(_ context.Context, q security.Query) (security.Decision, error) {
		name := "audit"
		if q.Subject.Subject() == "bob" {
			name = "mask"
		}
		return security.Decision{Allow: true, Obligations: []security.Obligation{{Name: name}}}, nil
	})
	ops := boot(t, security.Module(security.Authenticate(headerAuth{"test"}), security.Decide(d),
		security.HandleObligation("audit", func(context.Context, security.Query, security.Obligation) error {
			audited.Add(1)
			return nil
		})), listNotes)
	if _, err, _ := call(ops, "notes.list", &NoteIn{}, map[string]string{"X-User-test": "ann"}); err != nil || audited.Load() != 1 {
		t.Fatalf("audited %d, %v", audited.Load(), err)
	}
	_, err, _ := call(ops, "notes.list", &NoteIn{}, map[string]string{"X-User-test": "bob"})
	wantCode(t, err, errs.PermissionDenied, security.CodeDenied)
}

func TestHandlersAskForDecisionsCachedPerCall(t *testing.T) {
	var checks atomic.Int32
	d := security.DeciderFunc(func(_ context.Context, q security.Query) (security.Decision, error) {
		checks.Add(1)
		if q.Resource != nil && q.Resource.ID == "secret" {
			return security.Deny("secret"), nil
		}
		return security.Allow(""), nil
	})
	type Out struct{ Can []bool }
	var handlerErr error
	ask := op.Unary[NoteIn, Out]("notes.ask").Authorize("notes:list")
	app, err := newApp(t, galleon.Modules(
		security.Module(security.Authenticate(headerAuth{"test"}), security.Decide(d)),
		galleon.ModuleFunc("n", func(r *galleon.Registry) error {
			r.Operation(ask, func(ctx context.Context, in *NoteIn) (*Out, error) {
				// The pre-authorization's decision is cached.
				if ok, err := security.Can(ctx, "notes:list", nil); !ok || err != nil {
					handlerErr = errors.Join(errors.New("cached decision"), err)
				}
				each, err := security.CanEach(ctx, "notes:read", []security.Resource{{Type: "note", ID: "a"}, {Type: "note", ID: "secret"}, {Type: "note", ID: "a"}})
				if err != nil {
					return nil, err
				}
				if again, err := security.Can(ctx, "notes:read", &security.Resource{Type: "note", ID: "secret"}); again || err != nil {
					handlerErr = errors.Join(errors.New("cached denial"), err)
				}
				if err := security.Require(ctx, "notes:read", &security.Resource{Type: "note", ID: "secret"}); errs.From(err).Code != security.CodeDenied {
					handlerErr = errors.Join(errors.New("require"), err)
				}
				return &Out{Can: each}, nil
			})
			return nil
		})))
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{"X-User-Test": {"ann"}}
	out, err := galleon.MustResolve[*galleon.Operations](app).Lookup("notes.ask").Invoke(context.Background(), &op.Call{Input: &NoteIn{}, Request: &op.Request{Transport: "http", Header: hdr}})
	if err != nil || handlerErr != nil {
		t.Fatal(err, handlerErr)
	}
	if got := out.(*Out).Can; len(got) != 3 || !got[0] || got[1] || !got[2] {
		t.Fatalf("CanEach: %v", got)
	}
	// One check for the action; one each for "a" (twice in the batch) and
	// "secret" in CanEach; the rest came from the cache.
	if n := checks.Load(); n != 4 {
		t.Fatalf("%d checks, want 4", n)
	}
	if ok, err := security.Can(context.Background(), "notes:list", nil); ok || errs.From(err).Code != security.CodeNoCall {
		t.Fatalf("outside a call: %v, %v", ok, err)
	}
}

func TestCallsInProcessRunAsTheirCaller(t *testing.T) {
	ops := boot(t, security.Module(security.Authenticate(headerAuth{"test"}), security.Decide(roles)), listNotes)
	if _, err := ops.Call(context.Background(), "notes.list", &NoteIn{}); errs.From(err).Code != security.CodeUnauthenticated {
		t.Fatalf("anonymous in process: %v", err)
	}
	ann := security.Identity{Subject: "ann", Kind: security.KindUser, Claims: security.NewValues(map[string]any{"roles": []string{"lister"}})}.Principal()
	out, err := ops.Call(security.WithPrincipal(context.Background(), ann), "notes.list", &NoteIn{})
	if err != nil || out.(*NoteOut).Subject != "ann" {
		t.Fatalf("as ann: %+v, %v", out, err)
	}
	// Over a network, a principal in the context doesn't count.
	c := &op.Call{Input: &NoteIn{}, Request: &op.Request{Transport: "http", Header: http.Header{}}}
	if _, err := ops.Lookup("notes.list").Invoke(security.WithPrincipal(context.Background(), ann), c); errs.From(err).Code != security.CodeUnauthenticated {
		t.Fatalf("forged over HTTP: %v", err)
	}
}

func TestANestedCallInheritsThePrincipal(t *testing.T) {
	var opsRef *galleon.Operations
	outer := op.Unary[NoteIn, NoteOut]("notes.outer").Authorize("notes:list")
	app, err := newApp(t, galleon.Modules(
		security.Module(security.Authenticate(headerAuth{"test"}), security.Decide(roles)),
		galleon.ModuleFunc("n", func(r *galleon.Registry) error {
			r.Operation(listNotes, noteHandler)
			r.Operation(outer, func(ctx context.Context, in *NoteIn) (*NoteOut, error) {
				out, err := opsRef.Call(ctx, "notes.list", in)
				if err != nil {
					return nil, err
				}
				return out.(*NoteOut), nil
			})
			return nil
		})))
	if err != nil {
		t.Fatal(err)
	}
	opsRef = galleon.MustResolve[*galleon.Operations](app)
	out, err, _ := call(opsRef, "notes.outer", &NoteIn{}, map[string]string{"X-User-test": "ann", "X-Roles": "lister"})
	if err != nil || out.Subject != "ann" {
		t.Fatalf("%+v, %v", out, err)
	}
}

// keyStore is a dependency an authenticator gets through injection.
type keyStore struct{ user string }

func TestPartsFromDependencyInjection(t *testing.T) {
	store := galleon.ModuleFunc("store", func(r *galleon.Registry) error {
		galleon.Supply[*keyStore](r, &keyStore{user: "svc"})
		return nil
	})
	sec := security.Module(
		security.AuthenticateWith("store", func(s *keyStore) security.Authenticator { return storeAuth{s} }),
		security.DecideWith(func(s *keyStore) (security.Decider, error) { return roles, nil }),
	)
	app, err := newApp(t, galleon.Modules(store, sec, notesModule(listNotes)))
	if err != nil {
		t.Fatal(err)
	}
	ops := galleon.MustResolve[*galleon.Operations](app)
	out, err, _ := call(ops, "notes.list", &NoteIn{}, map[string]string{"X-Store": "1"})
	if err != nil || out.Subject != "svc" {
		t.Fatalf("%+v, %v", out, err)
	}

	_, err = galleon.New(galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode), galleon.Modules(sec, notesModule(listNotes)))
	if err == nil || !strings.Contains(err.Error(), "needs *security_test.keyStore, but nothing provides it") {
		t.Fatalf("a missing dependency: %v", err)
	}
	wrong := security.Module(security.AuthenticateWith("other", func(s *keyStore) security.Authenticator { return storeAuth{s} }), security.Decide(roles))
	if _, err := newApp(t, galleon.Modules(store, wrong, notesModule(listNotes))); err == nil || !strings.Contains(err.Error(), `the authenticator for scheme "other"`) {
		t.Fatalf("a scheme mismatch: %v", err)
	}
}

type storeAuth struct{ s *keyStore }

func (storeAuth) Scheme() string { return "store" }
func (a storeAuth) Authenticate(_ context.Context, r *op.Request) (security.Principal, error) {
	if r.Header.Get("X-Store") == "" {
		return nil, security.ErrNoCredentials
	}
	return security.Identity{Subject: a.s.user, Kind: security.KindService, Claims: security.NewValues(map[string]any{"roles": "lister"})}.Principal(), nil
}

func TestThePipelineSkipsStagesAnOperationDoesntNeed(t *testing.T) {
	ops := boot(t, security.Module(security.Authenticate(headerAuth{"test"}), security.Decide(roles)), publicNote, readNote)
	steps := strings.Join(ops.Lookup("notes.public").Steps, ",")
	if strings.Contains(steps, "authorize") || strings.Contains(steps, "gate") || !strings.Contains(steps, "authenticate") {
		t.Fatalf("public: %s", steps)
	}
	steps = strings.Join(ops.Lookup("notes.read").Steps, ",")
	if !strings.Contains(steps, "pre_authorize") || !strings.Contains(steps, ",authorize") {
		t.Fatalf("read: %s", steps)
	}
}
