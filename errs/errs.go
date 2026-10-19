// Package errs is Galleon's error model. Every error that reaches a transport
// edge is an *Error with a Kind (which fixes its HTTP status and gRPC code) and a
// stable catalog Code that clients can switch on.
package errs

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Kind classifies an error. The zero value is Internal, so an unclassified error
// never maps to a success or client-error status by accident.
type Kind uint8

const (
	Internal Kind = iota
	InvalidArgument
	Unauthenticated
	PermissionDenied
	NotFound
	Conflict
	FailedPrecondition
	TooManyRequests
	Canceled
	DeadlineExceeded
	Unavailable
	Unimplemented
)

var kindNames = [...]string{
	Internal:           "internal",
	InvalidArgument:    "invalid_argument",
	Unauthenticated:    "unauthenticated",
	PermissionDenied:   "permission_denied",
	NotFound:           "not_found",
	Conflict:           "conflict",
	FailedPrecondition: "failed_precondition",
	TooManyRequests:    "too_many_requests",
	Canceled:           "canceled",
	DeadlineExceeded:   "deadline_exceeded",
	Unavailable:        "unavailable",
	Unimplemented:      "unimplemented",
}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// HTTPStatus is the status code a Kind maps to.
func (k Kind) HTTPStatus() int {
	switch k {
	case InvalidArgument:
		return 400
	case Unauthenticated:
		return 401
	case PermissionDenied:
		return 403
	case NotFound:
		return 404
	case Conflict:
		return 409
	case FailedPrecondition:
		return 422
	case TooManyRequests:
		return 429
	case Canceled:
		return 499
	case DeadlineExceeded:
		return 504
	case Unavailable:
		return 503
	case Unimplemented:
		return 501
	default:
		return 500
	}
}

// GRPCCode is the gRPC status code a Kind maps to (values from google.golang.org/grpc/codes).
func (k Kind) GRPCCode() uint32 {
	switch k {
	case Canceled:
		return 1
	case InvalidArgument:
		return 3
	case DeadlineExceeded:
		return 4
	case NotFound:
		return 5
	case Conflict:
		return 6 // ALREADY_EXISTS
	case PermissionDenied:
		return 7
	case TooManyRequests:
		return 8 // RESOURCE_EXHAUSTED
	case FailedPrecondition:
		return 9
	case Unimplemented:
		return 12
	case Unavailable:
		return 14
	case Unauthenticated:
		return 16
	default:
		return 13 // INTERNAL
	}
}

// Error is the single error type Galleon understands at its edges.
type Error struct {
	Kind    Kind
	Code    string   // stable catalog code, "<module>.<name>"
	Message string   // safe to show to clients
	Details []Detail // structured, client-visible details
	Cause   error    // internal only; never serialized
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Kind.String()
	}
	if e.Code != "" {
		msg = e.Code + ": " + msg
	}
	if e.Cause != nil {
		return msg + ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

// Is reports whether target is an *Error with the same Kind and Code, so
// errors.Is(err, errs.New(errs.NotFound, "orders.not_found", "")) works.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return e.Kind == t.Kind && (t.Code == "" || e.Code == t.Code)
}

// New returns an Error without a cause.
func New(kind Kind, code, message string, details ...Detail) *Error {
	return &Error{Kind: kind, Code: code, Message: message, Details: details}
}

// Wrap returns an Error that keeps cause for logs but never shows it to clients.
func Wrap(cause error, kind Kind, code, message string, details ...Detail) *Error {
	return &Error{Kind: kind, Code: code, Message: message, Details: details, Cause: cause}
}

// From converts any error into an *Error. Context errors become Canceled and
// DeadlineExceeded; anything unclassified becomes Internal with the original
// error kept as Cause. From(nil) returns nil.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	switch {
	case errors.Is(err, context.Canceled):
		return Wrap(err, Canceled, "galleon.canceled", "the request was canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return Wrap(err, DeadlineExceeded, "galleon.deadline_exceeded", "the request took too long")
	default:
		return Wrap(err, Internal, "galleon.internal", "internal error")
	}
}

// KindOf returns the Kind of err as From would classify it.
func KindOf(err error) Kind { return From(err).Kind }

// Detail is client-visible structured information attached to an Error.
type Detail interface {
	// DetailType names the detail in serialized form, e.g. "field_violations".
	DetailType() string
}

// FieldViolations lists input problems. Paths are JSON Pointers into the wire
// representation, not Go field names.
type FieldViolations []Violation

type Violation struct {
	Path   string         `json:"path"`
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
	MsgKey string         `json:"msg_key,omitempty"`
}

func (FieldViolations) DetailType() string { return "field_violations" }

// RetryInfo tells clients when retrying may succeed.
type RetryInfo struct {
	After time.Duration `json:"-"`
}

func (RetryInfo) DetailType() string { return "retry_info" }
