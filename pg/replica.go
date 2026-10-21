package pg

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
)

// replica is a streaming replica of the primary, and the latest reading the
// tracker took of it.
type replica struct {
	src     *source
	zone    string
	reading atomic.Pointer[reading]
	// rotating is whether the latest reading put it in rotation, for logs
	// and metrics; reads decide afresh from the reading itself.
	rotating atomic.Bool
}

// reading is what one poll found of a replica.
type reading struct {
	at      time.Time
	healthy bool
	reason  string // why it isn't healthy
	replay  uint64 // pg_last_wal_replay_lsn()
	// lagBytes is how far its replay is behind the primary's latest
	// reading, unknownBytes when the tracker has no fresh reading of the
	// primary; lag is how long ago, at the latest, the primary was where
	// the replica is, unknownLag when that can't be told.
	lagBytes int64
	lag      time.Duration
}

const (
	unknownLag   = time.Duration(math.MaxInt64)
	unknownBytes = int64(math.MaxInt64)
)

// Reasons a replica is out of rotation, as logs, spans and metrics give them.
const (
	reasonUnreachable   = "unreachable"
	reasonNotInRecovery = "not_in_recovery"
	reasonStale         = "stale_reading"
	reasonLagging       = "lagging"
	reasonReplicaError  = "replica_error"
)

// Why a read that might have used a replica read the primary instead.
const (
	fallbackNoReplica   = "no_replica"
	fallbackWaitTimeout = "wait_timeout"
	fallbackError       = "replica_error"
)

// topology is the replicas, the settings that choose among them, and the
// tracker that keeps their readings.
type topology struct {
	replicas []*replica
	sel      Selection
	zone     string
	maxLag   time.Duration
	maxBytes int64
	poll     time.Duration
	rr       atomic.Uint64
	primary  history
	now      func() time.Time
	// layoutRetry is how long the primary's poller waits to try the WAL
	// layout again after it couldn't read it, and layoutTried when Start
	// last tried.
	layoutRetry time.Duration
	layoutTried time.Time

	life   sync.Mutex
	cancel context.CancelFunc
	done   sync.WaitGroup
}

// staleAfter is how old a reading may be before the replica it describes
// is taken out of rotation: the tracker stopped hearing from it.
func (t *topology) staleAfter() time.Duration { return max(5*t.poll, 500*time.Millisecond) }

// history is the primary's recent WAL positions, oldest first, which tell
// how long ago the primary was where a replica is.
type history struct {
	mu      sync.Mutex
	samples []sample
}

type sample struct {
	at  time.Time
	lsn uint64
}

const historyLen = 1024

func (h *history) add(s sample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.samples) == historyLen {
		h.samples = append(h.samples[:0], h.samples[1:]...)
	}
	h.samples = append(h.samples, s)
}

// latest is the newest sample, if any.
func (h *history) latest() (sample, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.samples) == 0 {
		return sample{}, false
	}
	return h.samples[len(h.samples)-1], true
}

// lag is how far a replica that has replayed replay, read at at, is behind
// the primary: in bytes against the latest sample, and in time since the
// newest sample it has passed, so even a replica past the latest is as far
// behind as that sample is old: about a poll, and up to fresh (the
// staleness bound) when the primary's poller is slow. Both are unknown without
// samples or when the latest is older than fresh: the tracker has stopped
// hearing from the primary, which may have moved on since, so the history
// tells nothing of the replica. The time is unknown too when it has passed
// no sample.
func (h *history) lag(replay uint64, at time.Time, fresh time.Duration) (bytes int64, lag time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(h.samples)
	if n == 0 {
		return unknownBytes, unknownLag
	}
	latest := h.samples[n-1]
	if at.Sub(latest.at) > fresh {
		return unknownBytes, unknownLag
	}
	if replay >= latest.lsn {
		// Caught up with the latest sample, which is itself as old as it
		// is: the primary may have written since.
		return 0, max(at.Sub(latest.at), 0)
	}
	bytes = int64(min(latest.lsn-replay, math.MaxInt64))
	for i := n - 1; i >= 0; i-- {
		if s := h.samples[i]; s.lsn <= replay {
			return bytes, max(at.Sub(s.at), 0)
		}
	}
	return bytes, unknownLag
}

// rotating reports whether rd puts its replica in rotation at now, and why
// not when it doesn't.
func (t *topology) rotating(rd *reading, now time.Time) (bool, string) {
	switch {
	case rd == nil:
		return false, reasonStale
	case !rd.healthy:
		return false, rd.reason
	case now.Sub(rd.at) > t.staleAfter():
		return false, reasonStale
	case rd.lag > t.maxLag || (t.maxBytes > 0 && rd.lagBytes > t.maxBytes):
		return false, reasonLagging
	}
	return true, ""
}

