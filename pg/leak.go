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
type scope struct {
	mu   sync.Mutex
	open []*handle
}

func (s *scope) add(h *handle) {
	s.mu.Lock()
	s.open = append(s.open, h)
	s.mu.Unlock()
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
	s.open = nil
	return open
}

// track runs the rest of an operation's pipeline, then rolls back and
// reports every root transaction the call began and left open. The module
// installs it as the outermost interceptor, so the op plane needs nothing of
// this package.
func (m *Manager) track(ctx context.Context, call *op.Call, next op.Invoker) (any, error) {
	s := &scope{}
	out, err := next(context.WithValue(ctx, scopeKey{}, s), call)
	leaked := s.take()
	if len(leaked) == 0 {
		return out, err
	}
	var report strings.Builder
	for i, h := range leaked {
		rerr := h.rollback(ctx)
		stack := formatStack(h.root.began)
		m.log.ErrorContext(ctx, "transaction still open when its operation ended; rolled back",
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
