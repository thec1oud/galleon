package data

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thec1oud/galleon/errs"
)

// RetryPolicy bounds how often code is rerun after a serialization failure
// or a deadlock. Zero fields take the defaults: 3 attempts, and waits of up
// to 10ms doubling to at most 250ms. A Backoff above MaxBackoff is lowered
// to it.
type RetryPolicy struct {
	// Attempts is how many times the code runs at most, the first run
	// included. 1 turns retries off.
	Attempts int
	// Backoff is the first wait's ceiling; each later one doubles it, up to
	// MaxBackoff. The wait is a random fraction of the ceiling, so
	// transactions that conflicted once don't collide again in step.
	Backoff    time.Duration
	MaxBackoff time.Duration
}

// NoRetry runs code once.
var NoRetry = RetryPolicy{Attempts: 1}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.Attempts <= 0 {
		p.Attempts = 3
	}
	if p.Backoff <= 0 {
		p.Backoff = 10 * time.Millisecond
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = max(p.Backoff, 250*time.Millisecond)
	}
	p.Backoff = min(p.Backoff, p.MaxBackoff)
	return p
}

// Run runs fn, and runs it again while it fails with a retryable error
// (see Retryable) and attempts remain, waiting between attempts. It
// returns fn's last error, or, if ctx ends while waiting, ctx's error as
// Canceled or DeadlineExceeded with fn's last error in its cause. It
// doesn't look at ctx's transaction; implementations of Manager use it for
// closures that own their root transaction.
func (p RetryPolicy) Run(ctx context.Context, fn func(ctx context.Context) error) error {
	p = p.withDefaults()
	ceiling := p.Backoff
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil || attempt >= p.Attempts || !Retryable(err) {
			return err
		}
		t := time.NewTimer(rand.N(ceiling) + 1)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctxError(ctx, err)
		}
		ceiling = min(2*ceiling, p.MaxBackoff)
	}
}

// ctxError is ctx's error, classified as errs.From classifies it, after
// last failed.
func ctxError(ctx context.Context, last error) error {
	cause := fmt.Errorf("%w while waiting to retry after: %v", ctx.Err(), last)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errs.Wrap(cause, errs.DeadlineExceeded, "galleon.deadline_exceeded", "the request took too long")
	}
	return errs.Wrap(cause, errs.Canceled, "galleon.canceled", "the request was canceled")
}

// Retrier is a retry policy as an option, and as a way to rerun code.
type Retrier struct{ p RetryPolicy }

// Retry sets the retry policy of a Read or Write closure. Begin calls refuse
// it: a pair never retries. Its Do reruns code that is safe to run again,
// such as a function that begins, commits and returns:
//
//	err := data.Retry(data.RetryPolicy{}).Do(ctx, func(ctx context.Context) error {
//		tx, err := m.BeginWrite(ctx)
//		...
//		return tx.Commit(ctx)
//	})
func Retry(p RetryPolicy) Retrier { return Retrier{p: p} }

func (r Retrier) applyRead(o *Options)  { o.Retry = &r.p }
func (r Retrier) applyWrite(o *Options) { o.Retry = &r.p }

// Do runs fn under the policy. Inside a transaction, which a serialization
// failure has already doomed, it runs fn once and leaves retrying to the
// code that owns the transaction.
func (r Retrier) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	if From(ctx) != nil {
		return fn(ctx)
	}
	return r.p.Run(ctx, fn)
}

// Error codes of the data plane. The kinds they come with are part of the
// contract: callers and clients branch on them.
const (
	// Conflict, retryable: rerunning the whole transaction can succeed.
	CodeSerializationFailure = "data.serialization_failure" // SQLSTATE 40001
	CodeDeadlock             = "data.deadlock"              // SQLSTATE 40P01
	// Conflict: the data already exists.
	CodeUniqueViolation    = "data.unique_violation"    // 23505
	CodeExclusionViolation = "data.exclusion_violation" // 23P01
	// FailedPrecondition: the change breaks a constraint.
	CodeCheckViolation      = "data.check_violation"       // 23514
	CodeForeignKeyViolation = "data.foreign_key_violation" // 23503
	CodeNotNullViolation    = "data.not_null_violation"    // 23502
	// Conflict: a lock wasn't granted within lock_timeout, or NOWAIT.
	CodeLockNotAvailable = "data.lock_not_available" // 55P03
	// DeadlineExceeded: statement_timeout canceled the statement.
	CodeStatementTimeout = "data.statement_timeout" // 57014
	// Unavailable: the database can't be reached, or refused the session.
	CodeUnavailable = "data.unavailable"
	// Internal: the handle was used wrongly.
	CodeTxEnded       = "data.tx_ended"      // used after Commit or Rollback
	CodePropagation   = "data.propagation"   // Mandatory, Never, a read joined by a write, …
	CodeCrossSource   = "data.cross_source"  // joining a transaction of another source
	CodeRollbackOnly  = "data.rollback_only" // a joined handle rolled back; the commit didn't happen
	CodeTxOpen        = "data.tx_open"       // a nested transaction was still open at commit
	CodeTxLeaked      = "data.tx_leaked"     // still open when its operation ended
	CodeUnknownSource = "data.unknown_source"
)

// Retryable reports whether err is a serialization failure or a deadlock,
// as the database reported it or as the data plane classified it, so
// rerunning the whole transaction can succeed.
func Retryable(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "40001" || pe.Code == "40P01"
	}
	var e *errs.Error
	return errors.As(err, &e) && e.Kind == errs.Conflict && (e.Code == CodeSerializationFailure || e.Code == CodeDeadlock)
}
