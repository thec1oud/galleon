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
		r.app.knownRoles[role] = true
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
// upgrade only retires the old generation once this one serves.
func (a *App) watchReadiness() {
	if len(a.readyHooks) == 0 && !process.Supervised() {
		return
	}
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			a.mu.Lock()
			st := a.state
			a.mu.Unlock()
			if st != stateStarted {
				return
			}
			if a.Ready(context.Background()) == nil {
				break
			}
			<-t.C
		}
		for _, h := range a.readyHooks {
			hctx, cancel := context.WithTimeout(context.Background(), a.settings.startTimeout)
			err := h.fn(hctx)
			cancel()
			if err != nil {
				// Not reporting ready lets a supervisor roll the upgrade back.
				a.log.Error("ready hook failed; not reporting ready", "module", h.module, "site", h.site, "error", err)
				return
			}
		}
		if err := process.NotifyReady(); err != nil {
			a.log.Error("reporting readiness to the supervisor failed", "error", err)
		}
	}()
}
