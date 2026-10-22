package coordpg

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/errs"
)

// grant takes a free or expired lease row, adding one to its token, and
// tells waiters and observers. When the key is held, it returns no token,
// and how many microseconds the holder's lease has left, if the
// statement's snapshot has the row.
const grant = `
WITH got AS (
	INSERT INTO {leases} AS l (key, token, holder, node, expires_at)
	VALUES ($1, 1, $2, $3, now() + $4::bigint * interval '1 microsecond')
	ON CONFLICT (key) DO UPDATE
		SET token = l.token + 1, holder = excluded.holder, node = excluded.node, expires_at = excluded.expires_at
		WHERE l.holder IS NULL OR l.expires_at <= now()
	RETURNING token
), told AS (
	SELECT pg_notify($5, $6) FROM got
)
SELECT (SELECT token FROM got), (SELECT count(*) FROM told),
	(SELECT (extract(epoch FROM expires_at - now()) * 1000000)::bigint FROM {leases}
		WHERE key = $1 AND NOT EXISTS (SELECT 1 FROM got))`

// renew extends a lease its holder still has.
const renew = `
UPDATE {leases} SET expires_at = now() + $3::bigint * interval '1 microsecond'
WHERE key = $1 AND holder = $2 AND expires_at > now()`

// release frees a lease its holder still has, and tells waiters and
// observers.
const release = `
WITH freed AS (
	UPDATE {leases} SET holder = NULL, node = NULL, expires_at = NULL
	WHERE key = $1 AND holder = $2
	RETURNING key
)
SELECT count(*) FROM (SELECT pg_notify($3, $4) FROM freed) told`

type locker struct{ b *Backend }

func (lk locker) TryAcquire(ctx context.Context, key string, ttl time.Duration) (coord.Lease, bool, error) {
	if err := lk.b.ready(); err != nil {
		return nil, false, err
	}
	if err := checkTTL(ttl); err != nil {
		return nil, false, err
	}
	l, _, err := lk.b.tryAcquire(ctx, key, ttl)
	if err != nil || l == nil {
		return nil, false, err
	}
	return l, true, nil
}

func (lk locker) Acquire(ctx context.Context, key string, ttl time.Duration) (coord.Lease, error) {
	if err := lk.b.ready(); err != nil {
		return nil, err
	}
	if err := checkTTL(ttl); err != nil {
		return nil, err
	}
	l, err := lk.b.acquire(ctx, key, ttl)
	if err != nil {
		return nil, err
	}
	return l, nil
}

func checkTTL(ttl time.Duration) error {
	if ttl < time.Millisecond {
		return errs.New(errs.InvalidArgument, CodeInvalidArgument, "a lease's ttl must be at least 1ms")
	}
	return nil
}

// maxWait bounds a wait for a lock or a leader change, in case a
// notification is lost in a way the listener can't see.
const maxWait = 5 * time.Second

// acquire waits for key, woken when its row changes or its holder's lease
// runs out.
func (b *Backend) acquire(ctx context.Context, key string, ttl time.Duration) (*lease, error) {
	h := keyHash(key)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		woken := b.waitFor(h)
		l, left, err := b.tryAcquire(ctx, key, ttl)
		if err != nil {
			return nil, err
		}
		if l != nil {
			return l, nil
		}
		wait := maxWait
		if left >= 0 && left < wait {
			wait = left + time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-woken:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		}
		t.Stop()
		if b.closed() {
			return nil, coord.ErrClosed
		}
	}
}

// tryAcquire grants the lease if key is free. Otherwise it returns how long
// the holder's lease has left, or -1 if it can't tell.
func (b *Backend) tryAcquire(ctx context.Context, key string, ttl time.Duration) (*lease, time.Duration, error) {
	holder := randomHex(16)
	start := time.Now()
	var token, left *int64
	var told int64
	err := b.pool.QueryRow(ctx, b.q.sql(grant), key, holder, string(b.node), micros(ttl), b.events, "l"+keyHash(key)).
		Scan(&token, &told, &left)
	if err != nil {
		return nil, 0, b.fail(ctx, err)
	}
	if token == nil {
		if left == nil {
			return nil, -1, nil
		}
		return nil, max(time.Duration(*left)*time.Microsecond, 0), nil
	}
	l := &lease{b: b, key: key, holder: holder, token: uint64(*token), ttl: ttl, lost: make(chan struct{})}
	b.mu.Lock()
	if b.state != stateStarted {
		b.mu.Unlock()
		// Stopped while granting: give it straight back.
		_ = l.free(context.WithoutCancel(ctx))
		close(l.lost)
		return nil, 0, coord.ErrClosed
	}
	b.leases[l] = struct{}{}
	b.mu.Unlock()
	l.extend(start)
	return l, 0, nil
}

// lease is a held lease row. Its deadline is measured on this node's
// monotonic clock from just before the statement that granted or last
// renewed it, so it falls before the row's expiry on the database's clock.
type lease struct {
	b      *Backend
	key    string
	holder string
	token  uint64
	ttl    time.Duration
	lost   chan struct{}

	mu       sync.Mutex
	deadline time.Time
	timer    *time.Timer
	ended    bool
}

func (l *lease) Key() string           { return l.key }
func (l *lease) Token() uint64         { return l.token }
func (l *lease) Epoch() string         { return l.b.epoch }
func (l *lease) Lost() <-chan struct{} { return l.lost }

// guard is how long before its deadline a lease that wasn't renewed is
// released, so the release is done before the deadline.
func (l *lease) guard() time.Duration { return min(l.ttl/10, 100*time.Millisecond) }

// extend moves the deadline to start plus the TTL.
func (l *lease) extend(start time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.deadline = start.Add(l.ttl)
	at := time.Until(l.deadline) - l.guard()
	if l.timer == nil {
		l.timer = time.AfterFunc(at, l.expire)
	} else {
		l.timer.Reset(at)
	}
}

