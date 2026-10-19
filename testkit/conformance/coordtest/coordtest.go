// Package coordtest is the conformance suite for coordination backends. Every
// backend runs it in its own tests:
//
//	func TestConformance(t *testing.T) {
//		coordtest.Run(t, coordtest.Factory{New: func(t *testing.T) coord.Backend { return mybackend.New(...) }})
//	}
//
// Capabilities the backend does not declare are skipped, and declaring one it
// cannot honour fails.
package coordtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thec1oud/galleon/coord"
)

// Factory creates the backends under test.
type Factory struct {
	// New returns a backend on a fresh, empty store.
	New func(t *testing.T) coord.Backend
	// Peer returns a second node's handle on the same store as b. Leave it nil
	// for process-scoped backends; the cross-node tests are then skipped.
	Peer func(t *testing.T, b coord.Backend) coord.Backend
}

// Run runs every conformance test that applies to the backend.
func Run(t *testing.T, f Factory) {
	t.Run("Capabilities", func(t *testing.T) {
		if err := checkCapabilities(f.New(t).Capabilities(), f.Peer != nil); err != nil {
			t.Error(err)
		}
	})
	t.Run("Membership", func(t *testing.T) {
		need(t, f, coord.MembershipCap)
		membership(t, f)
	})
	t.Run("Locks", func(t *testing.T) {
		need(t, f, coord.Locks)
		t.Run("MutualExclusion", func(t *testing.T) { mutualExclusion(t, f) })
		t.Run("FencingTokensIncrease", func(t *testing.T) { fencing(t, f) })
		t.Run("ExpiryClosesLost", func(t *testing.T) { expiry(t, f) })
		t.Run("RenewKeepsTheLease", func(t *testing.T) { renew(t, f) })
		t.Run("AcquireWaitsAndHonoursContext", func(t *testing.T) { acquireWaits(t, f) })
	})
	t.Run("LeaderElection", func(t *testing.T) {
		need(t, f, coord.LeaderElection)
		election(t, f)
	})
	t.Run("PubSub", func(t *testing.T) {
		need(t, f, coord.PubSubCap)
		pubsub(t, f)
	})
	t.Run("KV", func(t *testing.T) {
		need(t, f, coord.KVCap)
		t.Run("CompareAndSwap", func(t *testing.T) { kvCAS(t, f) })
		t.Run("CompareAndSwapRace", func(t *testing.T) { kvCASRace(t, f) })
		t.Run("TTLAndWatch", func(t *testing.T) { kvTTLWatch(t, f) })
	})
	t.Run("Counters", func(t *testing.T) {
		need(t, f, coord.CountersCap)
		counters(t, f)
	})
}

func need(t *testing.T, f Factory, c coord.Capability) {
	t.Helper()
	if !f.New(t).Capabilities().Has(c) {
		t.Skipf("backend does not declare %q", c)
	}
}

