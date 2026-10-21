package migrate

// OnRenewed makes r call f after it has handled each lease renewal, so
// tests can advance a fake clock knowing the renewal's effect is in place.
func OnRenewed(r *Runner, f func(err error)) { r.renewed = f }
