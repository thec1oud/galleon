package concepts

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/security"
	"github.com/thec1oud/galleon/testkit"
)

func TestCreateOrder(t *testing.T) {
	sec := testkit.NewSecurity().Allow("orders:create")
	h := testkit.New(t, galleon.Modules(Orders{}, security.Module(security.Decide(sec))))
	ctx := security.WithPrincipal(context.Background(), sec.User("u_1"))
	out, err := testkit.Get[*galleon.Operations](h).Call(ctx, "orders.create", &CreateOrderIn{Item: "tea", Quantity: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(*CreateOrderOut).ID; got != "ord_1" {
		t.Errorf("ID = %q", got)
	}
}

func TestAnOperationThatNamesAnActionNeedsAuthorization(t *testing.T) {
	_, err := galleon.New(galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode), galleon.Modules(Orders{}))
	if err == nil || !strings.Contains(err.Error(), "no module implements") {
		t.Fatalf("New = %v, want a missing stage", err)
	}
}

func TestFindOrder(t *testing.T) {
	err := findOrder(context.Background(), false)
	if errs.KindOf(err) != errs.NotFound || !errors.Is(err, errs.New(errs.NotFound, "orders.not_found", "")) {
		t.Errorf("err = %v", err)
	}
}
