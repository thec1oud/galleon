package data_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
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

func TestRetryStopsWithTheContextsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := data.RetryPolicy{Attempts: 5, Backoff: 10 * time.Second, MaxBackoff: 10 * time.Second}.Run(ctx, func(context.Context) error {
		return &pgconn.PgError{Code: "40001"}
	})
	if errs.KindOf(err) != errs.DeadlineExceeded || data.Retryable(err) || time.Since(start) > 5*time.Second {
		t.Fatalf("got %v after %v", err, time.Since(start))
	}
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err = data.RetryPolicy{Attempts: 5, Backoff: 10 * time.Second, MaxBackoff: 10 * time.Second}.Run(ctx, func(context.Context) error {
		return &pgconn.PgError{Code: "40P01"}
	})
	if errs.KindOf(err) != errs.Canceled {
		t.Fatalf("got %v", err)
	}
}

func TestRetryKeepsAnExplicitMaxBackoff(t *testing.T) {
	start := time.Now()
	runs := 0
	_ = data.RetryPolicy{Attempts: 6, Backoff: time.Second, MaxBackoff: time.Millisecond}.Run(context.Background(), func(context.Context) error {
		runs++
		return &pgconn.PgError{Code: "40001"}
	})
	if runs != 6 || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("%d runs took %v; the waits must stay under MaxBackoff", runs, time.Since(start))
	}
}

func TestRetryWaitsHaveAFloor(t *testing.T) {
	// Every wait is at least half its ceiling.
	const ceiling = 40 * time.Millisecond
	var calls []time.Time
	_ = data.RetryPolicy{Attempts: 11, Backoff: ceiling, MaxBackoff: ceiling}.Run(context.Background(), func(context.Context) error {
		calls = append(calls, time.Now())
		return &pgconn.PgError{Code: "40001"}
	})
	for i := 1; i < len(calls); i++ {
		if d := calls[i].Sub(calls[i-1]); d < ceiling/2 {
			t.Fatalf("wait %d was %v, under half the %v ceiling", i, d, ceiling)
		}
	}
}
