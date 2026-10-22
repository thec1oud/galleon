// Package coordpg is the coordination backend on PostgreSQL: every node of a
// cluster that reaches the same database shares its locks, leaders, pub/sub,
// key-value store, counters and membership (coord.ScopeCluster).
//
//	b := coordpg.New()
//	app, err := galleon.New(
//		galleon.Coordination(b), // installs b as a module, too
//		galleon.Deploy(galleon.MultiNode),
//		galleon.Modules(...),
//	)
//
// with the "coord_pg" config section:
//
//	{"coord_pg": {"dsn": "postgres://app@db/orders"}}
//
// or the ORDERS_COORD_PG_DSN environment variable. A backend given to
// galleon.Coordination is a module the app installs itself, so it reads that
// section, starts before every provider and stops after them all. Outside an
// app, call Start and Stop yourself, with WithConfig for the config.
//
// At start the backend creates its tables in its own schema
// (Config.Schema), once, however many nodes start together. The schema
// holds the store's epoch, made when the schema is created and kept from
// then on, so the fencing tokens of every node and every restart compare.
//
// # Leases
//
// A lease is a row of the leases table: its key, its holder, the fencing
// token of its last grant, and when it expires, on the database's clock.
// Granting a lease adds one to the token in the same statement, and the
// row is never deleted, so tokens only increase for a key while the schema
// lives. A frozen or partitioned holder can't keep a key past its TTL: the
// next acquirer takes it over once the database's clock passes the
// expiry.
//
// A holder measures its deadline from just before the call that granted or
// last renewed its lease; the database's expiry is later, since it starts
// when the statement runs. A lease that isn't renewed is released by its
// holder shortly before that deadline (a tenth of its TTL, at most 100ms),
// and Lost closes once the release returns, and never after the deadline.
// So Lost closes before another node can take the key over by expiry, and
// the key is free when Lost closes. That is rule 3's first guarantee, for
// a holder whose process keeps running: one that is paused past its
// deadline learns of it only when it resumes, and the lease's fencing token
// is what stops it then.
//
// # Pub/sub
//
// Pub/sub is LISTEN/NOTIFY: at-most-once, with no order across publishers.
// A message reaches the nodes subscribed to its topic when it is published,
// and is lost by a node whose listening connection is down. Each topic is a
// channel of its own, so a node receives only the topics it subscribes to.
// A message's topic and its data, base64-encoded, must fit NOTIFY's 8000
// bytes (CodeMessageTooLarge): about 5.9KB of data.
//
// # Key-value
//
// Every change to the key-value store takes the next revision of one
// counter, under a lock, so revisions follow the order changes commit in,
// and each is kept in a history table for Config.History. Watch reads the
// history, and NOTIFY only wakes it, so a watcher misses nothing while its
// connection is down: it catches up when the connection returns. A watcher
// that falls further behind than the history reaches is closed. Keys with
// a TTL are deleted, with a delete event, within about a second of their
// expiry; until then they read as absent.
package coordpg

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/errs"
)

// Section is the backend's config section.
const Section = "coord_pg"

// Config configures a Backend: the "coord_pg" section in an app, or
// WithConfig outside one.
type Config struct {
	// DSN is the database to coordinate through, when the backend isn't
	// given a pool with UsePool. Its pool_max_conns parameter sizes the
	// pool the backend opens.
	DSN string `json:"dsn" secret:"true"`
	// Schema holds the backend's tables, and is created at start if it
	// doesn't exist. Every node of a cluster must name the same one; two
	// apps on one database keep apart by naming different ones.
	Schema string `json:"schema"`
	// Heartbeat is how often a node renews its membership.
	Heartbeat time.Duration `json:"heartbeat"`
	// FailureDetection is how long after its last heartbeat a node that
	// stopped is still listed as a member. It is the bound Capabilities
	// reports, and must be at least twice Heartbeat.
	FailureDetection time.Duration `json:"failure_detection"`
	// LeaderTTL is the TTL of a leader's lease, renewed every third of it:
	// how long leadership outlives a leader that died.
	LeaderTTL time.Duration `json:"leader_ttl"`
	// History is how long key-value changes are kept for watchers to catch
	// up on.
	History time.Duration `json:"history"`
}

const (
	defaultSchema           = "galleon_coord"
	defaultHeartbeat        = time.Second
	defaultFailureDetection = 5 * time.Second
	defaultLeaderTTL        = 10 * time.Second
	defaultHistory          = 10 * time.Minute
)