// expire runs from the timer. A Renew may have moved the deadline since it
// was set.
func (l *lease) expire() {
	l.mu.Lock()
	if l.ended {
		l.mu.Unlock()
		return
	}
	if at := time.Until(l.deadline) - l.guard(); at > 0 {
		l.timer.Reset(at)
		l.mu.Unlock()
		return
	}
	l.ended = true
	deadline := l.deadline
	l.mu.Unlock()
	// Free the key before Lost closes, but never close Lost after the
	// deadline: a release that can't finish by then is left to the row's
	// expiry, which is later.
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	_ = l.free(ctx)
	cancel()
	l.end()
}

// end closes Lost and forgets the lease. The caller has set ended.
func (l *lease) end() {
	l.mu.Lock()
	if l.timer != nil {
		l.timer.Stop()
	}
	l.mu.Unlock()
	close(l.lost)
	l.b.mu.Lock()
	delete(l.b.leases, l)
	l.b.mu.Unlock()
}

func (l *lease) Renew(ctx context.Context) error {
	l.mu.Lock()
	ended := l.ended
	l.mu.Unlock()
	if ended {
		return coord.ErrLeaseLost
	}
	if err := l.b.ready(); err != nil {
		return err
	}
	start := time.Now()
	tag, err := l.b.pool.Exec(ctx, l.b.q.sql(renew), l.key, l.holder, micros(l.ttl))
	if err != nil {
		return l.b.fail(ctx, err)
	}
	l.mu.Lock()
	if l.ended {
		l.mu.Unlock()
		return coord.ErrLeaseLost
	}
	if tag.RowsAffected() == 0 {
		// Expired on the database's clock, and maybe taken over.
		l.ended = true
		l.mu.Unlock()
		l.end()
		return coord.ErrLeaseLost
	}
	l.mu.Unlock()
	l.extend(start)
	return nil
}

func (l *lease) Release(ctx context.Context) error { return l.release(ctx) }

func (l *lease) release(ctx context.Context) error {
	l.mu.Lock()
	if l.ended {
		l.mu.Unlock()
		return nil
	}
	l.ended = true
	l.mu.Unlock()
	err := l.free(ctx)
	l.end()
	if err != nil {
		return l.b.fail(ctx, err)
	}
	return nil
}

// free frees the row, if this lease still holds it.
func (l *lease) free(ctx context.Context) error {
	var n int64
	return l.b.pool.QueryRow(ctx, l.b.q.sql(release), l.key, l.holder, l.b.events, "l"+keyHash(l.key)).Scan(&n)
}

// ---- leader election ----

type election struct {
	b    *Backend
	name string
}

func (e election) key() string { return "galleon/election/" + e.name }

// Campaign waits for leadership, and renews it every third of
// Config.LeaderTTL until it is released or lost, or the backend stops.
func (e election) Campaign(ctx context.Context) (coord.Lease, error) {
	if err := e.b.ready(); err != nil {
		return nil, err
	}
	ttl := e.b.cfg.LeaderTTL
	l, err := e.b.acquire(ctx, e.key(), ttl)
	if err != nil {
		return nil, err
	}
	go func() {
		t := time.NewTicker(ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-l.Lost():
				return
			case <-t.C:
				// A renewal that fails for another reason is tried again
				// at the next tick; the deadline ends the lease if none
				// succeeds in time.
				rctx, cancel := context.WithTimeout(context.Background(), ttl/3)
				err := l.Renew(rctx)
				cancel()
				if errors.Is(err, coord.ErrLeaseLost) || errors.Is(err, coord.ErrClosed) {
					return
				}
			}
		}
	}()
	return l, nil
}

// leader reads who leads, and how long their lease has left.
func (e election) leader(ctx context.Context) (coord.NodeID, time.Duration, error) {
	var node string
	var left int64
	err := e.b.pool.QueryRow(ctx, e.b.q.sql(`
SELECT node, (extract(epoch FROM expires_at - now()) * 1000000)::bigint FROM {leases}
WHERE key = $1 AND holder IS NOT NULL AND expires_at > now()`), e.key()).Scan(&node, &left)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", -1, nil
	}
	if err != nil {
		return "", 0, e.b.fail(ctx, err)
	}
	return coord.NodeID(node), time.Duration(left) * time.Microsecond, nil
}

func (e election) Leader(ctx context.Context) (coord.NodeID, bool, error) {
	if err := e.b.ready(); err != nil {
		return "", false, err
	}
	id, _, err := e.leader(ctx)
	return id, id != "", err
}

// Observe reads the leader whenever its lease row changes or runs out. A
// read that fails is tried again after a second.
func (e election) Observe(ctx context.Context) <-chan coord.NodeID {
	out := make(chan coord.NodeID)
	if e.b.ready() != nil {
		close(out)
		return out
	}
	h := keyHash(e.key())
	go func() {
		defer close(out)
		first := true
		var last coord.NodeID
		for {
			woken := e.b.waitFor(h)
			id, left, err := e.leader(ctx)
			wait := maxWait
			switch {
			case err != nil:
				if ctx.Err() != nil || errors.Is(err, coord.ErrClosed) {
					return
				}
				wait = time.Second
			case first || id != last:
				select {
				case out <- id:
				case <-ctx.Done():
					return
				}
				first, last = false, id
				continue // read again: the leader may have changed while we were sending
			case left >= 0 && left < wait:
				wait = left + time.Millisecond
			}
			t := time.NewTimer(wait)
			select {
			case <-woken:
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return
			}
			t.Stop()
			if e.b.closed() {
				return
			}
		}
	}()
	return out
}
