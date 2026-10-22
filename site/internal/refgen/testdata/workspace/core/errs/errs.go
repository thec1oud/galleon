// Package errs is a stand-in for Galleon's, with what the scanner reads.
package errs

type Kind int

const (
	Internal Kind = iota
	InvalidArgument
	Unauthenticated
	PermissionDenied
	NotFound
	Conflict
)

type Error struct {
	Kind    Kind
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func New(k Kind, code, msg string) error { return &Error{Kind: k, Code: code, Message: msg} }

func Wrap(err error, k Kind, code, msg string) error { return New(k, code, msg) }