// candidate is a replica and the reading it was chosen by: each is loaded
// once, since Stop and the tracker replace readings at any time.
type candidate struct {
	r  *replica
	rd *reading
}

// candidates are the replicas that may serve a read at level l with causal
// floor floor, from their latest readings alone.
func (t *topology) candidates(l data.Level, floor uint64, now time.Time) []candidate {
	if l.Kind() == data.PrimaryKind {
		return nil
	}
	if l.Kind() == data.BoundedStalenessKind && l.Staleness() <= 0 {
		// No replica can be known to be no lag behind.
		return nil
	}
	var out []candidate
	for _, r := range t.replicas {
		rd := r.reading.Load()
		if ok, _ := t.rotating(rd, now); !ok {
			continue
		}
		switch l.Kind() {
		case data.BoundedStalenessKind:
			// The replica may have fallen further behind since.
			if age := max(now.Sub(rd.at), 0); rd.lag > l.Staleness() || age > l.Staleness()-rd.lag {
				continue
			}
		case data.ReadYourWritesKind:
			if rd.replay < floor {
				continue
			}
		}
		out = append(out, candidate{r, rd})
	}
	return out
}

// pick chooses one of cands, which isn't empty, by the strategy, from the
// readings they were chosen by.
func (t *topology) pick(cands []candidate) *replica {
	switch t.sel {
	case Random:
		return cands[rand.N(len(cands))].r
	case RoundRobin:
		return cands[(t.rr.Add(1)-1)%uint64(len(cands))].r
	case Nearest:
		var near []candidate
		for _, c := range cands {
			if c.r.zone != "" && c.r.zone == t.zone {
				near = append(near, c)
			}
		}
		if len(near) > 0 {
			cands = near
		}
	}
	return leastLagging(cands)
}

func leastLagging(cands []candidate) *replica {
	best := cands[0]
	for _, c := range cands[1:] {
		if c.rd.lag < best.rd.lag || (c.rd.lag == best.rd.lag && c.rd.lagBytes < best.rd.lagBytes) {
			best = c
		}
	}
	return best.r
}

// closest is the replica in rotation that has replayed the most, the one
// to wait on, or nil.
func (t *topology) closest(now time.Time) *replica {
	var best *replica
	var replay uint64
	for _, r := range t.replicas {
		rd := r.reading.Load()
		if ok, _ := t.rotating(rd, now); ok && (best == nil || rd.replay > replay) {
			best, replay = r, rd.replay
		}
	}
	return best
}

// markDown takes r out of rotation until the tracker next hears from it,
// after a read failed to begin on it.
func (t *topology) markDown(r *replica) {
	rd := &reading{at: t.now(), reason: reasonReplicaError, lag: unknownLag}
	if old := r.reading.Load(); old != nil {
		rd.replay = old.replay
	}
	r.reading.Store(rd)
}

// start begins tracking: a poller for the primary's position and one per
// replica. stop ends them and waits until they have.
func (t *topology) start(m *Manager, primary *source) {
	t.life.Lock()
	defer t.life.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.done.Add(1 + len(t.replicas))
	go func() {
		defer t.done.Done()
		t.pollPrimary(ctx, m, primary)
	}()
	for _, r := range t.replicas {
		go func() {
			defer t.done.Done()
			t.pollReplica(ctx, m, r)
		}()
	}
}

func (t *topology) stop() {
	t.life.Lock()
	defer t.life.Unlock()
	if t.cancel == nil {
		return
	}
	t.cancel()
	t.done.Wait()
	t.cancel = nil
	for _, r := range t.replicas {
		r.reading.Store(nil)
		r.rotating.Store(false)
	}
}

// poller keeps one connection of its own, outside the pools, so tracking
// neither waits for nor takes a connection the application needs, and its
// statements make no spans.
type poller struct {
	cfg  *pgconn.Config
	conn *pgconn.PgConn
	// wait bounds each query; connecting has the source's connect timeout.
	wait time.Duration
}

// defaultPollerConnectTimeout bounds a poller's connecting when its source
// sets no connect timeout.
const defaultPollerConnectTimeout = 10 * time.Second

// query runs sql, connecting first if need be, and returns its one row.
// An error the server sent, such as a function the role may not call,
// leaves the connection open for the next query; on any other the
// connection is closed, and the next query connects anew.
func (p *poller) query(ctx context.Context, sql string) ([][]byte, error) {
	if p.conn == nil || p.conn.IsClosed() {
		// pgconn bounds the connecting with the config's ConnectTimeout.
		c, err := pgconn.ConnectConfig(ctx, p.cfg)
		if err != nil {
			return nil, err
		}
		p.conn = c
	}
	qctx, cancel := context.WithTimeout(ctx, p.wait)
	defer cancel()
	res, err := p.conn.Exec(qctx, sql).ReadAll()
	if err == nil && (len(res) != 1 || len(res[0].Rows) != 1) {
		err = errors.New("the tracker's query returned no row")
	}
	if err != nil {
		if _, sent := errors.AsType[*pgconn.PgError](err); !sent {
			p.close()
		}
		return nil, err
	}
	return res[0].Rows[0], nil
}

