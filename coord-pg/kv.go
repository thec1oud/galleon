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

// Every change to the store first locks meta's row, in the same implicit
// transaction as the change, so changes take revisions in the order they
// commit. The statement after the lock reads with a snapshot taken once it
// is held, so its conditions see every change committed before.
const lockRevision = `SELECT kv_revision FROM {meta} FOR UPDATE`

// A key with an expiry in the past is absent, whether or not the sweeper
// has deleted it yet.
const live = `(expires_at IS NULL OR expires_at > now())`

// put writes a key when its condition holds: $5 is 0 for none, 1 for
// IfAbsent and 2 for IfRevision($6). It returns no revision when the
// condition fails.
const put = `
WITH cur AS (
	SELECT revision FROM {kv} WHERE key = $1 AND ` + live + `
), ok AS (
	SELECT 1 WHERE CASE $5::int
		WHEN 1 THEN NOT EXISTS (SELECT 1 FROM cur)
		WHEN 2 THEN EXISTS (SELECT 1 FROM cur WHERE revision = $6)
		ELSE true END
), rev AS (
	UPDATE {meta} SET kv_revision = kv_revision + 1 WHERE EXISTS (SELECT 1 FROM ok) RETURNING kv_revision AS r
), wrote AS (
	INSERT INTO {kv} (key, value, revision, expires_at)
	SELECT $1, $2, r, CASE WHEN $3::bigint > 0 THEN now() + $3::bigint * interval '1 microsecond' END FROM rev
	ON CONFLICT (key) DO UPDATE SET value = excluded.value, revision = excluded.revision, expires_at = excluded.expires_at
	RETURNING revision
), logged AS (
	INSERT INTO {history} (revision, key, value, deleted) SELECT revision, $1, $2, false FROM wrote
), told AS (
	SELECT pg_notify($4, $7) FROM wrote
)
SELECT (SELECT revision FROM wrote), (SELECT count(*) FROM told)`

// del deletes a live key, if its revision is $3 when that isn't 0. It
// reports whether the key was live, and whether it was deleted.
const del = `
WITH cur AS (
	SELECT revision FROM {kv} WHERE key = $1 AND ` + live + `
), ok AS (
	SELECT 1 FROM cur WHERE $3::bigint = 0 OR revision = $3
), rev AS (
	UPDATE {meta} SET kv_revision = kv_revision + 1 WHERE EXISTS (SELECT 1 FROM ok) RETURNING kv_revision AS r
), gone AS (
	DELETE FROM {kv} WHERE key = $1 AND EXISTS (SELECT 1 FROM rev) RETURNING key
), logged AS (
	INSERT INTO {history} (revision, key, value, deleted) SELECT r, $1, NULL, true FROM rev, gone
), told AS (
	SELECT pg_notify($2, $4) FROM gone
)
SELECT EXISTS (SELECT 1 FROM cur), (SELECT count(*) FROM gone), (SELECT count(*) FROM told)`

// expire deletes the keys whose expiry has passed, each with a revision of
// its own, and returns how many there were.
const expire = `
WITH gone AS (
	DELETE FROM {kv} WHERE expires_at <= now() RETURNING key
), numbered AS (
	SELECT key, row_number() OVER (ORDER BY key) AS i FROM gone
), base AS (
	SELECT kv_revision AS r FROM {meta}
), logged AS (
	INSERT INTO {history} (revision, key, value, deleted) SELECT r + i, key, NULL, true FROM numbered, base
), rev AS (
	UPDATE {meta} SET kv_revision = kv_revision + (SELECT count(*) FROM gone) WHERE EXISTS (SELECT 1 FROM gone)
), told AS (
	SELECT pg_notify($1, CASE WHEN octet_length(key) <= $2 THEN 'k' || key ELSE 'k' END) FROM numbered
)
SELECT count(*) FROM told`

// maxNotifyKey is the longest key a key-value notification names; a
// longer one's notification wakes every watcher.
const maxNotifyKey = 7000

func kvPayload(key string) string {
	if len(key) > maxNotifyKey {
		return "k"
	}
	return "k" + key
}

type kv struct{ b *Backend }

func (k kv) Get(ctx context.Context, key string) (coord.Entry, error) {
	if err := k.b.ready(); err != nil {
		return coord.Entry{}, err
	}
	var e coord.Entry
	var rev int64
	err := k.b.pool.QueryRow(ctx, k.b.q.sql(`SELECT value, revision FROM {kv} WHERE key = $1 AND `+live), key).Scan(&e.Value, &rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return coord.Entry{}, coord.ErrNotFound
	}
	if err != nil {
		return coord.Entry{}, k.b.fail(ctx, err)
	}
	e.Key, e.Revision = key, uint64(rev)
	return e, nil
}

