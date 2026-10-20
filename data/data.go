// Package data is the data plane's seam: typed transaction handles, the
// Manager that opens them, and the options that say how a transaction relates
// to the one already in progress. The default implementation is package pg.
//
// The types follow pgx, since Galleon's data plane is Postgres-only: rows,
// batches and command tags are pgx's own, so code written against pgx reads
// the same here.
//
// A transaction has two forms. The closure form begins, commits or rolls back,
// and retries on a serialization failure or deadlock:
//
//	err := m.Write(ctx, func(ctx context.Context, tx data.WriteTx) error {
//		_, err := tx.Exec(ctx, "UPDATE accounts SET balance = balance - $1 WHERE id = $2", amount, id)
//		return err
//	})
//
// The pair form never retries; data.Retry(policy).Do reruns code that can be
// run again:
//
//	tx, err := m.BeginWrite(ctx)
//	if err != nil {
//		return err
//	}
//	defer tx.Rollback(ctx) // a no-op after Commit
//	if err := save(data.With(ctx, tx), tx, order); err != nil {
//		return err
//	}
//	return tx.Commit(ctx)
//
// A transaction joins the one in ctx by default (Join). The closure form puts
// its handle in the ctx it passes on; with pairs, data.With does it.
package data

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The pgx shapes the handles use.
type (
	CommandTag     = pgconn.CommandTag
	Rows           = pgx.Rows
	Row            = pgx.Row
	Batch          = pgx.Batch
	BatchResults   = pgx.BatchResults
	Identifier     = pgx.Identifier
	CopyFromSource = pgx.CopyFromSource
)

// ErrNoRows is what Row.Scan returns when the query returned no row.
var ErrNoRows = pgx.ErrNoRows

// Reader runs statements that read. Query can run any SQL, but a read
// transaction runs READ ONLY, so a write through it fails at the database.
type Reader interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
	SendBatch(ctx context.Context, b *Batch) BatchResults
}

// Querier runs any statement. A WriteTx is one; an ORM or query builder
// takes a Querier and never chooses a database itself.
type Querier interface {
	Reader
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
	CopyFrom(ctx context.Context, table Identifier, columns []string, src CopyFromSource) (int64, error)
}

// ReadTx is a read transaction, or a read view of a write transaction. It has
// no Exec or CopyFrom, so code given a ReadTx can't write through it without
// a type assertion, and that assertion fails.
//
// A handle is not safe for concurrent use, and is not usable once it has
// ended.
type ReadTx interface {
	Reader
	// Begin starts a nested read transaction: a savepoint.
	Begin(ctx context.Context) (ReadTx, error)
	// Commit ends the handle. Only the root handle's Commit commits; a nested
	// handle's releases its savepoint, and a joined handle's only ends it.
	// Commit after Rollback returns an error.
	Commit(ctx context.Context) error
	// Rollback ends the handle and undoes what it did: the whole transaction
	// for the root, back to its savepoint for a nested handle. A joined write
	// handle that rolls back marks the transaction it joined rollback-only,
	// so that one's Commit fails. Rollback after Commit or Rollback is a
	// no-op that returns nil, so defer tx.Rollback(ctx) is always safe.
	Rollback(ctx context.Context) error
}

// WriteTx is a write transaction on the primary or a named source.
type WriteTx interface {
	ReadTx
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
	CopyFrom(ctx context.Context, table Identifier, columns []string, src CopyFromSource) (int64, error)
	// BeginWrite starts a nested write transaction: a savepoint.
	BeginWrite(ctx context.Context) (WriteTx, error)
	// AfterCommit registers fn to run once the root transaction has
	// committed, in registration order. It never runs on rollback. A
	// callback registered on a nested handle belongs to the root, and is
	// dropped if that handle's savepoint is rolled back.
	AfterCommit(fn func(ctx context.Context))
	// AfterRollback registers fn to run once the work it was registered with
	// is rolled back: when the root rolls back (a failed commit included),
	// or when the savepoint of the nested handle it was registered on does.
	AfterRollback(fn func(ctx context.Context))
}

// Manager opens transactions.
type Manager interface {
	// BeginRead and BeginWrite are the pair form: the caller ends the
	// handle with Commit or Rollback. They never retry.
	BeginRead(ctx context.Context, o ...ReadOption) (ReadTx, error)
	BeginWrite(ctx context.Context, o ...WriteOption) (WriteTx, error)
	// Read and Write are the closure form. They commit when fn returns nil
	// and roll back otherwise, and when they own the root transaction they
	// rerun the whole of fn on a serialization failure or deadlock, under
	// the retry policy. Joined or nested, they never retry: the error goes
	// to the root's owner, which can. fn's ctx carries the handle.
	Read(ctx context.Context, fn func(context.Context, ReadTx) error, o ...ReadOption) error
	Write(ctx context.Context, fn func(context.Context, WriteTx) error, o ...WriteOption) error
}

type txKey struct{}

// With returns a ctx that carries tx, so transactions begun with it join or
// nest in tx. The closure forms do this for the ctx they pass on.
func With(ctx context.Context, tx ReadTx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// From returns the transaction ctx carries, or nil. It is a WriteTx when the
// enclosing transaction writes.
func From(ctx context.Context) ReadTx {
	tx, _ := ctx.Value(txKey{}).(ReadTx)
	return tx
}