// Default sets every key but DSN.
func (c *Config) Default() {
	*c = Config{Schema: defaultSchema, Heartbeat: defaultHeartbeat, FailureDetection: defaultFailureDetection,
		LeaderTTL: defaultLeaderTTL, History: defaultHistory}
}

// Validate checks the keys, but not that DSN is set: a backend given a pool
// doesn't need it.
func (c *Config) Validate() error {
	var problems []error
	if c.Schema == "" {
		problems = append(problems, errors.New("schema must not be empty"))
	}
	if c.Heartbeat <= 0 {
		problems = append(problems, errors.New("heartbeat must be positive"))
	}
	if c.FailureDetection < 2*c.Heartbeat {
		problems = append(problems, fmt.Errorf("failure_detection (%s) must be at least twice heartbeat (%s)", c.FailureDetection, c.Heartbeat))
	}
	if c.LeaderTTL < 30*time.Millisecond {
		problems = append(problems, errors.New("leader_ttl must be at least 30ms"))
	}
	if c.History < time.Second {
		problems = append(problems, errors.New("history must be at least 1s"))
	}
	return errors.Join(problems...)
}

const (
	// CodeNoDatabase is FailedPrecondition: the backend was started with
	// neither a pool (UsePool) nor a DSN.
	CodeNoDatabase = "coord_pg.no_database"
	// CodeNotStarted is FailedPrecondition: the backend was used before
	// Start.
	CodeNotStarted = "coord_pg.not_started"
	// CodeSchemaNewer is FailedPrecondition: the schema was created by a
	// newer version of coord-pg than this node's.
	CodeSchemaNewer = "coord_pg.schema_newer"
	// CodeInvalidArgument is InvalidArgument: a lease's TTL under 1ms, a
	// key's TTL below zero, a counter's window under 1µs, or a topic that
	// isn't valid UTF-8 without NUL bytes.
	CodeInvalidArgument = "coord_pg.invalid_argument"
	// CodeMessageTooLarge is InvalidArgument: a published message's topic
	// and base64-encoded data don't fit a NOTIFY payload.
	CodeMessageTooLarge = "coord_pg.message_too_large"
	// CodeUnavailable is Unavailable, retryable: the coordination database
	// couldn't be reached, or failed a statement.
	CodeUnavailable = "coord_pg.unavailable"
)

// Backend implements coord.Backend on PostgreSQL. Make it with New.
type Backend struct {
	node  coord.NodeID
	roles []string

	cfg      Config
	cfgGiven bool // set by WithConfig
	pool     *pgxpool.Pool
	ownPool  bool // the backend opened pool, and closes it
	log      *slog.Logger

	// Set by Start; read-only after.
	epoch  string
	q      queries
	events string // the NOTIFY channel of lock and key-value changes
	lis    *listener

	mu      sync.Mutex
	state   state
	leases  map[*lease]struct{}
	waits   map[string]chan struct{} // closed when a lock key, by its hash, changes
	subs    map[string]map[*subscription]struct{}
	watches map[*watcher]struct{}
	stop    context.CancelFunc // ends the background loops
	loops   sync.WaitGroup
	sweep   chan struct{} // asks the sweeper to run now
}

type state uint8

const (
	stateNew state = iota
	stateStarted
	stateClosed
)

// Option configures a Backend.
type Option func(*Backend)

// UsePool coordinates through pool, which the caller keeps and closes after
// the backend stops. The backend also opens one connection of its own, with
// pool's settings, to listen on.
func UsePool(pool *pgxpool.Pool) Option { return func(b *Backend) { b.pool = pool } }

// WithNodeID overrides the generated node ID.
func WithNodeID(id coord.NodeID) Option { return func(b *Backend) { b.node = id } }

// WithRoles sets the roles membership lists for this node.
func WithRoles(roles ...string) Option { return func(b *Backend) { b.roles = roles } }

// WithConfig configures a backend used outside an app. In an app, the
// "coord_pg" section configures it, and WithConfig is refused at boot.
// Keys left zero take their defaults.
func WithConfig(c Config) Option {
	return func(b *Backend) { b.cfg, b.cfgGiven = c, true }
}

// WithLogger sets the logger, which is silent by default. The backend
// starts before the app's providers, so it can't take the app's own.
func WithLogger(l *slog.Logger) Option { return func(b *Backend) { b.log = l } }

