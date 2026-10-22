package concepts

import (
	"context"
	"testing"

	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/testkit"
)

func TestDebit(t *testing.T) {
	db := testkit.DB(t, testkit.Schema(
		"CREATE TABLE accounts (id text PRIMARY KEY, balance bigint NOT NULL)",
		"INSERT INTO accounts VALUES ('a', 100)",
	))
	ctx := context.Background()
	m := db.Manager()
	if err := debit(ctx, m, "a", 10); err != nil {
		t.Fatal(err)
	}
	if err := debitPair(ctx, m, "a", 5); err != nil {
		t.Fatal(err)
	}
	var balance int64
	err := m.Read(ctx, func(ctx context.Context, tx data.ReadTx) error {
		return tx.QueryRow(ctx, "SELECT balance FROM accounts WHERE id = 'a'").Scan(&balance)
	})
	if err != nil {
		t.Fatal(err)
	}
	if balance != 85 {
		t.Errorf("balance = %d, want 85", balance)
	}
}
