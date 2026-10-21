package migrate

import "time"

// OnRenewed makes r call f after it has handled each lease renewal, so
// tests can advance a fake clock knowing the renewal's effect is in place.
func OnRenewed(r *Runner, f func(err error)) { r.renewed = f }

// OnSleeping makes r call f once each pause's timer is armed on its clock,
// so tests advance a fake clock only when the pause is waiting on it.
func OnSleeping(r *Runner, f func(d time.Duration)) { r.sleeping = f }