// New returns a backend, which connects when it starts.
func New(opts ...Option) *Backend {
	host, _ := os.Hostname()
	b := &Backend{
		node:    coord.NodeID(fmt.Sprintf("%s/%d/%s", host, os.Getpid(), randomHex(4))),
		log:     slog.New(slog.DiscardHandler),
		leases:  map[*lease]struct{}{},
		waits:   map[string]chan struct{}{},
		subs:    map[string]map[*subscription]struct{}{},
		watches: map[*watcher]struct{}{},
		sweep:   make(chan struct{}, 1),
	}
	b.cfg.Default()
	for _, o := range opts {
		o(b)
	}
	if b.cfgGiven {
		b.cfg.fillDefaults()
	}
	return b
}

func (c *Config) fillDefaults() {
	var d Config
	d.Default()
	if c.Schema == "" {
		c.Schema = d.Schema
	}
	if c.Heartbeat == 0 {
		c.Heartbeat = d.Heartbeat
	}
	if c.FailureDetection == 0 {
		c.FailureDetection = max(d.FailureDetection, 2*c.Heartbeat)
	}
	if c.LeaderTTL == 0 {
		c.LeaderTTL = d.LeaderTTL
	}
	if c.History == 0 {
		c.History = d.History
	}
}

var (
	_ coord.Backend  = (*Backend)(nil)
	_ galleon.Module = (*Backend)(nil)
)

// Name is the backend's module name.
func (b *Backend) Name() string { return Section }

// Register reads the "coord_pg" section. The app calls it, since
// galleon.Coordination installs the backend as a module.
func (b *Backend) Register(r *galleon.Registry) error {
	if b.cfgGiven {
		return errors.New("coordpg.WithConfig configures a backend used outside an app; in an app, use the coord_pg config section")
	}
	b.cfg = *galleon.Config[Config](r, Section)
	if b.pool == nil && b.cfg.DSN == "" {
		return errors.New("coord_pg needs a database: set coord_pg.dsn, or give the backend a pool with coordpg.UsePool")
	}
	return nil
}

// Capabilities reports a cluster scope and every capability.
func (b *Backend) Capabilities() coord.Capabilities {
	return coord.Capabilities{
		Name:  "pg",
		Scope: coord.ScopeCluster,
		Provides: []coord.Capability{
			coord.Locks, coord.LeaderElection, coord.PubSubCap,
			coord.KVCap, coord.CountersCap, coord.MembershipCap,
		},
		FailureDetection: b.cfg.FailureDetection,
	}
}

func (b *Backend) Membership() coord.Membership      { return membership{b} }
func (b *Backend) Locks() coord.Locker               { return locker{b} }
func (b *Backend) Leader(name string) coord.Election { return election{b: b, name: name} }
func (b *Backend) PubSub() coord.PubSub              { return pubsub{b} }
func (b *Backend) KV() coord.KV                      { return kv{b} }
func (b *Backend) Counters() coord.Counters          { return counters{b} }

// Start connects, creates the schema if it is missing, joins membership and
// starts listening. It fails, and leaves nothing running, if any of that
// fails.
func (b *Backend) Start(ctx context.Context) (err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateStarted:
		return errors.New("coordpg: Start called twice")
	case stateClosed:
		return coord.ErrClosed
	}
	if err := b.cfg.Validate(); err != nil {
		return fmt.Errorf("coordpg: %w", err)
	}
	if b.pool == nil {
		if b.cfg.DSN == "" {
			return errs.New(errs.FailedPrecondition, CodeNoDatabase, "the coordination backend has no database: give it a pool or a DSN")
		}
		pool, err := pgxpool.New(ctx, b.cfg.DSN)
		if err != nil {
			return errs.Wrap(err, errs.FailedPrecondition, CodeNoDatabase, "the coordination backend's DSN can't be used")
		}
		b.pool, b.ownPool = pool, true
	}
	defer func() {
		if err != nil && b.ownPool {
			b.pool.Close()
			b.pool, b.ownPool = nil, false
		}
	}()
	b.q = newQueries(b.cfg.Schema)
	b.events = channelName(b.cfg.Schema, "e", "")
	if b.epoch, err = b.install(ctx); err != nil {
		return err
	}
	if err := b.heartbeat(ctx); err != nil {
		return b.fail(ctx, err)
	}
	loops, stop := context.WithCancel(context.WithoutCancel(ctx))
	b.lis = newListener(b, b.pool.Config().ConnConfig)
	if err := b.lis.start(ctx, loops); err != nil {
		stop()
		b.lis.wait()
		_ = b.leave(context.WithoutCancel(ctx))
		return err
	}
	b.stop = stop
	b.loops.Add(2)
	go b.heartbeats(loops)
	go b.sweeper(loops)
	b.state = stateStarted
	b.log.InfoContext(ctx, "coordination started", "node", b.node, "schema", b.cfg.Schema, "epoch", b.epoch)
	return nil
}

