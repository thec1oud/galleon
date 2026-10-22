package pg

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Fence is a table of fencing rows, one per owner (a migrator's name), that
// lets a holder of a coord.Lease prove, inside the transaction it writes in,
// that no newer holder has written since: a node that paused past its lease
// while another took the lock can't write once it resumes.
//
// Tokens compare only within one coordination epoch (coord.Lease.Epoch): the
// row refuses a lower token of its epoch. A lease of a new epoch, such as one
// from a new process on the in-process backend, whose tokens start again at
// 1, takes the row over, and the epoch it replaces is recorded as superseded
// in the row; a lease of a superseded epoch is refused whatever its token,
// so the old process can't take the row back. Epochs aren't ordered
// otherwise: the first lease of an epoch the row hasn't seen wins it.
//
// sqlmigrate and the dpg adapter fence their migrations with it.
type Fence struct {
	// Table is the fencing table, optionally schema-qualified.
	Table pgx.Identifier
}

// FenceLease is the lease a write is made under.
type FenceLease struct {
	Epoch string
	Token int64
}

// StaleLeaseError is a lease Raise refused.
type StaleLeaseError struct {
	Owner string
	// Lease is the refused lease.
	Lease FenceLease
	// Held is what the row holds: the epoch and token of the last holder
	// that wrote.
	Held FenceLease
	// Superseded: the lease's epoch has been taken over by Held's, so no
	// token of it can write again. Otherwise Held is of the lease's epoch,
	// with a higher token.
	Superseded bool
}

func (e *StaleLeaseError) Error() string {
	if e.Superseded {
		return fmt.Sprintf("pg: fence %q: the lease's epoch %q is stale: a holder of epoch %q has written since", e.Owner, e.Lease.Epoch, e.Held.Epoch)
	}
	return fmt.Sprintf("pg: fence %q: the lease's fencing token %d is stale: a holder of token %d in its epoch has written since", e.Owner, e.Lease.Token, e.Held.Token)
}

// Create creates the fencing table in tx if it doesn't exist. A table made
// before epochs, or before superseded epochs were kept, gains the columns;
// its rows get the empty epoch, which the next lease supersedes. Callers
// that may race creating it hold a lock while they do (sqlmigrate takes a
// transaction-scoped advisory lock).
func (f Fence) Create(ctx context.Context, tx pgx.Tx) error {
	table := f.Table.Sanitize()
	_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+table+` (
	migrator   text   PRIMARY KEY,
	epoch      text   NOT NULL,
	token      bigint NOT NULL,
	superseded text[] NOT NULL DEFAULT '{}'
)`)
	if err != nil {
		return err
	}
	// The table is altered only when it lacks a column: ALTER waits for
	// every lock on it, such as another owner's write that holds its row.
	var have int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM pg_attribute WHERE attrelid = $1::regclass AND attname IN ('epoch', 'superseded') AND NOT attisdropped`, table).Scan(&have)
	if err != nil || have == 2 {
		return err
	}
	_, err = tx.Exec(ctx, `ALTER TABLE `+table+`
	ADD COLUMN IF NOT EXISTS epoch      text   NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS superseded text[] NOT NULL DEFAULT '{}'`)
	return err
}

// Raise moves owner's row to the lease's epoch and token in tx, or returns
// a *StaleLeaseError if the lease is stale. The row stays locked until tx
// ends, so the writes of two holders never interleave.
func (f Fence) Raise(ctx context.Context, tx pgx.Tx, owner string, l FenceLease) error {
	table := f.Table.Sanitize()
	tag, err := tx.Exec(ctx, `INSERT INTO `+table+` (migrator, epoch, token) VALUES ($1, $2, $3) ON CONFLICT (migrator) DO NOTHING`, owner, l.Epoch, l.Token)
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	var (
		held       FenceLease
		superseded []string
	)
	err = tx.QueryRow(ctx, `SELECT epoch, token, superseded FROM `+table+` WHERE migrator = $1 FOR UPDATE`, owner).Scan(&held.Epoch, &held.Token, &superseded)
	if err != nil {
		return err
	}
	switch {
	case held.Epoch == l.Epoch && held.Token > l.Token:
		return &StaleLeaseError{Owner: owner, Lease: l, Held: held}
	case held.Epoch == l.Epoch:
		_, err = tx.Exec(ctx, `UPDATE `+table+` SET token = $2 WHERE migrator = $1`, owner, l.Token)
	case slices.Contains(superseded, l.Epoch):
		return &StaleLeaseError{Owner: owner, Lease: l, Held: held, Superseded: true}
	default:
		_, err = tx.Exec(ctx, `UPDATE `+table+` SET epoch = $2, token = $3, superseded = array_append(superseded, epoch) WHERE migrator = $1`, owner, l.Epoch, l.Token)
	}
	return err
}
