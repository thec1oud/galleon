package local

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thec1oud/galleon/coord"
)

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestLockMutualExclusion(t *testing.T) {
	b := New()
	var inside, maxInside int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := b.Locks().Acquire(ctx(t), "k", time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			n := atomic.AddInt32(&inside, 1)
			for {
				m := atomic.LoadInt32(&maxInside)
				if n <= m || atomic.CompareAndSwapInt32(&maxInside, m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&inside, -1)
			_ = l.Release(context.Background())
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("max holders at once = %d, want 1", maxInside)
	}
}

func TestFencingTokensIncrease(t *testing.T) {
	b := New()
	var last uint64
	for i := 0; i < 5; i++ {
		l, err := b.Locks().Acquire(ctx(t), "k", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if l.Token() <= last {
			t.Fatalf("token %d not greater than %d", l.Token(), last)
		}
		last = l.Token()
		_ = l.Release(context.Background())
	}
}

func TestLeaseExpiresAndClosesLost(t *testing.T) {
	b := New()
	l1, ok, err := b.Locks().TryAcquire(ctx(t), "k", 30*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("first acquire: %v %v", ok, err)
	}
	select {
	case <-l1.Lost():
	case <-time.After(time.Second):
		t.Fatal("lease did not expire")
	}
	if err := l1.Renew(context.Background()); !errors.Is(err, coord.ErrLeaseLost) {
		t.Fatalf("renew after expiry = %v", err)
	}
	l2, ok, _ := b.Locks().TryAcquire(ctx(t), "k", time.Second)
	if !ok || l2.Token() <= l1.Token() {
		t.Fatalf("reacquire ok=%v token=%d prev=%d", ok, l2.Token(), l1.Token())
	}
}

func TestRenewKeepsLease(t *testing.T) {
	b := New()
	l, _, _ := b.Locks().TryAcquire(ctx(t), "k", 50*time.Millisecond)
	for i := 0; i < 5; i++ {
		time.Sleep(25 * time.Millisecond)
		if err := l.Renew(context.Background()); err != nil {
			t.Fatalf("renew %d: %v", i, err)
		}
	}
	if _, ok, _ := b.Locks().TryAcquire(ctx(t), "k", time.Second); ok {
		t.Fatal("lock should still be held after renewals")
	}
}

func TestAcquireBlocksUntilReleaseAndHonoursContext(t *testing.T) {
	b := New()
	l, _, _ := b.Locks().TryAcquire(ctx(t), "k", time.Minute)

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := b.Locks().Acquire(short, "k", time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}

	got := make(chan coord.Lease)
	go func() {
		l2, _ := b.Locks().Acquire(ctx(t), "k", time.Second)
		got <- l2
	}()
	time.Sleep(10 * time.Millisecond)
	_ = l.Release(context.Background())
	select {
	case l2 := <-got:
		if l2 == nil {
			t.Fatal("acquire failed")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter not woken by release")
	}
}

func TestElection(t *testing.T) {
	b := New(WithNodeID("n1"))
	e := b.Leader("cron")
	if _, ok, _ := e.Leader(ctx(t)); ok {
		t.Fatal("no leader expected before campaign")
	}
	l, err := e.Campaign(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if id, ok, _ := e.Leader(ctx(t)); !ok || id != "n1" {
		t.Fatalf("leader = %q %v", id, ok)
	}
	_ = l.Release(context.Background())
	if _, ok, _ := e.Leader(ctx(t)); ok {
		t.Fatal("leadership should end on release")
	}
}

func TestPubSub(t *testing.T) {
	b := New()
	c, cancel := context.WithCancel(ctx(t))
	s, _ := b.PubSub().Subscribe(c, "t")
	_ = b.PubSub().Publish(ctx(t), "t", []byte("hi"))
	_ = b.PubSub().Publish(ctx(t), "other", []byte("no"))
	select {
	case m := <-s.C():
		if string(m.Data) != "hi" {
			t.Fatalf("got %q", m.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("no message")
	}
	cancel()
	select {
	case _, open := <-s.C():
		if open {
			t.Fatal("unexpected message after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription not closed on context cancel")
	}
}

func TestKVCompareAndSwap(t *testing.T) {
	k := New().KV()
	c := ctx(t)
	r1, err := k.Put(c, "a", []byte("1"), coord.IfAbsent())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Put(c, "a", []byte("x"), coord.IfAbsent()); !errors.Is(err, coord.ErrRevisionMismatch) {
		t.Fatalf("IfAbsent on existing key: %v", err)
	}
	r2, err := k.Put(c, "a", []byte("2"), coord.IfRevision(r1))
	if err != nil || r2 <= r1 {
		t.Fatalf("CAS put: rev %d err %v", r2, err)
	}
	if _, err := k.Put(c, "a", []byte("3"), coord.IfRevision(r1)); !errors.Is(err, coord.ErrRevisionMismatch) {
		t.Fatalf("stale CAS: %v", err)
	}
	e, _ := k.Get(c, "a")
	if string(e.Value) != "2" || e.Revision != r2 {
		t.Fatalf("get = %+v", e)
	}
}

func TestKVTTLAndWatch(t *testing.T) {
	k := New().KV()
	c := ctx(t)
	events, _ := k.Watch(c, "cfg/")
	_, _ = k.Put(c, "cfg/x", []byte("1"), coord.WithTTL(20*time.Millisecond))
	_, _ = k.Put(c, "other", []byte("ignored"))

	want := []coord.EventType{coord.EventPut, coord.EventDelete}
	for _, w := range want {
		select {
		case ev := <-events:
			if ev.Type != w || ev.Entry.Key != "cfg/x" {
				t.Fatalf("event = %+v, want type %v on cfg/x", ev, w)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing event %v", w)
		}
	}
	if _, err := k.Get(c, "cfg/x"); !errors.Is(err, coord.ErrNotFound) {
		t.Fatalf("expired key still present: %v", err)
	}
}

func TestCountersWindow(t *testing.T) {
	cs := New().Counters()
	c := ctx(t)
	for i := 1; i <= 3; i++ {
		n, _ := cs.Add(c, "ip:1", 1, time.Hour)
		if n != int64(i) {
			t.Fatalf("count = %d, want %d", n, i)
		}
	}
	// A different window size is a different counter.
	if n, _ := cs.Add(c, "ip:1", 1, time.Minute); n != 1 {
		t.Fatalf("separate window count = %d", n)
	}
}

func TestCapabilitiesAreHonest(t *testing.T) {
	caps := New().Capabilities()
	if caps.Scope != coord.ScopeProcess {
		t.Fatalf("local backend must report process scope, got %v", caps.Scope)
	}
}

func TestEveryBackendHasAnEpochOfItsOwn(t *testing.T) {
	ctx := context.Background()
	a, _ := New().Locks().Acquire(ctx, "k", time.Second)
	b, _ := New().Locks().Acquire(ctx, "k", time.Second)
	if a.Token() != b.Token() || a.Epoch() == "" || a.Epoch() == b.Epoch() {
		t.Fatalf("tokens %d and %d, epochs %q and %q: restarted counters must have new epochs", a.Token(), b.Token(), a.Epoch(), b.Epoch())
	}
}