// Stop releases every lease the node holds, leaves membership, ends every
// subscription and watch, and closes the connections the backend opened.
// Every call after it returns coord.ErrClosed.
func (b *Backend) Stop(ctx context.Context) error {
	b.mu.Lock()
	if b.state != stateStarted {
		b.state = stateClosed
		b.mu.Unlock()
		return nil
	}
	b.state = stateClosed
	held := make([]*lease, 0, len(b.leases))
	for l := range b.leases {
		held = append(held, l)
	}
	subs := []*subscription{}
	for _, m := range b.subs {
		for s := range m {
			subs = append(subs, s)
		}
	}
	b.mu.Unlock()

	var problems []error
	for _, l := range held {
		if err := l.release(ctx); err != nil {
			problems = append(problems, err)
		}
	}
	if err := b.leave(ctx); err != nil {
		problems = append(problems, err)
	}
	for _, s := range subs {
		_ = s.Close()
	}
	b.stop()
	b.lis.wait()
	b.loops.Wait()
	b.mu.Lock()
	for w := range b.watches {
		w.end()
	}
	b.mu.Unlock()
	if b.ownPool {
		b.pool.Close()
	}
	return errors.Join(problems...)
}

// Close is Stop without a deadline.
func (b *Backend) Close() error { return b.Stop(context.Background()) }

// ready returns an error unless the backend has started and not stopped.
func (b *Backend) ready() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateNew:
		return errs.New(errs.FailedPrecondition, CodeNotStarted, "the coordination backend hasn't started")
	case stateClosed:
		return coord.ErrClosed
	}
	return nil
}

func (b *Backend) closed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state == stateClosed
}

// fail classifies a database error: the caller's context error if ctx has
// ended, coord.ErrClosed if the backend has stopped, and otherwise
// CodeUnavailable.
func (b *Backend) fail(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if b.closed() {
		return coord.ErrClosed
	}
	var e *errs.Error
	if errors.As(err, &e) {
		return err
	}
	return errs.Wrap(err, errs.Unavailable, CodeUnavailable, "the coordination database failed", errs.RetryInfo{})
}

// waitFor returns a channel closed at the next change of the lock key
// whose hash is h, or when the listener reconnects. Take it before reading
// the state it waits on, so a change in between isn't missed.
func (b *Backend) waitFor(h string) <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := b.waits[h]
	if ch == nil {
		ch = make(chan struct{})
		b.waits[h] = ch
	}
	return ch
}

// changed wakes the waiters of the lock key whose hash is h.
func (b *Backend) changed(h string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch := b.waits[h]; ch != nil {
		close(ch)
		delete(b.waits, h)
	}
}

// wakeAll wakes every waiter and watcher, after notifications may have been
// lost.
func (b *Backend) wakeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for h, ch := range b.waits {
		close(ch)
		delete(b.waits, h)
	}
	for w := range b.watches {
		w.wake()
	}
}

// keyHash names a lock key in notifications, whatever its length.
func keyHash(key string) string {
	s := sha256.Sum256([]byte(key))
	return hex.EncodeToString(s[:8])
}

// channelName is a NOTIFY channel of the schema: kind, then the hash of
// name if there is one. A channel name is at most 63 bytes, so a long
// schema is named by its hash.
func channelName(schema, kind, name string) string {
	base := schema
	if len(base) > 40 {
		s := sha256.Sum256([]byte(schema))
		base = "galleon_coord_" + hex.EncodeToString(s[:8])
	}
	if name == "" {
		return base + ":" + kind
	}
	return base + ":" + kind + ":" + keyHash(name)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func micros(d time.Duration) int64 { return d.Microseconds() }

func ident(parts ...string) string { return pgx.Identifier(parts).Sanitize() }
