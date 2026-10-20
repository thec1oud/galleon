package pg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"

	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
)

func TestClassify(t *testing.T) {
	bg := context.Background()
	canceled, cancel := context.WithCancel(bg)
	cancel()
	expired, cancel2 := context.WithDeadline(bg, time.Unix(0, 0))
	defer cancel2()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		kind errs.Kind
		code string
	}{
		{"serialization", bg, &pgconn.PgError{Code: "40001"}, errs.Conflict, data.CodeSerializationFailure},
		{"deadlock", bg, &pgconn.PgError{Code: "40P01"}, errs.Conflict, data.CodeDeadlock},
		{"unique", bg, &pgconn.PgError{Code: "23505"}, errs.Conflict, data.CodeUniqueViolation},
		{"exclusion", bg, &pgconn.PgError{Code: "23P01"}, errs.Conflict, data.CodeExclusionViolation},
		{"check", bg, &pgconn.PgError{Code: "23514"}, errs.FailedPrecondition, data.CodeCheckViolation},
		{"foreign key", bg, &pgconn.PgError{Code: "23503"}, errs.FailedPrecondition, data.CodeForeignKeyViolation},
		{"not null", bg, &pgconn.PgError{Code: "23502"}, errs.FailedPrecondition, data.CodeNotNullViolation},
		{"connection exception", bg, &pgconn.PgError{Code: "08006"}, errs.Unavailable, data.CodeUnavailable},
		{"admin shutdown", bg, &pgconn.PgError{Code: "57P01"}, errs.Unavailable, data.CodeUnavailable},
		{"too many connections", bg, &pgconn.PgError{Code: "53300"}, errs.Unavailable, data.CodeUnavailable},
		{"refused", bg, &net.OpError{Op: "dial", Err: errors.New("connection refused")}, errs.Unavailable, data.CodeUnavailable},
		{"closed pool", bg, puddle.ErrClosedPool, errs.Unavailable, data.CodeUnavailable},
		// A pooled connection whose server went away.
		{"closed connection", bg, fmt.Errorf("begin: %w", pgconn.ErrConnClosed), errs.Unavailable, data.CodeUnavailable},
		{"lock not available", bg, &pgconn.PgError{Code: "55P03"}, errs.Conflict, data.CodeLockNotAvailable},
		{"statement timeout", bg, &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}, errs.DeadlineExceeded, data.CodeStatementTimeout},
		{"statement timeout in German", bg, &pgconn.PgError{Code: "57014", Message: "storniere Anfrage wegen Zeitüberschreitung der Anweisung"}, errs.DeadlineExceeded, data.CodeStatementTimeout},
		{"statement timeout without a message", bg, &pgconn.PgError{Code: "57014"}, errs.DeadlineExceeded, data.CodeStatementTimeout},
		{"admin cancel", bg, &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"}, errs.Unavailable, data.CodeUnavailable},
		{"canceled", canceled, context.Canceled, errs.Canceled, "galleon.canceled"},
		{"deadline", expired, context.DeadlineExceeded, errs.DeadlineExceeded, "galleon.deadline_exceeded"},
		// The pool cancels its own dials as it closes: not the caller's doing.
		{"canceled from inside", bg, context.Canceled, errs.Unavailable, data.CodeUnavailable},
		{"query canceled by ctx", canceled, &pgconn.PgError{Code: "57014"}, errs.Canceled, "galleon.canceled"},
		{"broken by ctx", canceled, &net.OpError{Op: "read", Err: errors.New("closed")}, errs.Canceled, "galleon.canceled"},
	} {
		err := classify(tc.ctx, tc.err)
		var e *errs.Error
		if !errors.As(err, &e) || e.Kind != tc.kind || e.Code != tc.code {
			t.Errorf("%s: got %v", tc.name, err)
			continue
		}
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: the driver's error isn't the cause", tc.name)
		}
	}
	// Errors callers compare stay as they are.
	for _, err := range []error{pgx.ErrNoRows, &pgconn.PgError{Code: "42601"}, errors.New("scan")} {
		if got := classify(bg, err); got != err {
			t.Errorf("%v became %v", err, got)
		}
	}
	if classify(canceled, pgx.ErrNoRows) != pgx.ErrNoRows {
		t.Error("no rows after the ctx ended isn't a cancellation")
	}
	if classify(bg, nil) != nil {
		t.Error("nil isn't nil")
	}
}

func TestOperationName(t *testing.T) {
	for sql, want := range map[string]string{
		"select 1":                    "SELECT",
		"  (SELECT 1) UNION SELECT 2": "SELECT",
		"\n\tINSERT INTO x":           "INSERT",
		"-- comment\nselect":          "",
		"":                            "",
	} {
		if got := operation(sql); got != want {
			t.Errorf("operation(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestAClosedScopeTracksNothing(t *testing.T) {
	s := &scope{}
	h := &handle{}
	if !s.add(h) {
		t.Fatal("an open scope refused a transaction")
	}
	if got := s.take(); len(got) != 1 {
		t.Fatalf("took %d", len(got))
	}
	if s.add(&handle{}) {
		t.Fatal("a scope took a transaction after its operation ended")
	}
	if got := s.take(); len(got) != 0 {
		t.Fatalf("a closed scope kept %d transactions", len(got))
	}
}
