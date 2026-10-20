package pg

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"

	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
)

// classify turns the driver's errors into Galleon's for the conditions
// callers branch on. The driver's error stays the Cause, so errors.As still
// finds a *pgconn.PgError, but only the safe Message reaches clients. Errors
// it doesn't recognize, such as pgx.ErrNoRows or a syntax error, are
// returned as they are: callers compare them, and at the edge they become
// Internal.
//
// A serialization failure or deadlock is a Conflict with a RetryInfo
// detail: rerunning the whole transaction can succeed, and the closure
// form does so itself.
func classify(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var e *errs.Error
	if errors.As(err, &e) {
		return err
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		if k, code, msg, ok := sqlstate(pe.Code); ok {
			if k == errs.Conflict && data.Retryable(pe) {
				return errs.Wrap(err, k, code, msg, errs.RetryInfo{})
			}
			return errs.Wrap(err, k, code, msg)
		}
	}
	ctxErr := ctx.Err()
	byContext := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		// The driver reports a cancelled or expired context in its own
		// words: a cancelled query, a timeout, a broken connection.
		ctxErr != nil && ((pe != nil && pe.Code == "57014") || pgconn.Timeout(err) || unreachable(err))
	if byContext {
		if ctxErr == nil {
			ctxErr = err
		}
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return errs.Wrap(err, errs.DeadlineExceeded, "galleon.deadline_exceeded", "the request took too long")
		}
		return errs.Wrap(err, errs.Canceled, "galleon.canceled", "the request was canceled")
	}
	if pe != nil && pe.Code == "57014" {
		// Canceled with the caller's ctx still live: statement_timeout.
		return errs.Wrap(err, errs.DeadlineExceeded, data.CodeStatementTimeout, "the request took too long")
	}
	if pe == nil && unreachable(err) {
		return errs.Wrap(err, errs.Unavailable, data.CodeUnavailable, "the database is unavailable")
	}
	return err
}

// sqlstate maps the SQLSTATEs the data plane classifies.
func sqlstate(code string) (k errs.Kind, c, msg string, ok bool) {
	switch code {
	case "40001":
		return errs.Conflict, data.CodeSerializationFailure, "the change conflicted with another one; retry it", true
	case "40P01":
		return errs.Conflict, data.CodeDeadlock, "the change conflicted with another one; retry it", true
	case "55P03":
		return errs.Conflict, data.CodeLockNotAvailable, "the data is locked by another change; retry it", true
	case "23505":
		return errs.Conflict, data.CodeUniqueViolation, "the data already exists", true
	case "23P01":
		return errs.Conflict, data.CodeExclusionViolation, "the data conflicts with existing data", true
	case "23514":
		return errs.FailedPrecondition, data.CodeCheckViolation, "the change breaks a rule of the data", true
	case "23503":
		return errs.FailedPrecondition, data.CodeForeignKeyViolation, "the change refers to data that doesn't exist, or removes data still referred to", true
	case "23502":
		return errs.FailedPrecondition, data.CodeNotNullViolation, "a required value is missing", true
	}
	switch {
	case strings.HasPrefix(code, "08"), // connection exception
		code == "57P01", code == "57P02", code == "57P03", // admin shutdown, crash shutdown, cannot connect now
		code == "53300": // too many connections
		return errs.Unavailable, data.CodeUnavailable, "the database is unavailable", true
	}
	return 0, "", "", false
}

// unreachable reports whether err means the database couldn't be reached or
// the connection broke.
func unreachable(err error) bool {
	var ce *pgconn.ConnectError
	var ne net.Error
	return errors.As(err, &ce) || errors.As(err, &ne) ||
		errors.Is(err, puddle.ErrClosedPool) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
