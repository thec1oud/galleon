package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Level is a read consistency: which copy of the data a read may see. Reads
// choose one with Consistency; operations declare a default for their reads
// in their descriptor (op.Descriptor.Consistency); the implementation has a
// default of its own. A call's option wins over the operation's, which wins
// over the implementation's.
//
// Consistency only routes a read that begins a transaction. A read that
// joins or nests in one runs on that transaction's connection, whatever it
// asks for, so a read inside a write transaction never leaves the primary.
// With no replicas, every level reads the primary.
type Level struct {
	kind  ConsistencyKind
	bound time.Duration // BoundedStaleness
	wait  time.Duration // ReadYourWrites: how long to wait for a replica
	fail  bool          // ReadYourWrites: fail instead of reading the primary
}

// ConsistencyKind is what a Level asks for, without its parameters.
type ConsistencyKind uint8

const (
	// PrimaryKind reads the primary.
	PrimaryKind ConsistencyKind = iota + 1
	// AnyKind reads any replica in rotation.
	AnyKind
	// BoundedStalenessKind reads a replica no more than a bound behind.
	BoundedStalenessKind
	// ReadYourWritesKind reads a replica that has replayed the causal floor.
	ReadYourWritesKind
)

var (
	// Primary always reads the primary.
	Primary = Level{kind: PrimaryKind}
	// Any reads a replica in rotation, chosen by the implementation's
	// strategy, or the primary when none is.
	Any = Level{kind: AnyKind}
	// ReadYourWrites reads a replica that has replayed everything the
	// request, or the requests whose causal token it carries, committed.
	// When none has, it reads the primary at once; Wait and OrFail change
	// that.
	ReadYourWrites = Level{kind: ReadYourWritesKind}
)

// BoundedStaleness reads a replica whose tracked lag, plus the age of the
// reading it was tracked by, is at most d, or the primary when none is. A
// replica whose lag can't be told never qualifies. Lag is tracked by
// polling, so even a caught-up replica may be about a poll behind, and up
// to the tracker's staleness bound when polls are slow: a d below about
// one poll reads the primary, and a d of zero always does. A negative d
// fails the read with InvalidArgument when it begins.
func BoundedStaleness(d time.Duration) Level {
	return Level{kind: BoundedStalenessKind, bound: d}
}

// Wait makes a ReadYourWrites read wait up to d for a replica to catch up
// when none has, before it reads the primary (or fails, with OrFail).
func (l Level) Wait(d time.Duration) Level { l.wait = d; return l }

// OrFail makes a ReadYourWrites read fail, once its wait is over, with
// Unavailable and CodeConsistencyTimeout instead of reading the primary, for
// reads that mustn't add to the primary's load. Without replicas there is
// nothing to wait for, and it reads the primary all the same.
func (l Level) OrFail() Level { l.fail = true; return l }

// Kind is what l asks for.
func (l Level) Kind() ConsistencyKind { return l.kind }

// Staleness is a BoundedStaleness level's bound.
func (l Level) Staleness() time.Duration { return l.bound }

// MaxWait is how long a ReadYourWrites read waits for a replica.
func (l Level) MaxWait() time.Duration { return l.wait }

// FailsOnTimeout reports whether a ReadYourWrites read fails, rather than
// reads the primary, when no replica has caught up within MaxWait.
func (l Level) FailsOnTimeout() bool { return l.fail }

// Validate reports what is wrong with l: the zero Level, a negative bound
// or wait, or Wait or OrFail on a level other than ReadYourWrites.
func (l Level) Validate() error {
	switch {
	case l.kind < PrimaryKind || l.kind > ReadYourWritesKind:
		return errors.New("unknown read consistency (use data.Primary, data.Any, data.BoundedStaleness or data.ReadYourWrites)")
	case l.bound < 0:
		return fmt.Errorf("the bound of %s must not be negative", l)
	case l.wait < 0:
		return fmt.Errorf("the wait of %s must not be negative", l)
	case l.kind != ReadYourWritesKind && (l.wait != 0 || l.fail):
		return fmt.Errorf("Wait and OrFail apply to data.ReadYourWrites, not to %s", Level{kind: l.kind, bound: l.bound})
	}
	return nil
}

