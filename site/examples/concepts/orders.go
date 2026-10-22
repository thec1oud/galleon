// Package concepts holds the samples of the docs' Concepts pages.
package concepts

import (
	"context"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

type CreateOrderIn struct {
	Item     string `json:"item" validate:"required"`
	Quantity int    `json:"quantity" validate:"min=1"`
}

type CreateOrderOut struct {
	ID string `json:"id"`
}

// docs:start operation
var CreateOrder = op.Unary[CreateOrderIn, CreateOrderOut]("orders.create").
	HTTP("POST", "/orders").
	RPC("orders.v1.Orders", "Create").
	Authorize("orders:create").
	Timeout(5 * time.Second)

type Orders struct{}

func (Orders) Name() string { return "orders" }

func (Orders) Register(r *galleon.Registry) error {
	r.Operation(CreateOrder, createOrder)
	return nil
}

func createOrder(ctx context.Context, in *CreateOrderIn) (*CreateOrderOut, error) {
	return &CreateOrderOut{ID: "ord_1"}, nil
}

// docs:end operation

func findOrder(ctx context.Context, found bool) error {
	if !found {
		// docs:start error
		return errs.New(errs.NotFound, "orders.not_found", "no such order")
		// docs:end error
	}
	return nil
}

func debit(ctx context.Context, m data.Manager, id string, amount int64) error {
	// docs:start closure
	err := m.Write(ctx, func(ctx context.Context, tx data.WriteTx) error {
		_, err := tx.Exec(ctx, "UPDATE accounts SET balance = balance - $1 WHERE id = $2", amount, id)
		return err
	})
	// docs:end closure
	return err
}

func debitPair(ctx context.Context, m data.Manager, id string, amount int64) error {
	// docs:start pair
	tx, err := m.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // a no-op after Commit
	if _, err := tx.Exec(ctx, "UPDATE accounts SET balance = balance - $1 WHERE id = $2", amount, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
	// docs:end pair
}
