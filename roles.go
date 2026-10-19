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

// Active reports whether this process runs role. Asking also declares the role
// as known, so a configured role that no module asks about fails boot as a typo.
func (r *Registry) Active(role Role) bool {
	r.app.knownRoles[role] = true
	return r.app.runs(role)
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

// notifySupervisorWhenReady reports readiness to a supervisor the first time
// every readiness gate passes, so an upgrade only retires the old generation
// once this one can serve.
func (a *App) notifySupervisorWhenReady() {
	if !process.Supervised() {
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
				if err := process.NotifyReady(); err != nil {
					a.log.Error("reporting readiness to the supervisor failed", "error", err)
				}
				return
			}
			<-t.C
		}
	}()
}
