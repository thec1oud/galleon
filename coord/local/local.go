// Package local is the in-process coordination backend. Its guarantees are exact
// but hold inside one OS process only (coord.ScopeProcess). Use it for
// single-node deployments, development and tests; it must be chosen explicitly.
package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/thec1oud/galleon/coord"
)

// Backend implements coord.Backend in memory. Its fencing tokens start
// again at 1 in every Backend, so each has an epoch of its own, random, that
// its leases report: a resource that recorded a token from an earlier
// process tells the two counters apart.
type Backend struct {
	node  coord.NodeID
	epoch string

	mu      sync.Mutex
	locks   map[string]*lease
	tokens  map[string]uint64
	changed chan struct{} // closed and replaced whenever a lock is taken or freed

	subs map[string]map[*subscription]struct{}

	kv       map[string]*kvEntry
	rev      uint64
	watchers map[*watcher]struct{}

	counters map[string]*counter
}

// Option configures a Backend.
type Option func(*Backend)

// WithNodeID overrides the generated node ID.
func WithNodeID(id coord.NodeID) Option { return func(b *Backend) { b.node = id } }

// New returns an empty in-process backend.
func New(opts ...Option) *Backend {
	host, _ := os.Hostname()
	var e [8]byte
	_, _ = rand.Read(e[:])
	b := &Backend{
		node:     coord.NodeID(fmt.Sprintf("%s-%d", host, os.Getpid())),
		epoch:    "local-" + hex.EncodeToString(e[:]),
		locks:    map[string]*lease{},
		tokens:   map[string]uint64{},
		changed:  make(chan struct{}),
		subs:     map[string]map[*subscription]struct{}{},
		kv:       map[string]*kvEntry{},
		watchers: map[*watcher]struct{}{},
		counters: map[string]*counter{},
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

var _ coord.Backend = (*Backend)(nil)

func (b *Backend) Capabilities() coord.Capabilities {
	return coord.Capabilities{
		Name:  "local",
		Scope: coord.ScopeProcess,
		Provides: []coord.Capability{
			coord.Locks, coord.LeaderElection, coord.PubSubCap,
			coord.KVCap, coord.CountersCap, coord.MembershipCap,
		},
	}
}

func (b *Backend) Membership() coord.Membership      { return membership{b} }
func (b *Backend) Locks() coord.Locker               { return locker{b} }
func (b *Backend) Leader(name string) coord.Election { return election{b: b, name: name} }
func (b *Backend) PubSub() coord.PubSub              { return pubsub{b} }
func (b *Backend) KV() coord.KV                      { return kv{b} }
func (b *Backend) Counters() coord.Counters          { return counters{b} }

// ---- membership ----

type membership struct{ b *Backend }

func (m membership) Self() coord.NodeID { return m.b.node }
func (m membership) Members(context.Context) ([]coord.Member, error) {
	return []coord.Member{{ID: m.b.node}}, nil
}

// ---- locks ----

type locker struct{ b *Backend }

type lease struct {
	b       *Backend
	key     string
	token   uint64
	ttl     time.Duration
	expires time.Time
	timer   *time.Timer
	lost    chan struct{}
	ended   bool
}

func (l *lease) Key() string           { return l.key }
func (l *lease) Token() uint64         { return l.token }
func (l *lease) Epoch() string         { return l.b.epoch }
func (l *lease) Lost() <-chan struct{} { return l.lost }

func (l *lease) Renew(context.Context) error {
	l.b.mu.Lock()
	defer l.b.mu.Unlock()
	if l.ended {
		return coord.ErrLeaseLost
	}
	l.expires = time.Now().Add(l.ttl)
	l.timer.Reset(l.ttl)
	return nil
}

func (l *lease) Release(context.Context) error {
	l.b.mu.Lock()
	defer l.b.mu.Unlock()
	l.b.endLocked(l)
	return nil
}

// expire runs from the lease timer. A Renew may have moved the deadline after
// the timer fired, so re-check before ending the lease.
func (l *lease) expire() {
	l.b.mu.Lock()
	defer l.b.mu.Unlock()
	if l.ended {
		return
	}
	if remaining := time.Until(l.expires); remaining > 0 {
		l.timer.Reset(remaining)
		return
	}
	l.b.endLocked(l)
}

func (b *Backend) endLocked(l *lease) {
	if l.ended {
		return
	}
	l.ended = true
	l.timer.Stop()
	close(l.lost)
	if b.locks[l.key] == l {
		delete(b.locks, l.key)
	}
	b.locksChangedLocked()
}

func (b *Backend) locksChangedLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

func (b *Backend) tryAcquireLocked(key string, ttl time.Duration) (*lease, chan struct{}) {
	if cur := b.locks[key]; cur != nil && !cur.ended {
		return nil, b.changed
	}
	b.tokens[key]++
	l := &lease{b: b, key: key, token: b.tokens[key], ttl: ttl, expires: time.Now().Add(ttl), lost: make(chan struct{})}
	l.timer = time.AfterFunc(ttl, l.expire)
	b.locks[key] = l
	b.locksChangedLocked()
	return l, nil
}

func (lk locker) TryAcquire(ctx context.Context, key string, ttl time.Duration) (coord.Lease, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if ttl <= 0 {
		return nil, false, fmt.Errorf("coord/local: ttl must be positive")
	}
	lk.b.mu.Lock()
	defer lk.b.mu.Unlock()
	l, _ := lk.b.tryAcquireLocked(key, ttl)
	if l == nil {
		return nil, false, nil
	}
	return l, true, nil
}

func (lk locker) Acquire(ctx context.Context, key string, ttl time.Duration) (coord.Lease, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("coord/local: ttl must be positive")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lk.b.mu.Lock()
		l, wait := lk.b.tryAcquireLocked(key, ttl)
		lk.b.mu.Unlock()
		if l != nil {
			return l, nil
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// ---- leader election ----

type election struct {
	b    *Backend
	name string
}

const electionTTL = 10 * time.Second

func (e election) key() string { return "galleon/election/" + e.name }

func (e election) Campaign(ctx context.Context) (coord.Lease, error) {
	l, err := locker{e.b}.Acquire(ctx, e.key(), electionTTL)
	if err != nil {
		return nil, err
	}
	go func() {
		t := time.NewTicker(electionTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-l.Lost():
				return
			case <-t.C:
				if l.Renew(context.Background()) != nil {
					return
				}
			}
		}
	}()
	return l, nil
}

func (e election) Leader(context.Context) (coord.NodeID, bool, error) {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	id, ok := e.leaderLocked()
	return id, ok, nil
}

func (e election) leaderLocked() (coord.NodeID, bool) {
	if l := e.b.locks[e.key()]; l != nil && !l.ended {
		return e.b.node, true
	}
	return "", false
}

func (e election) Observe(ctx context.Context) <-chan coord.NodeID {
	out := make(chan coord.NodeID)
	go func() {
		defer close(out)
		first := true
		var last coord.NodeID
		for {
			e.b.mu.Lock()
			id, _ := e.leaderLocked()
			wait := e.b.changed
			e.b.mu.Unlock()
			if first || id != last {
				select {
				case out <- id:
				case <-ctx.Done():
					return
				}
				first, last = false, id
				continue // re-read: the leader may have changed while we were sending
			}
			select {
			case <-wait:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// ---- pub/sub ----

type pubsub struct{ b *Backend }

type subscription struct {
	b      *Backend
	topic  string
	ch     chan coord.Message
	closed bool
}

const subscriptionBuffer = 256

func (s *subscription) C() <-chan coord.Message { return s.ch }

func (s *subscription) Close() error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	delete(s.b.subs[s.topic], s)
	close(s.ch)
	return nil
}

func (p pubsub) Publish(ctx context.Context, topic string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	msg := coord.Message{Topic: topic, Data: append([]byte(nil), data...)}
	p.b.mu.Lock()
	defer p.b.mu.Unlock()
	for s := range p.b.subs[topic] {
		select {
		case s.ch <- msg:
		default: // at-most-once: a slow subscriber drops messages
		}
	}
	return nil
}

func (p pubsub) Subscribe(ctx context.Context, topic string) (coord.Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := &subscription{b: p.b, topic: topic, ch: make(chan coord.Message, subscriptionBuffer)}
	p.b.mu.Lock()
	if p.b.subs[topic] == nil {
		p.b.subs[topic] = map[*subscription]struct{}{}
	}
	p.b.subs[topic][s] = struct{}{}
	p.b.mu.Unlock()
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	return s, nil
}

// ---- key/value ----

type kv struct{ b *Backend }

type kvEntry struct {
	value []byte
	rev   uint64
	timer *time.Timer
}

func (k kv) Get(ctx context.Context, key string) (coord.Entry, error) {
	if err := ctx.Err(); err != nil {
		return coord.Entry{}, err
	}
	k.b.mu.Lock()
	defer k.b.mu.Unlock()
	e := k.b.kv[key]
	if e == nil {
		return coord.Entry{}, coord.ErrNotFound
	}
	return coord.Entry{Key: key, Value: append([]byte(nil), e.value...), Revision: e.rev}, nil
}

func (k kv) Put(ctx context.Context, key string, value []byte, opts ...coord.PutOption) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	o := coord.ApplyPutOptions(opts)
	b := k.b
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.kv[key]
	if o.IfAbsent && cur != nil {
		return 0, coord.ErrRevisionMismatch
	}
	if o.IfRevision != 0 && (cur == nil || cur.rev != o.IfRevision) {
		return 0, coord.ErrRevisionMismatch
	}
	if cur != nil && cur.timer != nil {
		cur.timer.Stop()
	}
	b.rev++
	e := &kvEntry{value: append([]byte(nil), value...), rev: b.rev}
	if o.TTL > 0 {
		rev := e.rev
		e.timer = time.AfterFunc(o.TTL, func() { b.expireKey(key, rev) })
	}
	b.kv[key] = e
	b.notifyLocked(coord.Event{Type: coord.EventPut, Entry: coord.Entry{Key: key, Value: append([]byte(nil), value...), Revision: e.rev}})
	return e.rev, nil
}

func (k kv) Delete(ctx context.Context, key string, opts ...coord.PutOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o := coord.ApplyPutOptions(opts)
	b := k.b
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.kv[key]
	if o.IfRevision != 0 && (cur == nil || cur.rev != o.IfRevision) {
		return coord.ErrRevisionMismatch
	}
	if cur == nil {
		return nil
	}
	b.deleteLocked(key, cur)
	return nil
}

func (b *Backend) expireKey(key string, rev uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur := b.kv[key]; cur != nil && cur.rev == rev {
		b.deleteLocked(key, cur)
	}
}

func (b *Backend) deleteLocked(key string, cur *kvEntry) {
	if cur.timer != nil {
		cur.timer.Stop()
	}
	delete(b.kv, key)
	b.rev++
	b.notifyLocked(coord.Event{Type: coord.EventDelete, Entry: coord.Entry{Key: key, Revision: b.rev}})
}

// watcher buffers events without bound so a slow reader never loses one.
type watcher struct {
	prefix string
	mu     sync.Mutex
	queue  []coord.Event
	signal chan struct{}
	done   bool
}

func (b *Backend) notifyLocked(ev coord.Event) {
	for w := range b.watchers {
		if !strings.HasPrefix(ev.Entry.Key, w.prefix) {
			continue
		}
		w.mu.Lock()
		w.queue = append(w.queue, ev)
		w.mu.Unlock()
		select {
		case w.signal <- struct{}{}:
		default:
		}
	}
}

func (k kv) Watch(ctx context.Context, prefix string) (<-chan coord.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w := &watcher{prefix: prefix, signal: make(chan struct{}, 1)}
	k.b.mu.Lock()
	k.b.watchers[w] = struct{}{}
	k.b.mu.Unlock()

	out := make(chan coord.Event)
	go func() {
		defer close(out)
		defer func() {
			k.b.mu.Lock()
			delete(k.b.watchers, w)
			k.b.mu.Unlock()
		}()
		for {
			w.mu.Lock()
			batch := w.queue
			w.queue = nil
			w.mu.Unlock()
			for _, ev := range batch {
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-w.signal:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// ---- counters ----

type counters struct{ b *Backend }

type counter struct {
	window time.Time
	value  int64
}

func (c counters) Add(ctx context.Context, key string, delta int64, window time.Duration) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if window <= 0 {
		return 0, fmt.Errorf("coord/local: window must be positive")
	}
	start := time.Now().Truncate(window)
	id := fmt.Sprintf("%s|%d", key, window)
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	cur := c.b.counters[id]
	if cur == nil || !cur.window.Equal(start) {
		cur = &counter{window: start}
		c.b.counters[id] = cur
	}
	cur.value += delta
	return cur.value, nil
}
