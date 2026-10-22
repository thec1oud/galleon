package coordpg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thec1oud/galleon/coord"
)

type membership struct{ b *Backend }

func (m membership) Self() coord.NodeID { return m.b.node }

// Members lists the nodes whose last heartbeat is younger than
// Config.FailureDetection.
func (m membership) Members(ctx context.Context) ([]coord.Member, error) {
	if err := m.b.ready(); err != nil {
		return nil, err
	}
	rows, err := m.b.pool.Query(ctx, m.b.q.sql(`SELECT node, roles FROM {members} WHERE expires_at > now() ORDER BY node`))
	if err != nil {
		return nil, m.b.fail(ctx, err)
	}
	defer rows.Close()
	var out []coord.Member
	for rows.Next() {
		var node string
		var roles []string
		if err := rows.Scan(&node, &roles); err != nil {
			return nil, m.b.fail(ctx, err)
		}
		if len(roles) == 0 {
			roles = nil
		}
		out = append(out, coord.Member{ID: coord.NodeID(node), Roles: roles})
	}
	if err := rows.Err(); err != nil {
		return nil, m.b.fail(ctx, err)
	}
	return out, nil
}

// heartbeat renews this node's membership.
func (b *Backend) heartbeat(ctx context.Context) error {
	roles := b.roles
	if roles == nil {
		roles = []string{}
	}
	_, err := b.pool.Exec(ctx, b.q.sql(`
INSERT INTO {members} (node, roles, expires_at) VALUES ($1, $2, now() + $3::bigint * interval '1 microsecond')
ON CONFLICT (node) DO UPDATE SET roles = excluded.roles, expires_at = excluded.expires_at`),
		string(b.node), roles, micros(b.cfg.FailureDetection))
	return err
}

// leave removes this node from membership.
func (b *Backend) leave(ctx context.Context) error {
	if _, err := b.pool.Exec(ctx, b.q.sql(`DELETE FROM {members} WHERE node = $1`), string(b.node)); err != nil {
		return b.fail(ctx, err)
	}
	return nil
}

func (b *Backend) heartbeats(ctx context.Context) {
	defer b.loops.Done()
	t := time.NewTicker(b.cfg.Heartbeat)
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		hctx, cancel := context.WithTimeout(ctx, b.cfg.Heartbeat)
		err := b.heartbeat(hctx)
		cancel()
		switch {
		case err != nil && ctx.Err() == nil && !failing:
			b.log.Warn("coordination heartbeat failed; other nodes will take this one for dead if it keeps failing",
				"error", err, "failure_detection", b.cfg.FailureDetection)
			failing = true
		case err == nil && failing:
			b.log.Info("coordination heartbeat recovered")
			failing = false
		}
	}
}

// sweepEvery is how often the sweeper looks for expired keys when it knows
// of none sooner, and pruneEvery how often it prunes what has outlived its
// use.
const (
	sweepEvery = time.Second
	pruneEvery = 30 * time.Second
)

// sweeper deletes expired keys, with their delete events, and prunes old
// history, counter windows and members.
func (b *Backend) sweeper(ctx context.Context) {
	defer b.loops.Done()
	pruned := time.Now()
	wait := time.Duration(0)
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-b.sweep:
			t.Stop()
		}
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		next, err := b.expireKeys(sctx)
		if err == nil && time.Since(pruned) >= pruneEvery {
			err = b.prune(sctx)
			pruned = time.Now()
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			b.log.Warn("coordination sweep failed", "error", err)
		}
		wait = sweepEvery
		if next >= 0 && next < wait {
			wait = next
		}
	}
}

// prune deletes key-value history older than Config.History, counter
// windows that ended over a minute ago, and members that stopped over a
// minute ago.
func (b *Backend) prune(ctx context.Context) error {
	batch := &pgx.Batch{}
	batch.Queue(b.q.sql(`DELETE FROM {history} WHERE at < now() - $1::bigint * interval '1 microsecond'`), micros(b.cfg.History))
	batch.Queue(b.q.sql(`DELETE FROM {counters} WHERE window_start + window_us < (extract(epoch FROM now()) * 1000000)::bigint - 60000000`))
	batch.Queue(b.q.sql(`DELETE FROM {members} WHERE expires_at < now() - interval '1 minute'`))
	return b.pool.SendBatch(ctx, batch).Close()
}
