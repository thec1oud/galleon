package data

import (
	"fmt"
	"strings"
)

// Propagation says how a new transaction relates to the one in ctx. Each
// value is itself an option: m.BeginRead(ctx, data.Independent).
type Propagation uint8

const (
	// Join reuses the enclosing transaction; it is the default. A read
	// joined to a write transaction is a read view of it, and sees its
	// uncommitted writes. With no enclosing transaction, a new one begins.
	Join Propagation = iota
	// Independent begins a separate transaction, on its own connection,
	// which doesn't see the enclosing transaction's uncommitted writes.
	Independent
	// Nested begins a savepoint in the enclosing transaction, or a new
	// transaction when there is none.
	Nested
	// Mandatory joins the enclosing transaction, and fails without one.
	Mandatory
	// Never begins a new transaction, and fails inside one.
	Never
)

func (p Propagation) applyRead(o *Options)  { o.Propagation = p }
func (p Propagation) applyWrite(o *Options) { o.Propagation = p }

func (p Propagation) String() string {
	switch p {
	case Join:
		return "join"
	case Independent:
		return "independent"
	case Nested:
		return "nested"
	case Mandatory:
		return "mandatory"
	case Never:
		return "never"
	}
	return fmt.Sprintf("propagation(%d)", uint8(p))
}

// IsolationLevel is a transaction isolation level. The zero value is the
// database's default, which on Postgres is read committed unless configured
// otherwise.
type IsolationLevel string

const (
	ReadCommitted  IsolationLevel = "read committed"
	RepeatableRead IsolationLevel = "repeatable read"
	Serializable   IsolationLevel = "serializable"
)

// Primary is the name of the primary source, as Source and spans call it.
const Primary = "primary"

// Options is what the options given to a Manager call add up to.
// Implementations read it with ReadOptions and WriteOptions.
type Options struct {
	Propagation Propagation
	// Source is the source's name; empty is the primary.
	Source string
	// Isolation is empty for the database's default, or, joined, for
	// whatever the enclosing transaction has.
	Isolation IsolationLevel
	// Retry is the retry policy of a closure; nil means the
	// implementation's default. Begin calls refuse it.
	Retry *RetryPolicy
}

// ReadOption configures a read transaction.
type ReadOption interface{ applyRead(*Options) }

// WriteOption configures a write transaction.
type WriteOption interface{ applyWrite(*Options) }

// Option configures read and write transactions alike.
type Option interface {
	ReadOption
	WriteOption
}

// option is an option that applies to reads and writes alike. Read-only
// options, such as a read consistency, implement ReadOption alone.
type option func(*Options)

func (o option) applyRead(x *Options)  { o(x) }
func (o option) applyWrite(x *Options) { o(x) }

// ReadOptions folds read options into Options.
func ReadOptions(opts ...ReadOption) Options {
	var o Options
	for _, opt := range opts {
		if opt != nil {
			opt.applyRead(&o)
		}
	}
	return o
}

// WriteOptions folds write options into Options.
func WriteOptions(opts ...WriteOption) Options {
	var o Options
	for _, opt := range opts {
		if opt != nil {
			opt.applyWrite(&o)
		}
	}
	return o
}

// Source runs the transaction on the named source instead of the primary.
// Joining or nesting in a transaction of another source fails: there is no
// two-phase commit.
func Source(name string) Option {
	return option(func(o *Options) { o.Source = strings.TrimSpace(name) })
}

// Isolation sets the isolation level of a new transaction. Joining a
// transaction with a different level fails.
func Isolation(level IsolationLevel) Option {
	return option(func(o *Options) { o.Isolation = level })
}
