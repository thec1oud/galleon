package data_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thec1oud/galleon/data"
)

func TestOptionsFold(t *testing.T) {
	p := data.RetryPolicy{Attempts: 7}
	r := data.ReadOptions(data.Independent, data.Source("analytics"), data.Isolation(data.RepeatableRead), data.Retry(p), nil)
	if r.Propagation != data.Independent || r.Source != "analytics" || r.Isolation != data.RepeatableRead || r.Retry == nil || r.Retry.Attempts != 7 {
		t.Fatalf("got %+v", r)
	}
	w := data.WriteOptions(data.Nested, data.Mandatory)
	if w.Propagation != data.Mandatory || w.Source != "" || w.Retry != nil {
		t.Fatalf("the last option must win, and others stay unset: %+v", w)
	}
	if (data.Options{}).Propagation != data.Join {
		t.Fatal("Join must be the default")
	}
}

type fakeTx struct{ data.ReadTx }

func TestContextCarriesTheTransaction(t *testing.T) {
	ctx := context.Background()
	if data.From(ctx) != nil {
		t.Fatal("an empty ctx carries a transaction")
	}
	tx := &fakeTx{}
	if data.From(data.With(ctx, tx)) != tx {
		t.Fatal("With and From disagree")
	}
}

func TestRetryDoLeavesRetriesToTheOwner(t *testing.T) {
	runs := 0
	fail := func(context.Context) error { runs++; return &pgconn.PgError{Code: "40001"} }
	_ = data.Retry(data.RetryPolicy{Attempts: 3, Backoff: 1}).Do(data.With(context.Background(), &fakeTx{}), fail)
	if runs != 1 {
		t.Fatalf("ran %d times inside a transaction", runs)
	}
	runs = 0
	_ = data.Retry(data.RetryPolicy{Attempts: 3, Backoff: 1}).Do(context.Background(), fail)
	if runs != 3 {
		t.Fatalf("ran %d times outside one", runs)
	}
}