func (p *poller) close() {
	if p.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = p.conn.Close(ctx)
		p.conn = nil
	}
}

func (t *topology) newPoller(src *source) *poller {
	p := &poller{cfg: src.config.ConnConfig.Config.Copy(), wait: t.staleAfter()}
	if p.cfg.ConnectTimeout <= 0 {
		p.cfg.ConnectTimeout = defaultPollerConnectTimeout
	}
	return p
}

// every runs fn now and then every t.poll until ctx ends.
func (t *topology) every(ctx context.Context, fn func(ctx context.Context)) {
	tick := time.NewTicker(t.poll)
	defer tick.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

const primarySQL = "SELECT pg_current_wal_lsn()::text"

// defaultLayoutRetry is how often the primary's poller tries again to
// read the WAL layout after it couldn't.
const defaultLayoutRetry = 5 * time.Second

func (t *topology) pollPrimary(ctx context.Context, m *Manager, src *source) {
	p := t.newPoller(src)
	defer p.close()
	// Start tried the layout already, and warned if it couldn't.
	next := t.layoutTried.Add(t.layoutRetry)
	t.every(ctx, func(qctx context.Context) {
		row, err := p.query(qctx, primarySQL)
		if err != nil {
			return // replicas keep their readings; their lag goes unknown in time
		}
		if lsn, err := parseLSN(string(row[0])); err == nil {
			t.primary.add(sample{at: t.now(), lsn: lsn})
		}
		if m.wal.Load() != nil || t.now().Before(next) {
			return
		}
		row, err = p.query(qctx, walLayoutSQL)
		var w walLayout
		if err == nil {
			w, err = parseWALLayout(row)
		}
		if err != nil {
			next = t.now().Add(t.layoutRetry)
			return
		}
		m.wal.Store(&w)
		m.logger().InfoContext(ctx, "the primary's WAL layout was read")
	})
}

// replicaSQL reads a replica's state. The time since the last replayed
// commit overstates the lag of a replica whose primary is idle, so the
// tracker only keeps it where its own history of the primary's position
// doesn't tell better (see history.lag).
const replicaSQL = "SELECT pg_is_in_recovery(), pg_last_wal_replay_lsn()::text, " +
	"extract(epoch FROM now() - pg_last_xact_replay_timestamp())::float8"

func (t *topology) pollReplica(ctx context.Context, m *Manager, r *replica) {
	p := t.newPoller(r.src)
	defer p.close()
	t.every(ctx, func(qctx context.Context) {
		rd := t.read(qctx, p)
		if ctx.Err() != nil {
			return // stopping: the query was cut short, not the replica
		}
		r.reading.Store(rd)
		in, why := t.rotating(rd, rd.at)
		if was := r.rotating.Swap(in); was != in {
			log := m.logger()
			switch {
			case in:
				log.Info("replica joined rotation", "replica", r.src.name, "lag", lagAttr(rd), "lag_bytes", lagBytesAttr(rd))
			default:
				log.Warn("replica left rotation", "replica", r.src.name, "reason", why, "lag", lagAttr(rd), "lag_bytes", lagBytesAttr(rd))
			}
		}
	})
}

// lagAttr and lagBytesAttr are rd's lag for a log, "unknown" when it is.
func lagAttr(rd *reading) any {
	if rd.lag == unknownLag {
		return "unknown"
	}
	return rd.lag
}

func lagBytesAttr(rd *reading) any {
	if rd.lagBytes == unknownBytes {
		return "unknown"
	}
	return rd.lagBytes
}

// read takes one reading through p.
func (t *topology) read(ctx context.Context, p *poller) *reading {
	row, err := p.query(ctx, replicaSQL)
	at := t.now()
	if err != nil {
		return &reading{at: at, reason: reasonUnreachable, lag: unknownLag}
	}
	if string(row[0]) != "t" || row[1] == nil {
		return &reading{at: at, reason: reasonNotInRecovery, lag: unknownLag}
	}
	replay, err := parseLSN(string(row[1]))
	if err != nil {
		return &reading{at: at, reason: reasonUnreachable, lag: unknownLag}
	}
	rd := &reading{at: at, healthy: true, replay: replay}
	xact := unknownLag
	if row[2] != nil {
		if secs, err := strconv.ParseFloat(string(row[2]), 64); err == nil {
			xact = time.Duration(min(max(secs, 0), float64(unknownLag/time.Second)) * float64(time.Second))
		}
	}
	rd.lagBytes, rd.lag = t.lagOf(replay, at, xact)
	return rd
}

// lagOf is the lag of a replica that has replayed replay, read at at, whose
// last replayed commit was committed xact ago (unknownLag if it has
// replayed none). Both the primary's history and xact bound the lag from
// above, so it is the smaller of the two. Neither alone will do: xact
// grows on a replica of an idle primary however caught up it is, which
// the history sees, and the history knows nothing once the tracker stops
// hearing from the primary, when only xact bounds it. xact is the replica's
// clock against the primary's commit timestamps, so skew between their
// clocks moves it by as much. When neither tells, the lag is unknown, never
// zero.
func (t *topology) lagOf(replay uint64, at time.Time, xact time.Duration) (int64, time.Duration) {
	bytes, hist := t.primary.lag(replay, at, t.staleAfter())
	return bytes, min(hist, xact)
}

// parseLSN reads an LSN as Postgres writes it, such as "16/B374D848".
func parseLSN(s string) (uint64, error) {
	hi, lo, ok := strings.Cut(s, "/")
	if !ok {
		return 0, fmt.Errorf("malformed LSN %q", s)
	}
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("malformed LSN %q", s)
	}
	return h<<32 | l, nil
}

