package coordpg_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thec1oud/galleon/coord"
	coordpg "github.com/thec1oud/galleon/coord-pg"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/testkit"
	"github.com/thec1oud/galleon/testkit/conformance/coordtest"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

// sharedPool is one pool on the package's database for every test.
func sharedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testkit.PackageDSN(t)
	poolOnce.Do(func() { pool, poolErr = pgxpool.New(context.Background(), dsn) })
	if poolErr != nil {
		t.Fatal(poolErr)
	}
	return pool
}

// store is a schema of its own, dropped when the test ends.
func store(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "coordtest_" + hex.EncodeToString(b)
	p := sharedPool(t)
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	return schema
}

// node starts a backend on schema, stopped when the test ends.
func node(t *testing.T, schema string, opts ...coordpg.Option) *coordpg.Backend {
	t.Helper()
	cfg := coordpg.Config{Schema: schema, Heartbeat: 100 * time.Millisecond, FailureDetection: 500 * time.Millisecond, LeaderTTL: time.Second}
	b := coordpg.New(append([]coordpg.Option{coordpg.UsePool(sharedPool(t)), coordpg.WithConfig(cfg)}, opts...)...)
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return b
}

func TestConformance(t *testing.T) {
	var schemas sync.Map // *coordpg.Backend → its schema
	coordtest.Run(t, coordtest.Factory{
		New: func(t *testing.T) coord.Backend {
			s := store(t)
			b := node(t, s)
			schemas.Store(b, s)
			return b
		},
		Peer: func(t *testing.T, b coord.Backend) coord.Backend {
			s, _ := schemas.Load(b)
			return node(t, s.(string))
		},
	})
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

// Tokens and the epoch live in the store, so a cluster restarted on it
// carries on from them.
func TestTokensAndTheEpochOutliveEveryNode(t *testing.T) {
	schema := store(t)
	a := node(t, schema)
	l1, ok, err := a.Locks().TryAcquire(ctx(t), "job", time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := node(t, schema)
	l2, ok, err := b.Locks().TryAcquire(ctx(t), "job", time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire after a restart: %v %v (Close should have released the first lease)", ok, err)
	}
	if l2.Token() <= l1.Token() || l2.Epoch() != l1.Epoch() || l1.Epoch() == "" {
		t.Fatalf("after a restart: token %d epoch %q, before: token %d epoch %q", l2.Token(), l2.Epoch(), l1.Token(), l1.Epoch())
	}
}

// A holder that stops renewing without releasing, as a frozen process
// does, keeps the key only until its expiry on the database's clock.
func TestAFrozenHolderLosesTheKeyAtItsExpiry(t *testing.T) {
	schema := store(t)
	b := node(t, schema)
	_, err := sharedPool(t).Exec(ctx(t), `INSERT INTO `+pgx.Identifier{schema, "leases"}.Sanitize()+
		` (key, token, holder, node, expires_at) VALUES ('frozen', 41, 'ghost', 'ghost-node', now() + interval '300 milliseconds')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := b.Locks().TryAcquire(ctx(t), "frozen", time.Second); ok {
		t.Fatal("took a key whose lease hasn't expired")
	}
	if id, ok, err := b.Leader("x").Leader(ctx(t)); ok || err != nil {
		t.Fatalf("leader of an election nobody campaigned in: %q %v %v", id, ok, err)
	}
	start := time.Now()
	l, err := b.Locks().Acquire(ctx(t), "frozen", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if l.Token() != 42 {
		t.Fatalf("token %d, want 42, one past the frozen holder's", l.Token())
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("waited %s for a lease with 300ms left", waited)
	}
}

func TestOneEpochWhenNodesInstallTogether(t *testing.T) {
	schema := store(t)
	var wg sync.WaitGroup
	epochs := make([]string, 6)
	for i := range epochs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg := coordpg.Config{Schema: schema}
			b := coordpg.New(coordpg.UsePool(sharedPool(t)), coordpg.WithConfig(cfg))
			if err := b.Start(ctx(t)); err != nil {
				t.Error(err)
				return
			}
			defer b.Close()
			l, ok, err := b.Locks().TryAcquire(ctx(t), fmt.Sprint("k", i), time.Second)
			if err != nil || !ok {
				t.Error(ok, err)
				return
			}
			epochs[i] = l.Epoch()
		}()
	}
	wg.Wait()
	for _, e := range epochs {
		if e != epochs[0] || e == "" {
			t.Fatalf("epochs %v, want one", epochs)
		}
	}
}

func TestANewerSchemaIsRefused(t *testing.T) {
	schema := store(t)
	node(t, schema)
	if _, err := sharedPool(t).Exec(ctx(t), `UPDATE `+pgx.Identifier{schema, "meta"}.Sanitize()+` SET version = 999`); err != nil {
		t.Fatal(err)
	}
	b := coordpg.New(coordpg.UsePool(sharedPool(t)), coordpg.WithConfig(coordpg.Config{Schema: schema}))
	err := b.Start(ctx(t))
	if !errors.Is(err, errs.New(errs.FailedPrecondition, coordpg.CodeSchemaNewer, "")) {
		t.Fatalf("Start on a newer schema = %v", err)
	}
}

func TestUseBeforeStartAndAfterStop(t *testing.T) {
	b := coordpg.New(coordpg.UsePool(sharedPool(t)), coordpg.WithConfig(coordpg.Config{Schema: store(t)}))
	if _, err := b.KV().Get(ctx(t), "k"); !errors.Is(err, errs.New(errs.FailedPrecondition, coordpg.CodeNotStarted, "")) {
		t.Fatalf("Get before Start = %v", err)
	}
	if err := b.Start(ctx(t)); err != nil {
		t.Fatal(err)
	}
	l, _, err := b.Locks().TryAcquire(ctx(t), "k", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-l.Lost():
	default:
		t.Fatal("Stop left a lease held")
	}
	if _, err := b.KV().Get(ctx(t), "k"); !errors.Is(err, coord.ErrClosed) {
		t.Fatalf("Get after Stop = %v", err)
	}
	if err := b.Start(ctx(t)); !errors.Is(err, coord.ErrClosed) {
		t.Fatalf("Start after Stop = %v", err)
	}
}

func TestStopLeavesMembership(t *testing.T) {
	schema := store(t)
	a, b := node(t, schema), node(t, schema)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	members, err := a.Membership().Members(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].ID != a.Membership().Self() {
		t.Fatalf("members after one stopped: %v", members)
	}
}

func TestAMessageMustFitANotify(t *testing.T) {
	b := node(t, store(t))
	err := b.PubSub().Publish(ctx(t), "big", make([]byte, 6000))
	if !errors.Is(err, errs.New(errs.InvalidArgument, coordpg.CodeMessageTooLarge, "")) {
		t.Fatalf("Publish of 6000 bytes = %v", err)
	}
	if err := b.PubSub().Publish(ctx(t), "big", make([]byte, 5000)); err != nil {
		t.Fatalf("Publish of 5000 bytes = %v", err)
	}
}

// terminateListeners ends every connection listening on the store, as a
// failover or a network fault would.
func terminateListeners(t *testing.T, schema string) {
	t.Helper()
	var n int
	err := sharedPool(t).QueryRow(ctx(t), `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE pid <> pg_backend_pid() AND query LIKE 'LISTEN %' AND query LIKE '%' || $1 || '%'`, schema).Scan(&n)
	if err != nil || n == 0 {
		t.Fatalf("terminated %d listeners: %v", n, err)
	}
}

// A watcher misses nothing while its node's listener is down, and pub/sub
// and lock waits work again once it reconnects.
func TestTheListenerReconnects(t *testing.T) {
	schema := store(t)
	a, b := node(t, schema), node(t, schema)
	events, err := b.KV().Watch(ctx(t), "w/")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := b.PubSub().Subscribe(ctx(t), "topic")
	if err != nil {
		t.Fatal(err)
	}
	terminateListeners(t, schema)
	for i := range 3 {
		if _, err := a.KV().Put(ctx(t), fmt.Sprint("w/", i), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 3 {
		select {
		case ev := <-events:
			if ev.Entry.Key != fmt.Sprint("w/", i) || ev.Type != coord.EventPut {
				t.Fatalf("event %d: %+v", i, ev)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("missing event %d", i)
		}
	}
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case m := <-sub.C():
			if string(m.Data) != "again" {
				t.Fatalf("message %q", m.Data)
			}
			return
		case <-tick.C:
			_ = a.PubSub().Publish(ctx(t), "topic", []byte("again"))
		case <-deadline:
			t.Fatal("pub/sub didn't recover")
		}
	}
}

// A watcher that falls behind the kept history is closed rather than
// skipping changes.
func TestAWatcherBehindTheHistoryIsClosed(t *testing.T) {
	schema := store(t)
	b := node(t, schema)
	events, err := b.KV().Watch(ctx(t), "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := b.KV().Put(ctx(t), fmt.Sprint("k", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	// The watcher holds k0 until it is read; the rest of the history goes.
	time.Sleep(200 * time.Millisecond)
	if _, err := sharedPool(t).Exec(ctx(t), `DELETE FROM `+pgx.Identifier{schema, "kv_history"}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.KV().Put(ctx(t), "k3", nil); err != nil {
		t.Fatal(err)
	}
	var got []string
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				if len(got) == 3 && got[2] == "k3" {
					t.Fatalf("got %v: the gap went unnoticed", got)
				}
				return
			}
			got = append(got, ev.Entry.Key)
		case <-timeout:
			t.Fatalf("the watcher wasn't closed; got %v", got)
		}
	}
}

func TestAnExpiredKeyIsAbsentBeforeItIsSwept(t *testing.T) {
	b := node(t, store(t))
	if _, err := b.KV().Put(ctx(t), "k", []byte("1"), coord.WithTTL(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := b.KV().Get(ctx(t), "k"); !errors.Is(err, coord.ErrNotFound) {
		t.Fatalf("Get of an expired key = %v", err)
	}
	if _, err := b.KV().Put(ctx(t), "k", []byte("2"), coord.IfAbsent()); err != nil {
		t.Fatalf("IfAbsent on an expired key = %v", err)
	}
}
