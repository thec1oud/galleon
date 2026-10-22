package securitytest_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/security"
	"github.com/thec1oud/galleon/security/apikey"
	"github.com/thec1oud/galleon/security/jwt"
	"github.com/thec1oud/galleon/testkit"
	"github.com/thec1oud/galleon/testkit/conformance/securitytest"
)

func user(subject string, roles ...string) security.Principal {
	return security.Identity{Subject: subject, Kind: security.KindUser, Claims: security.NewValues(map[string]any{"roles": roles})}.Principal()
}

func TestRoleTable(t *testing.T) {
	table := &security.RoleTable{Grants: map[string][]string{"editor": {"notes:*"}, "viewer": {"notes:read"}}}
	editor, viewer := user("e", "editor"), user("v", "viewer")
	securitytest.Decider(t, table, securitytest.Cases{Checks: []securitytest.Check{
		{Query: security.Query{Subject: editor, Action: "notes:edit"}, Allow: true},
		{Query: security.Query{Subject: viewer, Action: "notes:edit"}, Allow: false},
		{Query: security.Query{Subject: viewer, Action: "notes:read"}, Allow: true},
		{Query: security.Query{Subject: security.Anonymous(), Action: "notes:read"}, Allow: false},
		{Query: security.Query{Subject: editor, Action: "notes:edit", Resource: &security.Resource{Type: "note", ID: "1"}}, Allow: true},
	}})
}

func TestTestkitSecurity(t *testing.T) {
	sec := testkit.NewSecurity().Allow("notes:read").AllowFor("ann", "notes:edit")
	ann, bob := sec.User("ann"), sec.User("bob")
	securitytest.Decider(t, sec, securitytest.Cases{Checks: []securitytest.Check{
		{Query: security.Query{Subject: ann, Action: "notes:edit"}, Allow: true},
		{Query: security.Query{Subject: bob, Action: "notes:edit"}, Allow: false},
		{Query: security.Query{Subject: bob, Action: "notes:read"}, Allow: true},
		{Query: security.Query{Subject: security.Anonymous(), Action: "notes:read"}, Allow: false},
	}})
	h := func(v string) *op.Request { return &op.Request{Header: http.Header{"Authorization": {v}}} }
	securitytest.Authenticator(t, sec, securitytest.Credentials{
		None:    []*op.Request{{}, h("Bearer x")},
		Invalid: []*op.Request{h("Test nobody")},
		Valid:   []securitytest.Valid{{Request: h("Test ann"), Subject: "ann"}},
	})
}

func TestAPIKeys(t *testing.T) {
	key, digest, err := apikey.Generate("t")
	if err != nil {
		t.Fatal(err)
	}
	a := apikey.New(apikey.Keys{digest: security.Identity{Subject: "k1", Kind: security.KindAPIKey}.Principal()})
	h := func(k, v string) *op.Request { return &op.Request{Header: http.Header{k: {v}}} }
	securitytest.Authenticator(t, a, securitytest.Credentials{
		None:    []*op.Request{{}, h("Authorization", "Bearer x")},
		Invalid: []*op.Request{h("X-Api-Key", "t_wrong"), h("X-Api-Key", "")},
		Valid:   []securitytest.Valid{{Request: h("X-Api-Key", key), Subject: "k1"}},
	})
}

// owners lets a subject read the notes it owns, and filters lists to them.
type owners struct{}

func (owners) Check(_ context.Context, q security.Query) (security.Decision, error) {
	if q.Resource == nil || security.IsAnonymous(q.Subject) {
		return security.Deny(""), nil
	}
	owner, _ := q.Resource.Attributes.String("owner")
	return security.Decision{Allow: q.Action == "notes:read" && owner == q.Subject.Subject()}, nil
}

func (o owners) CheckMany(ctx context.Context, qs []security.Query) ([]security.Decision, error) {
	return security.DeciderFunc(o.Check).CheckMany(ctx, qs)
}

func (owners) Filter(_ context.Context, q security.FilterQuery) (security.Filter, error) {
	if q.Action != "notes:read" || security.IsAnonymous(q.Subject) {
		return security.Never(), nil
	}
	return security.Eq("owner", q.Subject.Subject()), nil
}

func TestAFilterer(t *testing.T) {
	ann := user("ann")
	note := func(id, owner string) security.Resource {
		return security.Resource{Type: "note", ID: id, Attributes: security.NewValues(map[string]any{"owner": owner})}
	}
	n1, n2 := note("1", "ann"), note("2", "bob")
	securitytest.Decider(t, owners{}, securitytest.Cases{
		Checks: []securitytest.Check{
			{Query: security.Query{Subject: ann, Action: "notes:read", Resource: &n1}, Allow: true},
			{Query: security.Query{Subject: ann, Action: "notes:read", Resource: &n2}, Allow: false},
		},
		Filters: []securitytest.FilterCase{
			{Query: security.FilterQuery{Subject: ann, Action: "notes:read", ResourceType: "note"}, Resources: []security.Resource{n1, n2}},
			{Query: security.FilterQuery{Subject: ann, Action: "notes:delete", ResourceType: "note"}, Resources: []security.Resource{n1, n2}},
		},
	})
}

func TestJWT(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	jwks := fmt.Sprintf(`{"keys": [{"kty": "OKP", "crv": "Ed25519", "kid": "k", "x": %q}]}`, b64(pub))
	sign := func(claims string) string {
		input := b64([]byte(`{"alg":"EdDSA","kid":"k"}`)) + "." + b64([]byte(claims))
		return input + "." + b64(ed25519.Sign(priv, []byte(input)))
	}
	exp := time.Now().Add(time.Hour).Unix()
	a, err := jwt.New(jwt.Config{Issuer: "https://id.example.com", Audience: []string{"api"}, JWKS: jwks})
	if err != nil {
		t.Fatal(err)
	}
	h := func(v string) *op.Request { return &op.Request{Header: http.Header{"Authorization": {v}}} }
	securitytest.Authenticator(t, a, securitytest.Credentials{
		None: []*op.Request{{}, h("Basic dTpw")},
		Invalid: []*op.Request{
			h("Bearer " + sign(fmt.Sprintf(`{"iss":"https://id.example.com","aud":"other","sub":"u","exp":%d}`, exp))),
			h("Bearer junk"),
		},
		Valid: []securitytest.Valid{{Request: h("Bearer " + sign(fmt.Sprintf(`{"iss":"https://id.example.com","aud":"api","sub":"u","exp":%d}`, exp))), Subject: "u"}},
	})
}