func formatLSN(lsn uint64) string { return fmt.Sprintf("%X/%X", lsn>>32, uint32(lsn)) }

// routing is where a read that begins a transaction on the primary source
// runs.
type routing struct {
	rep      *replica      // nil for the primary
	conn     *pgxpool.Conn // the replica connection a wait polled on, if it waited
	fallback string        // why it reads the primary when it might not have
	waited   time.Duration
}

var errConsistencyTimeout = errs.New(errs.Unavailable, data.CodeConsistencyTimeout,
	"no replica caught up with the data this request depends on in time; retry", errs.RetryInfo{})

// route chooses where a read at level l runs: an in-memory comparison of
// the replicas' latest readings, and for ReadYourWrites with a wait, polling
// the closest replica until it passes the floor.
func (m *Manager) route(ctx context.Context, l data.Level) (routing, error) {
	t := m.topo
	var floor uint64
	if l.Kind() == data.ReadYourWritesKind {
		if floor = m.floor(ctx); floor == unknownFloor {
			// No replica can be known to have the request's write, and
			// failing would have it retried: the primary has it.
			return routing{fallback: fallbackNoReplica}, nil
		}
	}
	now := t.now()
	if cands := t.candidates(l, floor, now); len(cands) > 0 {
		return routing{rep: t.pick(cands)}, nil
	}
	if l.Kind() == data.PrimaryKind {
		return routing{}, nil
	}
	rt := routing{fallback: fallbackNoReplica}
	if l.Kind() == data.ReadYourWritesKind && l.MaxWait() > 0 {
		if target := t.closest(now); target != nil {
			conn, waited, err := m.waitFor(ctx, target, floor, l.MaxWait())
			rt.waited = waited
			switch {
			case conn != nil:
				return routing{rep: target, conn: conn, waited: waited}, nil
			case ctx.Err() != nil:
				return rt, classify(ctx, ctx.Err())
			case errors.Is(err, context.DeadlineExceeded):
				rt.fallback = fallbackWaitTimeout
			default:
				t.markDown(target)
				rt.fallback = fallbackError
			}
		}
	}
	if l.Kind() == data.ReadYourWritesKind && l.FailsOnTimeout() {
		return rt, errConsistencyTimeout
	}
	return rt, nil
}

// waitFor polls r's replay position on a connection of its pool until it
// passes floor, and returns that connection, or gives up after budget.
func (m *Manager) waitFor(ctx context.Context, r *replica, floor uint64, budget time.Duration) (*pgxpool.Conn, time.Duration, error) {
	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	pool := r.src.pool.Load()
	if pool == nil {
		return nil, 0, errUnavailable
	}
	conn, err := pool.Acquire(wctx)
	if err != nil {
		return nil, time.Since(start), err
	}
	pause := time.Millisecond
	for {
		res, err := conn.Conn().PgConn().Exec(wctx, "SELECT pg_last_wal_replay_lsn()::text").ReadAll()
		if err == nil && (len(res) != 1 || len(res[0].Rows) != 1 || res[0].Rows[0][0] == nil) {
			err = errors.New("the replica reports no replay position")
		}
		if err != nil {
			conn.Release()
			if wctx.Err() != nil {
				err = wctx.Err()
			}
			return nil, time.Since(start), err
		}
		if lsn, err := parseLSN(string(res[0].Rows[0][0])); err == nil && lsn >= floor {
			return conn, time.Since(start), nil
		}
		timer := time.NewTimer(pause)
		select {
		case <-timer.C:
		case <-wctx.Done():
			timer.Stop()
			conn.Release()
			return nil, time.Since(start), wctx.Err()
		}
		pause = min(2*pause, 20*time.Millisecond)
	}
}
