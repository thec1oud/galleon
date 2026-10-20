package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
)

type scopeKey struct{}

// scope is the root transactions one operation call began and hasn't ended.
// Once the call has ended it is closed: a transaction begun later through a
// ctx that still carries it, such as one a goroutine kept with
// context.WithoutCancel, is background work, and is no more tracked than
// one begun outside any operation.
type scope struct {
	mu     sync.Mutex
	open   []*handle
	closed bool
}

// add tracks h, and reports whether it did.
func (s *scope) add(h *handle) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.open = append(s.open, h)
	return true
}

func (s *scope) remove(h *handle) {
	s.mu.Lock()
	for i, x := range s.open {
		if x == h {
			s.open = append(s.open[:i], s.open[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
}

func (s *scope) take() []*handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	open := s.open
	s.open, s.closed = nil, true
	return open
}

// track runs the rest of an operation's pipeline, then rolls back and
// reports every root transaction the call began and left open. The module
// installs it as the outermost interceptor, so the op plane needs nothing of
// this package.
//
// It also gives the call its causal floor, raised by the token the call
// brought and by every write it commits, and its operation's default read
// consistency, and once the call is over sends a token for the floor if
// it committed a write, whether it then failed or not.
func (m *Manager) track(ctx context.Context, call *op.Call, next op.Invoker) (any, error) {
	s := &scope{}
	c := &causal{}
	m.takeToken(ctx, call, c)
	ictx := context.WithValue(context.WithValue(ctx, scopeKey{}, s), causalKey{}, c)
	if l, ok := call.Desc.Consistency.(data.Level); ok {
		ictx = data.WithConsistency(ictx, l)
	}
	out, err := next(ictx, call)
	leaked := s.take()
	// Leaked transactions are rolled back below, and give no token.
	m.giveToken(call, c)
	if len(leaked) == 0 {
		return out, err
	}
	var report strings.Builder
	for i, h := range leaked {
		rerr := h.rollback(ctx)
		stack := formatStack(h.root.began)
		m.logger().ErrorContext(ctx, "transaction still open when its operation ended; rolled back",
			"operation", call.Desc.ID, "source", h.root.src.name, "began", stack, slog.Any("rollback_error", rerr))
		if i > 0 {
			report.WriteString("\n")
		}
		fmt.Fprintf(&report, "a %s transaction on source %q began at:\n%s", access(h.write), h.root.src.name, stack)
	}
	if m.leaks == leaksLog {
		return out, err
	}
	cause := fmt.Errorf("%d transaction(s) still open when operation %q ended were rolled back; %s", len(leaked), call.Desc.ID, report.String())
	if err != nil {
		cause = errors.Join(cause, err)
	}
	return nil, errs.Wrap(cause, errs.Internal, data.CodeTxLeaked, "a transaction was left open")
}

func access(write bool) string {
	if write {
		return "write"
	}
	return "read"
}