// selfID is the node ID a leader campaigning from b should be reported as.
// Without membership the suite can't know it, so it returns "" and callers
// only check that some leader is reported.
func selfID(b coord.Backend) coord.NodeID {
	if !b.Capabilities().Has(coord.MembershipCap) {
		return ""
	}
	return b.Membership().Self()
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

// nodes returns one handle, or two on the same store when the backend has peers.
func nodes(t *testing.T, f Factory) []coord.Backend {
	b := f.New(t)
	if f.Peer == nil {
		return []coord.Backend{b}
	}
	return []coord.Backend{b, f.Peer(t, b)}
}

// checkCapabilities enforces rule 6 (honest scope) and rule 7 (a reported
// failure-detection bound) of the coordination specification.
func checkCapabilities(caps coord.Capabilities, hasPeer bool) error {
	var errs []error
	if caps.Name == "" {
		errs = append(errs, errors.New("Capabilities().Name is empty"))
	}
	switch caps.Scope {
	case coord.ScopeProcess:
		if hasPeer {
			errs = append(errs, errors.New("a process-scoped backend cannot have peers on the same store"))
		}
	case coord.ScopeCluster:
		if !hasPeer {
			errs = append(errs, errors.New("a cluster-scoped backend must give the suite a Peer factory, so cross-node guarantees are tested"))
		}
		if caps.Has(coord.MembershipCap) && caps.FailureDetection <= 0 {
			errs = append(errs, errors.New("a cluster backend with membership must report its failure detection bound"))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown scope %v", caps.Scope))
	}
	return errors.Join(errs...)
}

func membership(t *testing.T, f Factory) {
	ns := nodes(t, f)
	want := map[coord.NodeID]bool{}
	for _, n := range ns {
		self := n.Membership().Self()
		if self == "" {
			t.Fatal("Self() is empty")
		}
		want[self] = true
	}
	if len(want) != len(ns) {
		t.Fatalf("nodes share a node ID: %v", want)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		members, err := ns[0].Membership().Members(ctx(t))
		if err != nil {
			t.Fatal(err)
		}
		seen := map[coord.NodeID]bool{}
		for _, m := range members {
			seen[m.ID] = true
		}
		missing := 0
		for id := range want {
			if !seen[id] {
				missing++
			}
		}
		if missing == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Members() = %v, want it to include %v", members, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mutualExclusion(t *testing.T, f Factory) {
	ns := nodes(t, f)
	var inside, maxInside atomic.Int32
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := ns[i%len(ns)].Locks().Acquire(ctx(t), "conformance/mutex", 5*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			n := inside.Add(1)
			for {
				m := maxInside.Load()
				if n <= m || maxInside.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
			if err := l.Release(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxInside.Load() != 1 {
		t.Fatalf("%d holders at once, want 1", maxInside.Load())
	}
}

func fencing(t *testing.T, f Factory) {
	ns := nodes(t, f)
	var last uint64
	for i := range 6 {
		l, err := ns[i%len(ns)].Locks().Acquire(ctx(t), "conformance/fence", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if l.Token() <= last {
			t.Fatalf("token %d is not greater than %d", l.Token(), last)
		}
		if l.Key() != "conformance/fence" {
			t.Fatalf("Key() = %q", l.Key())
		}
		last = l.Token()
		_ = l.Release(context.Background())
	}
}

func expiry(t *testing.T, f Factory) {
	ns := nodes(t, f)
	l1, ok, err := ns[0].Locks().TryAcquire(ctx(t), "conformance/expiry", 200*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	select {
	case <-l1.Lost():
	case <-time.After(5 * time.Second):
		t.Fatal("Lost() did not close after the TTL")
	}
	if err := l1.Renew(context.Background()); !errors.Is(err, coord.ErrLeaseLost) {
		t.Fatalf("Renew after expiry = %v, want ErrLeaseLost", err)
	}
	other := ns[len(ns)-1]
	l2, ok, err := other.Locks().TryAcquire(ctx(t), "conformance/expiry", 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("reacquire after expiry: ok=%v err=%v", ok, err)
	}
	defer l2.Release(context.Background())
	if l2.Token() <= l1.Token() {
		t.Fatalf("token after expiry %d is not greater than %d", l2.Token(), l1.Token())
	}
}

func renew(t *testing.T, f Factory) {
	ns := nodes(t, f)
	// A 1s TTL renewed every 100ms leaves 900ms of slack for a slow runner.
	l, ok, err := ns[0].Locks().TryAcquire(ctx(t), "conformance/renew", time.Second)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer l.Release(context.Background())
	for i := range 5 {
		time.Sleep(100 * time.Millisecond)
		if err := l.Renew(ctx(t)); err != nil {
			t.Fatalf("renew %d: %v", i, err)
		}
	}
	if _, ok, _ := ns[len(ns)-1].Locks().TryAcquire(ctx(t), "conformance/renew", time.Second); ok {
		t.Fatal("lease was taken while it was being renewed")
	}
}

func acquireWaits(t *testing.T, f Factory) {
	ns := nodes(t, f)
	l, ok, err := ns[0].Locks().TryAcquire(ctx(t), "conformance/wait", time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	waiter := ns[len(ns)-1]
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := waiter.Locks().Acquire(short, "conformance/wait", time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire on a held key with a short deadline = %v, want DeadlineExceeded", err)
	}
	type result struct {
		l   coord.Lease
		err error
	}
	got := make(chan result, 1) // buffered: the goroutine never blocks or touches t
	waitCtx := ctx(t)
	go func() {
		l2, err := waiter.Locks().Acquire(waitCtx, "conformance/wait", time.Second)
		got <- result{l2, err}
	}()
	time.Sleep(50 * time.Millisecond)
	_ = l.Release(context.Background())
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("waiting Acquire: %v", r.err)
		}
		_ = r.l.Release(context.Background())
	case <-time.After(5 * time.Second):
		t.Fatal("waiter was not woken by Release")
	}
}

func election(t *testing.T, f Factory) {
	ns := nodes(t, f)
	e := ns[0].Leader("conformance")
	if _, ok, err := e.Leader(ctx(t)); err != nil || ok {
		t.Fatalf("leader before any campaign: ok=%v err=%v", ok, err)
	}
	obsCtx, stop := context.WithCancel(ctx(t))
	defer stop()
	observed := ns[len(ns)-1].Leader("conformance").Observe(obsCtx)
	expect := func(want coord.NodeID) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case id, ok := <-observed:
				if !ok {
					t.Fatal("Observe closed early")
				}
				if id == want {
					return
				}
			case <-deadline:
				t.Fatalf("Observe never reported %q", want)
			}
		}
	}
	expect("")

	l, err := e.Campaign(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	self := selfID(ns[0])
	id, ok, _ := ns[len(ns)-1].Leader("conformance").Leader(ctx(t))
	if !ok || (self != "" && id != self) {
		t.Fatalf("leader = %q (ok=%v), want %q", id, ok, self)
	}
	expect(id)

	// A new observer gets the current leader first.
	late := ns[len(ns)-1].Leader("conformance").Observe(obsCtx)
	select {
	case first := <-late:
		if first != id {
			t.Fatalf("a new observer's first value = %q, want the current leader %q", first, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a new observer got no first value")
	}

	_ = l.Release(context.Background())
	if _, ok, _ := e.Leader(ctx(t)); ok {
		t.Fatal("leadership did not end on Release")
	}
	expect("")

	stop()
	closed := time.After(5 * time.Second)
	for _, ch := range []<-chan coord.NodeID{observed, late} {
	drain:
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					break drain
				}
			case <-closed:
				t.Fatal("Observe did not close when its context ended")
			}
		}
	}
}

func pubsub(t *testing.T, f Factory) {
	ns := nodes(t, f)
	subCtx, cancel := context.WithCancel(ctx(t))
	defer cancel()
	s, err := ns[len(ns)-1].PubSub().Subscribe(subCtx, "conformance/topic")
	if err != nil {
		t.Fatal(err)
	}
	pub := ns[0].PubSub()
	// At-most-once: a cluster backend may need a moment before a new
	// subscription receives, so publish until one message arrives.
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	_ = pub.Publish(ctx(t), "conformance/other", []byte("wrong topic"))
	_ = pub.Publish(ctx(t), "conformance/topic", []byte("hi"))
wait:
	for {
		select {
		case m := <-s.C():
			if m.Topic != "conformance/topic" || string(m.Data) != "hi" {
				t.Fatalf("got %q on %q", m.Data, m.Topic)
			}
			break wait
		case <-tick.C:
			_ = pub.Publish(ctx(t), "conformance/topic", []byte("hi"))
		case <-deadline:
			t.Fatal("no message delivered")
		}
	}
	cancel()
	closed := time.After(5 * time.Second)
	for {
		select {
		case _, open := <-s.C():
			if !open {
				return
			}
		case <-closed:
			t.Fatal("subscription not closed when its context ended")
		}
	}
}

func kvCAS(t *testing.T, f Factory) {
	ns := nodes(t, f)
	a, b := ns[0].KV(), ns[len(ns)-1].KV()
	c := ctx(t)
	r1, err := a.Put(c, "conformance/cas", []byte("1"), coord.IfAbsent())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(c, "conformance/cas", []byte("x"), coord.IfAbsent()); !errors.Is(err, coord.ErrRevisionMismatch) {
		t.Fatalf("IfAbsent on an existing key = %v, want ErrRevisionMismatch", err)
	}
	r2, err := b.Put(c, "conformance/cas", []byte("2"), coord.IfRevision(r1))
	if err != nil || r2 <= r1 {
		t.Fatalf("CAS put: revision %d, err %v", r2, err)
	}
	if _, err := a.Put(c, "conformance/cas", []byte("3"), coord.IfRevision(r1)); !errors.Is(err, coord.ErrRevisionMismatch) {
		t.Fatalf("stale CAS = %v, want ErrRevisionMismatch", err)
	}
	e, err := a.Get(c, "conformance/cas")
	if err != nil || string(e.Value) != "2" || e.Revision != r2 {
		t.Fatalf("Get = %+v, %v", e, err)
	}
	if err := a.Delete(c, "conformance/cas"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(c, "conformance/cas"); !errors.Is(err, coord.ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func kvCASRace(t *testing.T, f Factory) {
	ns := nodes(t, f)
	c := ctx(t)
	rev, err := ns[0].KV().Put(c, "conformance/race", []byte("0"))
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ns[i%len(ns)].KV().Put(c, "conformance/race", []byte("w"), coord.IfRevision(rev))
			switch {
			case err == nil:
				wins.Add(1)
			case !errors.Is(err, coord.ErrRevisionMismatch):
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d writers won the same compare-and-swap, want 1", wins.Load())
	}
}

func kvTTLWatch(t *testing.T, f Factory) {
	ns := nodes(t, f)
	c := ctx(t)
	events, err := ns[len(ns)-1].KV().Watch(c, "conformance/ttl/")
	if err != nil {
		t.Fatal(err)
	}
	kv := ns[0].KV()
	if _, err := kv.Put(c, "conformance/ttl/x", []byte("1"), coord.WithTTL(200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, _ = kv.Put(c, "conformance/elsewhere", []byte("ignored"))
	for _, want := range []coord.EventType{coord.EventPut, coord.EventDelete} {
		select {
		case ev := <-events:
			if ev.Type != want || ev.Entry.Key != "conformance/ttl/x" {
				t.Fatalf("event %+v, want type %v on conformance/ttl/x", ev, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("missing event %v", want)
		}
	}
	if _, err := kv.Get(c, "conformance/ttl/x"); !errors.Is(err, coord.ErrNotFound) {
		t.Fatalf("expired key: %v, want ErrNotFound", err)
	}
}

func counters(t *testing.T, f Factory) {
	ns := nodes(t, f)
	c := ctx(t)
	// A window boundary can fall inside a run; it can't fall inside two runs
	// in a row, so one retry with fresh keys tells a boundary from a bug.
	var err error
	for attempt := range 2 {
		if err = countersOnce(c, ns, fmt.Sprintf("conformance/ip/%d", attempt)); err == nil {
			return
		}
	}
	t.Fatal(err)
}

func countersOnce(c context.Context, ns []coord.Backend, key string) error {
	for i := 1; i <= 4; i++ {
		n, err := ns[i%len(ns)].Counters().Add(c, key, 1, time.Hour)
		if err != nil {
			return err
		}
		if n != int64(i) {
			return fmt.Errorf("count = %d, want %d", n, i)
		}
	}
	if n, err := ns[0].Counters().Add(c, key, 1, time.Minute); err != nil || n != 1 {
		return fmt.Errorf("a different window is a different counter: got %d (err %v), want 1", n, err)
	}
	return nil
}
