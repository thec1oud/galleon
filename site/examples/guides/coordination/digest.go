// Package digest is the sample behind the "Choose a coordination backend"
// guide: a module that relies on a cluster-wide lock.
package digest

import (
	"context"
	"log/slog"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord"
)

// docs:start module
// Digest sends the daily digest. However many nodes run it, the lock lets
// only one send at a time. It doesn't remember that the digest went out:
// a node that calls Send after the lock is released sends it again.
type Digest struct {
	locks coord.Locker
	log   *slog.Logger
}

// Module provides *Digest, and declares that it needs locks: boot fails on
// a backend that can't give them, or can give them only per process while
// the app runs as several.
func Module() galleon.Module {
	return galleon.ModuleFunc("digest", func(r *galleon.Registry) error {
		r.Needs(coord.Locks)
		galleon.Provide[*Digest](r, func(b coord.Backend, log *slog.Logger) *Digest {
			return &Digest{locks: b.Locks(), log: log}
		})
		return nil
	})
}

// Send sends the digest, unless another node is sending it: then it
// returns false and does nothing.
func (d *Digest) Send(ctx context.Context) (bool, error) {
	lease, ok, err := d.locks.TryAcquire(ctx, "digest.daily", time.Minute)
	if err != nil || !ok {
		return false, err
	}
	defer lease.Release(context.WithoutCancel(ctx))
	d.log.InfoContext(ctx, "digest sent")
	return true, nil
}

// docs:end module
