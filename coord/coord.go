// Package coord is Galleon's coordination interface: the single place every
// component gets cluster-wide guarantees (locks, leader election, pub/sub,
// shared KV, counters, membership). Galleon owns the interface; backends
// implement it. There is no default backend: an app must choose one explicitly.
package coord

import (
	"context"
	"errors"
	"time"
)

// Scope says how far a backend's guarantees reach.
type Scope uint8

const (
	// ScopeProcess guarantees hold inside one OS process only.
	ScopeProcess Scope = iota + 1
	// ScopeCluster guarantees hold across every process and node using the backend.
	ScopeCluster
)

func (s Scope) String() string {
	switch s {
	case ScopeProcess:
		return "process"
	case ScopeCluster:
		return "cluster"
	default:
		return "unknown"
	}
}

// Capability is something a component can declare it needs from coordination.
type Capability string

const (
	Locks          Capability = "locks"
	LeaderElection Capability = "leader_election"
	PubSubCap      Capability = "pubsub"
	KVCap          Capability = "kv"
	CountersCap    Capability = "counters"
	MembershipCap  Capability = "membership"
)

// Capabilities describes what a backend actually guarantees. It MUST be honest.
type Capabilities struct {
	Name     string
	Scope    Scope
	Provides []Capability
	// FailureDetection bounds how long a dead node can appear alive.
	FailureDetection time.Duration
}

// Has reports whether c is provided.
func (c Capabilities) Has(cap Capability) bool {
	for _, p := range c.Provides {
		if p == cap {
			return true
		}
	}
	return false
}

// NodeID identifies one running process.
type NodeID string

// Backend is the coordination seam.
type Backend interface {
	Capabilities() Capabilities
	Membership() Membership
	Locks() Locker
	Leader(name string) Election
	PubSub() PubSub
	KV() KV
	Counters() Counters
}

// Member is one node in the cluster.
type Member struct {
	ID    NodeID
	Roles []string
}

type Membership interface {
	Self() NodeID
	Members(ctx context.Context) ([]Member, error)
}

// Locker hands out leases. At most one holder per key at a time, as judged by
// fencing token.
type Locker interface {
	// Acquire blocks until the lease is held or ctx ends.
	Acquire(ctx context.Context, key string, ttl time.Duration) (Lease, error)
	// TryAcquire returns immediately; ok is false when someone else holds the key.
	TryAcquire(ctx context.Context, key string, ttl time.Duration) (lease Lease, ok bool, err error)
}

// Lease is a held lock. Its TTL never starts before the call that grants
// or renews it begins: TryAcquire's and Renew's start during the call, and
// Acquire's no earlier than the moment the lease is granted, however long
// it waited, so Acquire never returns a lease that has already expired. A
// holder measures its deadline from before the call that last granted or
// renewed the lease.
type Lease interface {
	Key() string
	// Token is a fencing token, strictly increasing per key within its
	// Epoch. Anything a lease protects should reject tokens lower than one
	// it has already seen from the same epoch.
	Token() uint64
	// Epoch identifies the counter Token comes from. Tokens compare only
	// within one epoch: a backend whose counter can restart, such as the
	// in-process one, gets a new epoch each time, and a protected resource
	// that sees a new epoch lets it take over. A cluster backend keeps its
	// counter, and so its epoch, durably, so its epoch never changes.
	Epoch() string
	Renew(ctx context.Context) error
	Release(ctx context.Context) error
	// Lost is closed when the lease ends for any reason (release, expiry, partition).
	Lost() <-chan struct{}
}

type Election interface {
	// Campaign blocks until this node is leader and returns the leadership lease.
	Campaign(ctx context.Context) (Lease, error)
	// Leader reports the current leader, if any.
	Leader(ctx context.Context) (NodeID, bool, error)
	// Observe sends the current leader, then every change of leader; "" means
	// there is none. The channel closes when ctx ends. Intermediate changes may
	// be skipped by a slow reader, but the latest leader is always delivered.
	Observe(ctx context.Context) <-chan NodeID
}

type Message struct {
	Topic string
	Data  []byte
}

// PubSub is at-most-once with no ordering across publishers. Use the outbox for
// anything that must not be lost.
type PubSub interface {
	Publish(ctx context.Context, topic string, data []byte) error
	Subscribe(ctx context.Context, topic string) (Subscription, error)
}

type Subscription interface {
	C() <-chan Message
	Close() error
}

type Entry struct {
	Key      string
	Value    []byte
	Revision uint64
}

type EventType uint8

const (
	EventPut EventType = iota + 1
	EventDelete
)

type Event struct {
	Type  EventType
	Entry Entry
}

type KV interface {
	Get(ctx context.Context, key string) (Entry, error) // ErrNotFound if absent
	Put(ctx context.Context, key string, value []byte, opts ...PutOption) (revision uint64, err error)
	Delete(ctx context.Context, key string, opts ...PutOption) error
	// Watch streams changes to keys with prefix until ctx ends.
	Watch(ctx context.Context, prefix string) (<-chan Event, error)
}

// PutOptions are applied by backends; use the With* helpers to build them.
type PutOptions struct {
	TTL time.Duration
	// IfRevision makes the write a compare-and-swap. Zero means "no condition";
	// use IfAbsent to require that the key does not exist.
	IfRevision uint64
	IfAbsent   bool
}

type PutOption func(*PutOptions)

func WithTTL(d time.Duration) PutOption { return func(o *PutOptions) { o.TTL = d } }
func IfRevision(rev uint64) PutOption   { return func(o *PutOptions) { o.IfRevision = rev } }
func IfAbsent() PutOption               { return func(o *PutOptions) { o.IfAbsent = true } }
func ApplyPutOptions(opts []PutOption) PutOptions {
	var o PutOptions
	for _, f := range opts {
		f(&o)
	}
	return o
}

// Counters are atomic counters over fixed time windows (rate limits, metering).
type Counters interface {
	// Add adds delta to key in the current window and returns the new total.
	Add(ctx context.Context, key string, delta int64, window time.Duration) (int64, error)
}

var (
	ErrNotFound         = errors.New("coord: not found")
	ErrRevisionMismatch = errors.New("coord: revision mismatch")
	ErrLeaseLost        = errors.New("coord: lease lost")
	ErrClosed           = errors.New("coord: backend closed")
)