// ConsistencyLevel names l, which makes it an op.ReadConsistency, so an
// operation can declare it as its reads' default.
func (l Level) ConsistencyLevel() string { return l.String() }

// String names l as ParseLevel reads it, such as "bounded_staleness(2s)" or
// "read_your_writes(wait 200ms, or fail)".
func (l Level) String() string {
	switch l.kind {
	case PrimaryKind:
		return "primary"
	case AnyKind:
		return "any"
	case BoundedStalenessKind:
		return "bounded_staleness(" + l.bound.String() + ")"
	case ReadYourWritesKind:
		var opts []string
		if l.wait > 0 {
			opts = append(opts, "wait "+l.wait.String())
		}
		if l.fail {
			opts = append(opts, "or fail")
		}
		if len(opts) == 0 {
			return "read_your_writes"
		}
		return "read_your_writes(" + strings.Join(opts, ", ") + ")"
	}
	return fmt.Sprintf("consistency(%d)", uint8(l.kind))
}

// ParseLevel reads a level as String names it: "primary", "any",
// "bounded_staleness(<duration>)", or "read_your_writes", optionally with
// "(wait <duration>)", "(or fail)" or "(wait <duration>, or fail)".
func ParseLevel(s string) (Level, error) {
	s = strings.TrimSpace(s)
	name, args, hasArgs := strings.Cut(s, "(")
	if hasArgs {
		var ok bool
		if args, ok = strings.CutSuffix(args, ")"); !ok {
			return Level{}, fmt.Errorf("read consistency %q: missing )", s)
		}
	}
	var l Level
	switch strings.TrimSpace(name) {
	case "primary":
		l = Primary
	case "any":
		l = Any
	case "bounded_staleness":
		d, err := time.ParseDuration(strings.TrimSpace(args))
		if err != nil {
			return Level{}, fmt.Errorf("read consistency %q: bounded_staleness takes a duration, such as bounded_staleness(2s)", s)
		}
		l = BoundedStaleness(d)
		return l, l.Validate()
	case "read_your_writes":
		l = ReadYourWrites
		if !hasArgs {
			return l, nil
		}
		for _, opt := range strings.Split(args, ",") {
			opt = strings.TrimSpace(opt)
			switch {
			case opt == "or fail":
				l.fail = true
			case strings.HasPrefix(opt, "wait "):
				d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(opt, "wait ")))
				if err != nil {
					return Level{}, fmt.Errorf("read consistency %q: wait takes a duration, such as wait 200ms", s)
				}
				l.wait = d
			default:
				return Level{}, fmt.Errorf("read consistency %q: read_your_writes takes \"wait <duration>\" and \"or fail\"", s)
			}
		}
		return l, l.Validate()
	default:
		return Level{}, fmt.Errorf("unknown read consistency %q: use primary, any, bounded_staleness(<duration>) or read_your_writes", s)
	}
	if hasArgs {
		return Level{}, fmt.Errorf("read consistency %q takes no arguments", name)
	}
	return l, nil
}

// consistencyOption is a level as a read option.
type consistencyOption struct{ l Level }

func (c consistencyOption) applyRead(o *Options) { l := c.l; o.Consistency = &l }

// Consistency sets the read consistency of a read transaction. It is a read
// option only: writes always run on the primary.
func Consistency(l Level) ReadOption { return consistencyOption{l} }

type consistencyKey struct{}

// WithConsistency returns a ctx whose reads default to l when they don't
// ask for a level themselves. The data module sets it from the operation's
// descriptor. A read with an l that fails Validate fails with
// InvalidArgument and CodeInvalidConsistency.
func WithConsistency(ctx context.Context, l Level) context.Context {
	return context.WithValue(ctx, consistencyKey{}, l)
}

// ConsistencyFrom returns the default level ctx carries, if any.
func ConsistencyFrom(ctx context.Context) (Level, bool) {
	l, ok := ctx.Value(consistencyKey{}).(Level)
	return l, ok
}
