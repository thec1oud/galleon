package concepts

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/security"
	"github.com/thec1oud/galleon/testkit"
)

func TestTheSecurityModuleBoots(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwks := fmt.Sprintf(`{"keys": [{"kty": "OKP", "crv": "Ed25519", "x": %q}]}`, base64.RawURLEncoding.EncodeToString(pub))
	cfg := fmt.Sprintf(`{"jwt": {"issuer": "https://id.example.com", "audience": ["orders"], "jwks": %q}}`, jwks)
	testkit.New(t, galleon.ConfigJSON([]byte(cfg)), galleon.Modules(Security(), Orders{}, CancelOrders{}))
}

func TestCancelOrderChecksTheOwner(t *testing.T) {
	// The owner may cancel their order; another clerk may cancel orders
	// in general, but the handler's check on this one denies.
	owner := security.DeciderFunc(func(_ context.Context, q security.Query) (security.Decision, error) {
		if q.Resource != nil {
			if o, ok := q.Resource.Attributes.String("owner"); ok && o != q.Subject.Subject() {
				return security.Deny("not the owner"), nil
			}
		}
		return security.Allow(""), nil
	})
	sec := testkit.NewSecurity().InContext("org", "acme")
	h := testkit.New(t, galleon.Modules(CancelOrders{}, security.Module(security.ResolveContext(sec), security.Decide(owner))))
	ops := testkit.Get[*galleon.Operations](h)
	as := func(subject string) context.Context {
		return security.WithPrincipal(context.Background(), sec.User(subject))
	}
	if _, err := ops.Call(as("u_42"), "orders.cancel", &CancelOrderIn{ID: "ord_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Call(as("u_7"), "orders.cancel", &CancelOrderIn{ID: "ord_1"}); errs.KindOf(err) != errs.PermissionDenied {
		t.Fatalf("another user: %v", err)
	}
}
