package concepts

import (
	"context"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/security"
	"github.com/thec1oud/galleon/security/jwt"
)

// docs:start module
// Security answers who is calling, in which context, and what they may do.
func Security() galleon.Module {
	return security.Module(
		// Who: bearer tokens from the issuer the "jwt" config section names.
		jwt.Bearer(),
		// Where: the organization the token's org_id claim names.
		security.ResolveContext(security.ClaimResolver{Domain: "org", Claim: "org_id"}),
		// What: each role grants actions; everything else is denied.
		security.Decide(&security.RoleTable{Grants: map[string][]string{
			"admin": {"*"},
			"clerk": {"orders:create", "orders:read"},
		}}),
	)
}

// docs:end module

type CancelOrderIn struct {
	ID string `path:"id" json:"-"`
}

type Order struct {
	ID, Owner string
}

// docs:start declare
var CancelOrder = op.Unary[CancelOrderIn, Order]("orders.cancel").
	HTTP("POST", "/orders/{id}/cancel").
	Contexts("org").                                               // only in an organization
	AuthorizeResource("orders:cancel", op.Resource("order", "id")) // the action, then this order

// docs:end declare

// docs:start handler
func cancelOrder(ctx context.Context, in *CancelOrderIn) (*Order, error) {
	order := Order{ID: in.ID, Owner: "u_42"} // loaded from the database
	// A question the pipeline can't ask, because it needs the order's owner.
	err := security.Require(ctx, "orders:cancel", &security.Resource{
		Type: "order", ID: order.ID,
		Attributes: security.NewValues(map[string]any{"owner": order.Owner}),
	})
	if err != nil {
		return nil, err // 401, 403 or 503, already right for the caller
	}
	_ = security.Caller(ctx).Subject() // who is cancelling, for the audit trail
	return &order, nil
}

// docs:end handler

// CancelOrders is the module that serves CancelOrder.
type CancelOrders struct{}

func (CancelOrders) Name() string { return "cancel-orders" }

func (CancelOrders) Register(r *galleon.Registry) error {
	r.Operation(CancelOrder, cancelOrder)
	return nil
}
