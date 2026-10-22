package concepts

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/testkit"
)

// allowAll implements the pre-authorize stage, which an operation that
// names an action needs, by letting every call through.
var allowAll = galleon.ModuleFunc("allow-all", func(r *galleon.Registry) error {
	r.ImplementStage(op.StagePreAuthorize, func(next op.Invoker) op.Invoker { return next })
	return nil
})

func TestCreateOrder(t *testing.T) {
	h := testkit.New(t, galleon.Modules(Orders{}, allowAll))
	out, err := testkit.Get[*galleon.Operations](h).Call(context.Background(), "orders.create", &CreateOrderIn{Item: "tea", Quantity: 1})
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
