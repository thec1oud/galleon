package clock_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thec1oud/galleon/clock"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func recv(t *testing.T, ch <-chan time.Time) (time.Time, bool) {
	t.Helper()
	select {
	case v := <-ch:
		return v, true
	default:
		return time.Time{}, false
	}
}

func TestFakeTimerFiresAtDeadline(t *testing.T) {
	c := clock.NewFake(epoch)
	tm := c.NewTimer(10 * time.Second)
	c.Advance(9 * time.Second)
	if _, ok := recv(t, tm.C()); ok {
		t.Fatal("fired early")
	}
	c.Advance(5 * time.Second)
	v, ok := recv(t, tm.C())
	if !ok || !v.Equal(epoch.Add(10*time.Second)) {
		t.Fatalf("fired=%v at %v, want at +10s", ok, v)
	}
	if got := c.Now(); !got.Equal(epoch.Add(14 * time.Second)) {
		t.Fatalf("now = %v", got)
	}
}

func TestFakeStopAndReset(t *testing.T) {
	c := clock.NewFake(epoch)
	tm := c.NewTimer(time.Second)
	if !tm.Stop() {
		t.Fatal("Stop on pending timer should report true")
	}
	if tm.Stop() {
		t.Fatal("second Stop should report false")
	}
	c.Advance(2 * time.Second)
	if _, ok := recv(t, tm.C()); ok {
		t.Fatal("stopped timer fired")
	}
	tm.Reset(time.Second)
	c.Advance(time.Second)
	if _, ok := recv(t, tm.C()); !ok {
		t.Fatal("reset timer did not fire")
	}
}

func TestFakeTickerFiresEachPeriodInOrder(t *testing.T) {
	c := clock.NewFake(epoch)
	tk := c.NewTicker(time.Second)
	defer tk.Stop()
	c.AfterFunc(1500*time.Millisecond, func() {})
	for i := 1; i <= 3; i++ {
		c.Advance(time.Second)
		v, ok := recv(t, tk.C())
		if !ok || !v.Equal(epoch.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("tick %d: ok=%v at %v", i, ok, v)
		}
	}
	if c.Waiters() != 1 {
		t.Fatalf("want only the ticker pending, got %d", c.Waiters())
	}
}

func TestFakeAfterFuncAndBlockUntil(t *testing.T) {
	c := clock.NewFake(epoch)
	var fired atomic.Int32
	done := make(chan struct{})
	go func() {
		c.AfterFunc(time.Minute, func() { fired.Add(1); close(done) })
	}()
	c.BlockUntil(1)
	c.Advance(time.Minute)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("AfterFunc did not run")
	}
	if fired.Load() != 1 {
		t.Fatal("ran more than once")
	}
}

func TestSystemClockIsReal(t *testing.T) {
	c := clock.System()
	tm := c.NewTimer(time.Millisecond)
	select {
	case <-tm.C():
	case <-time.After(time.Second):
		t.Fatal("system timer did not fire")
	}
}

func TestFakeAfterFuncRunsInOrderAtEachDeadline(t *testing.T) {
	c := clock.NewFake(epoch)
	var got []time.Duration
	record := func() { got = append(got, c.Since(epoch)) }
	c.AfterFunc(2*time.Second, record)
	c.AfterFunc(time.Second, func() {
		record()
		c.AfterFunc(time.Second, record) // due at +2s, after the one registered first
	})
	c.AfterFunc(5*time.Second, record)
	c.Advance(3 * time.Second)
	want := []time.Duration{time.Second, 2 * time.Second, 2 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("ran at %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ran at %v, want %v", got, want)
		}
	}
	if c.Waiters() != 1 {
		t.Fatalf("only the +5s callback should be pending, got %d", c.Waiters())
	}
}

func TestFakeStopAndResetDropStaleValues(t *testing.T) {
	c := clock.NewFake(epoch)
	tm := c.NewTimer(time.Second)
	c.Advance(time.Second) // fires; the value sits in the channel
	tm.Stop()
	if _, ok := recv(t, tm.C()); ok {
		t.Fatal("Stop must discard an undelivered value")
	}
	c.Advance(time.Second)
	tm.Reset(10 * time.Second)
	c.Advance(time.Second)
	if _, ok := recv(t, tm.C()); ok {
		t.Fatal("Reset must discard an undelivered value, so a new timeout does not fire early")
	}

	tk := c.NewTicker(time.Second)
	c.Advance(time.Second)
	tk.Reset(time.Minute)
	if _, ok := recv(t, tk.C()); ok {
		t.Fatal("Ticker.Reset must discard an undelivered tick")
	}
}

func TestFakeStopAndResetReportAnUnreceivedFireAsActive(t *testing.T) {
	// Matches time.Timer since Go 1.23: a fired value nobody received is
	// discarded and the timer counts as stopped before firing, so the idiom
	// "if !t.Stop() { <-t.C() }" never blocks.
	c := clock.NewFake(epoch)
	tm := c.NewTimer(time.Second)
	c.Advance(time.Second)
	if !tm.Stop() {
		t.Fatal("Stop after an unreceived fire should report true")
	}
	tm.Reset(time.Second)
	c.Advance(time.Second)
	if !tm.Reset(time.Second) {
		t.Fatal("Reset after an unreceived fire should report true")
	}
	c.Advance(time.Second)
	<-tm.C()
	if tm.Stop() {
		t.Fatal("Stop after the value was received should report false")
	}
}

func TestFakeSetNeverOvershoots(t *testing.T) {
	c := clock.NewFake(epoch)
	c.Set(epoch.Add(time.Hour))
	c.Set(epoch) // going back does nothing
	if !c.Now().Equal(epoch.Add(time.Hour)) {
		t.Fatalf("now = %v", c.Now())
	}
}

func TestFakeConcurrentResetKeepsOneEntry(t *testing.T) {
	c := clock.NewFake(epoch)
	tm := c.NewTimer(time.Hour)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				tm.Reset(time.Hour)
			}
		}()
	}
	wg.Wait()
	if n := c.Waiters(); n != 1 {
		t.Fatalf("one timer has %d pending entries", n)
	}
	if !tm.Stop() || c.Waiters() != 0 {
		t.Fatal("Stop should remove the timer's only entry")
	}
}
