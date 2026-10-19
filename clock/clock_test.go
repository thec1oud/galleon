package clock_test

import (
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