func (k kv) Put(ctx context.Context, key string, value []byte, opts ...coord.PutOption) (uint64, error) {
	if err := k.b.ready(); err != nil {
		return 0, err
	}
	o := coord.ApplyPutOptions(opts)
	if o.TTL < 0 {
		return 0, errs.New(errs.InvalidArgument, CodeInvalidArgument, "a key's ttl must not be negative")
	}
	if o.TTL > 0 && o.TTL < time.Microsecond {
		o.TTL = time.Microsecond
	}
	cond := 0
	switch {
	case o.IfAbsent:
		cond = 1
	case o.IfRevision != 0:
		cond = 2
	}
	if value == nil {
		value = []byte{}
	}
	batch := &pgx.Batch{}
	batch.Queue(k.b.q.sql(lockRevision))
	var rev *int64
	var told int64
	batch.Queue(k.b.q.sql(put), key, value, micros(o.TTL), k.b.events, cond, int64(o.IfRevision), kvPayload(key)).
		QueryRow(func(row pgx.Row) error { return row.Scan(&rev, &told) })
	if err := k.b.pool.SendBatch(ctx, batch).Close(); err != nil {
		return 0, k.b.fail(ctx, err)
	}
	if rev == nil {
		return 0, coord.ErrRevisionMismatch
	}
	if o.TTL > 0 {
		k.b.sweepAt(o.TTL)
	}
	return uint64(*rev), nil
}

func (k kv) Delete(ctx context.Context, key string, opts ...coord.PutOption) error {
	if err := k.b.ready(); err != nil {
		return err
	}
	o := coord.ApplyPutOptions(opts)
	batch := &pgx.Batch{}
	batch.Queue(k.b.q.sql(lockRevision))
	var existed bool
	var gone, told int64
	batch.Queue(k.b.q.sql(del), key, k.b.events, int64(o.IfRevision), kvPayload(key)).
		QueryRow(func(row pgx.Row) error { return row.Scan(&existed, &gone, &told) })
	if err := k.b.pool.SendBatch(ctx, batch).Close(); err != nil {
		return k.b.fail(ctx, err)
	}
	if o.IfRevision != 0 && gone == 0 {
		return coord.ErrRevisionMismatch
	}
	return nil
}

// sweepAt has the sweeper run once d has passed, for a key this node wrote
// with a TTL of d.
func (b *Backend) sweepAt(d time.Duration) {
	time.AfterFunc(d, func() {
		select {
		case b.sweep <- struct{}{}:
		default:
		}
	})
}

// expireKeys deletes the expired keys, and returns how long until the next
// one expires, or -1 if none will.
func (b *Backend) expireKeys(ctx context.Context) (time.Duration, error) {
	var due bool
	var next *int64
	err := b.pool.QueryRow(ctx, b.q.sql(`
SELECT EXISTS (SELECT 1 FROM {kv} WHERE expires_at <= now()),
	(SELECT (extract(epoch FROM min(expires_at) - now()) * 1000000)::bigint FROM {kv} WHERE expires_at > now())`)).
		Scan(&due, &next)
	if err != nil {
		return -1, err
	}
	if due {
		// Only take the revision lock when there is something to delete.
		batch := &pgx.Batch{}
		batch.Queue(b.q.sql(lockRevision))
		batch.Queue(b.q.sql(expire), b.events, maxNotifyKey)
		if err := b.pool.SendBatch(ctx, batch).Close(); err != nil {
			return -1, err
		}
	}
	if next == nil {
		return -1, nil
	}
	return time.Duration(*next) * time.Microsecond, nil
}

// watchBatch is how many changes a watcher reads at a time.
const watchBatch = 500

// watcher is one Watch. It reads the history after its cursor whenever a
// change of a key with its prefix is announced, or after maxWait in case an
// announcement was lost.
type watcher struct {
	b      *Backend
	prefix string
	signal chan struct{}
	done   chan struct{}
	once   sync.Once
}

func (w *watcher) wake() {
	select {
	case w.signal <- struct{}{}:
	default:
	}
}

func (w *watcher) end() { w.once.Do(func() { close(w.done) }) }

// keyChanged wakes the watchers of key; with no key, all of them.
func (b *Backend) keyChanged(key string, known bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for w := range b.watches {
		if !known || len(key) >= len(w.prefix) && key[:len(w.prefix)] == w.prefix {
			w.wake()
		}
	}
}

