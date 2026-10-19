package galleon

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/thec1oud/galleon/process"
)

// Role is a part of the app a process runs, such as "web", "rpc", "worker",
// "scheduler" or "realtime". Every role runs the same kernel and modules;
// modules use Registry.Active to decide which planes to start.
type Role string

// RoleAll runs every role. It is the default.
const RoleAll Role = "all"

// Roles sets the roles this process runs.
func Roles(roles ...Role) Option {
	return func(s *settings) { s.roles = append(s.roles[:0:0], roles...) }
}

// Active reports whether this process runs any of roles. Asking also declares
// every role passed as known, so a configured role that no module asks about
// fails boot as a typo. Pass all the roles a plane serves in one call rather
// than combining calls with ||, which would stop declaring at the first match.
func (r *Registry) Active(roles ...Role) bool {
	active := false
	for _, role := range roles {
		if !r.app.sealed { // after New, roles can no longer be checked
			r.app.knownRoles[role] = true
		}
		active = active || r.app.runs(role)
	}
	return active
}

// Roles returns the roles this process runs.
func (a *App) Roles() []Role { return slices.Clone(a.settings.roles) }

func (a *App) runs(role Role) bool {
	return slices.Contains(a.settings.roles, RoleAll) || slices.Contains(a.settings.roles, role)
}

func (a *App) checkRoles() {
	for _, role := range a.settings.roles {
		if role != RoleAll && !a.knownRoles[role] {
			a.bootErrs = append(a.bootErrs, fmt.Errorf("role %q is not used by any module (typo?)", role))
		}
	}
}

// watchReadiness waits, after Start, until every readiness gate first passes,
// then runs the OnReady hooks and reports readiness to a supervisor, so an
// upgrade only retires the old generation once this one serves. Shutdown
// cancels it and waits for it, so no hook runs, or is still running, once
// draining has begun. A failing hook is fatal: the app stops reporting ready
// and Run returns the error, so a supervisor restarts the worker instead of
// leaving it running with nothing listening.
func (a *App) watchReadiness() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.mu.Lock()
	a.readyCancel, a.readyDone = cancel, done
	a.mu.Unlock()
	if len(a.readyHooks) == 0 && !process.Supervised() {
		cancel()
		close(done)
		return
	}
	go func() {
		defer close(done)
		defer cancel()
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for a.Ready(ctx) != nil {
			select {
			case <-t.C:
			case <-ctx.Done():
				return
			}
		}
		for _, h := range a.readyHooks {
			if ctx.Err() != nil {
				return
			}
			hctx, hcancel := context.WithTimeout(ctx, a.settings.startTimeout)
			err := h.fn(hctx)
			hcancel()
			if err != nil {
				a.die(fmt.Errorf("ready hook of module %q (%s): %w", h.module, h.site, err))
				return
			}
		}
		if err := process.NotifyReady(); err != nil {
			a.log.Error("reporting readiness to the supervisor failed", "error", err)
		}
	}()
}

// stopWatchingReadiness cancels watchReadiness and waits until it has returned.
func (a *App) stopWatchingReadiness() {
	a.mu.Lock()
	cancel, done := a.readyCancel, a.readyDone
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// die records a fatal error: readiness fails from now on, and Run shuts the
// app down and returns err.
func (a *App) die(err error) {
	a.log.Error("fatal", "error", err)
	a.mu.Lock()
	if a.fatalErr == nil {
		a.fatalErr = err
		close(a.fatal)
	}
	a.mu.Unlock()
}