func (k kv) Watch(ctx context.Context, prefix string) (<-chan coord.Event, error) {
	if err := k.b.ready(); err != nil {
		return nil, err
	}
	w := &watcher{b: k.b, prefix: prefix, signal: make(chan struct{}, 1), done: make(chan struct{})}
	k.b.mu.Lock()
	k.b.watches[w] = struct{}{}
	k.b.mu.Unlock()
	forget := func() {
		k.b.mu.Lock()
		delete(k.b.watches, w)
		k.b.mu.Unlock()
	}
	var cursor int64
	if err := k.b.pool.QueryRow(ctx, k.b.q.sql(`SELECT kv_revision FROM {meta}`)).Scan(&cursor); err != nil {
		forget()
		return nil, k.b.fail(ctx, err)
	}
	out := make(chan coord.Event)
	go func() {
		defer close(out)
		defer forget()
		for {
			events, next, gap, err := w.read(ctx, cursor)
			switch {
			case err != nil && ctx.Err() != nil:
				return
			case err != nil:
				k.b.log.Warn("coordination watch read failed; retrying", "prefix", prefix, "error", err)
			case gap:
				k.b.log.Warn("coordination watch fell behind the kept history, and is closed", "prefix", prefix, "revision", cursor)
				return
			default:
				for _, ev := range events {
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					case <-w.done:
						return
					}
				}
				if next > cursor {
					cursor = next
					if len(events) == watchBatch {
						continue
					}
				}
			}
			wait := maxWait
			if err != nil {
				wait = time.Second
			}
			t := time.NewTimer(wait)
			select {
			case <-w.signal:
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return
			case <-w.done:
				t.Stop()
				return
			}
			t.Stop()
		}
	}()
	return out, nil
}

// read returns the changes of the watcher's keys after cursor, and the
// revision it has read up to. gap reports that history after cursor has
// been pruned, so changes were missed. Every revision has a row of
// history until it is pruned, so the oldest row tells.
func (w *watcher) read(ctx context.Context, cursor int64) (events []coord.Event, next int64, gap bool, err error) {
	rows, err := w.b.pool.Query(ctx, w.b.q.sql(`
WITH top AS (SELECT kv_revision AS h FROM {meta}),
	low AS (SELECT min(revision) AS m FROM {history})
SELECT top.h, low.m, c.revision, c.key, c.value, c.deleted
FROM top CROSS JOIN low LEFT JOIN LATERAL (
	SELECT revision, key, value, deleted FROM {history}
	WHERE revision > $1 AND revision <= top.h AND starts_with(key, $2)
	ORDER BY revision LIMIT $3
) c ON true`), cursor, w.prefix, watchBatch)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()
	next = cursor
	for rows.Next() {
		var top int64
		var low, rev *int64
		var key *string
		var value []byte
		var deleted *bool
		if err := rows.Scan(&top, &low, &rev, &key, &value, &deleted); err != nil {
			return nil, 0, false, err
		}
		if top > cursor && (low == nil || *low > cursor+1) {
			return nil, 0, true, nil
		}
		next = top
		if rev == nil {
			continue
		}
		ev := coord.Event{Type: coord.EventPut, Entry: coord.Entry{Key: *key, Value: value, Revision: uint64(*rev)}}
		if *deleted {
			ev.Type, ev.Entry.Value = coord.EventDelete, nil
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	if len(events) == watchBatch {
		// More may follow: read on from the last one returned.
		next = int64(events[len(events)-1].Entry.Revision)
	}
	return events, next, false, nil
}

// ---- counters ----

type counters struct{ b *Backend }

// Add counts in windows aligned to the Unix epoch on the database's clock,
// so every node agrees on when a window starts.
func (c counters) Add(ctx context.Context, key string, delta int64, window time.Duration) (int64, error) {
	if err := c.b.ready(); err != nil {
		return 0, err
	}
	if window < time.Microsecond {
		return 0, errs.New(errs.InvalidArgument, CodeInvalidArgument, "a counter's window must be at least 1µs")
	}
	var n int64
	err := c.b.pool.QueryRow(ctx, c.b.q.sql(`
INSERT INTO {counters} AS c (key, window_us, window_start, value)
SELECT $1, $3::bigint, (floor(extract(epoch FROM now()) * 1000000 / $3::bigint) * $3::bigint)::bigint, $2::bigint
ON CONFLICT (key, window_us) DO UPDATE SET
	-- A statement that began in an earlier window than the row's counts in the row's.
	value = CASE WHEN c.window_start >= excluded.window_start THEN c.value + excluded.value ELSE excluded.value END,
	window_start = greatest(c.window_start, excluded.window_start)
RETURNING value`), key, delta, micros(window)).Scan(&n)
	if err != nil {
		return 0, c.b.fail(ctx, err)
	}
	return n, nil
}
