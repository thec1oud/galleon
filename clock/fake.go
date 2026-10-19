package clock

import (
	"sort"
	"sync"
	"time"
)

// Fake is a Clock that only moves when told to. Advance fires timers, tickers
// and AfterFunc callbacks in deadline order, with the clock set to each one's
// deadline as it fires.
//
// Unlike time.AfterFunc, callbacks that fall due during Advance run
// synchronously in the goroutine calling Advance, so their order is
// deterministic. They may use the clock, including scheduling more callbacks,
// but must not block on something only the Advance caller can provide.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
	seq     uint64
	changed chan struct{} // closed and replaced whenever waiters change
}

// NewFake returns a fake clock set to start.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start, changed: make(chan struct{})}
}

type waiter struct {
	at     time.Time
	seq    uint64 // tie-break: earlier registrations fire first
	period time.Duration
	ch     chan time.Time
	fn     func()
	active bool
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }
func (f *Fake) Until(t time.Time) time.Duration { return t.Sub(f.Now()) }

func (f *Fake) NewTimer(d time.Duration) Timer {
	w := &waiter{ch: make(chan time.Time, 1)}
	f.arm(w, d)
	return &fakeTimer{f: f, w: w}
}

func (f *Fake) AfterFunc(d time.Duration, fn func()) Timer {
	w := &waiter{fn: fn}
	f.arm(w, d)
	return &fakeTimer{f: f, w: w}
}

func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	w := &waiter{ch: make(chan time.Time, 1), period: d}
	f.arm(w, d)
	return &fakeTicker{f: f, w: w}
}

// Advance moves the clock forward by d, firing everything that falls due on the
// way, each at its own deadline, including anything a callback schedules
// within the window.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	target := f.now.Add(d)
	for len(f.waiters) > 0 && !f.waiters[0].at.After(target) {
		w := f.waiters[0]
		if w.at.After(f.now) { // never move backwards if a callback advanced further
			f.now = w.at
		}
		if fn := f.fireLocked(w); fn != nil {
			f.mu.Unlock()
			fn()
			f.mu.Lock()
		}
	}
	if target.After(f.now) {
		f.now = target
	}
	f.mu.Unlock()
}

// Set moves the clock to t, which must not be before Now.
func (f *Fake) Set(t time.Time) {
	f.Advance(t.Sub(f.Now()))
}

// Waiters reports how many timers, tickers and callbacks are pending.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

// BlockUntil waits until at least n timers, tickers or callbacks are pending.
// Tests use it to know a goroutine has started waiting before advancing.
func (f *Fake) BlockUntil(n int) {
	for {
		f.mu.Lock()
		if len(f.waiters) >= n {
			f.mu.Unlock()
			return
		}
		ch := f.changed
		f.mu.Unlock()
		<-ch
	}
}

// fireLocked delivers a due waiter and re-arms tickers on their schedule. It
// returns the callback to run, which the caller runs without holding mu.
func (f *Fake) fireLocked(w *waiter) func() {
	f.removeLocked(w)
	if w.period > 0 {
		w.at = w.at.Add(w.period)
		f.insertLocked(w)
	}
	if w.fn != nil {
		return w.fn
	}
	select {
	case w.ch <- f.now:
	default: // like time.Ticker, drop ticks the reader hasn't taken
	}
	return nil
}

func (f *Fake) arm(w *waiter, d time.Duration) {
	f.mu.Lock()
	fn := f.armLocked(w, d)
	f.mu.Unlock()
	if fn != nil {
		go fn() // like time.AfterFunc(0, fn): not on the caller's stack
	}
}

// armLocked schedules w after d. A non-positive d fires at once; the returned
// callback, if any, must be run by the caller after releasing mu.
func (f *Fake) armLocked(w *waiter, d time.Duration) func() {
	w.at = f.now.Add(d)
	f.insertLocked(w)
	if d <= 0 {
		return f.fireLocked(w)
	}
	return nil
}

func (f *Fake) insertLocked(w *waiter) {
	f.seq++
	w.seq = f.seq
	w.active = true
	i := sort.Search(len(f.waiters), func(i int) bool {
		o := f.waiters[i]
		return o.at.After(w.at) || (o.at.Equal(w.at) && o.seq > w.seq)
	})
	f.waiters = append(f.waiters, nil)
	copy(f.waiters[i+1:], f.waiters[i:])
	f.waiters[i] = w
	f.notifyLocked()
}

// removeLocked reports whether w was pending.
func (f *Fake) removeLocked(w *waiter) bool {
	if !w.active {
		return false
	}
	for i, o := range f.waiters {
		if o == w {
			f.waiters = append(f.waiters[:i], f.waiters[i+1:]...)
			break
		}
	}
	w.active = false
	f.notifyLocked()
	return true
}

func (f *Fake) notifyLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

// drain discards an undelivered value, as Stop and Reset do on time.Timer and
// time.Ticker since Go 1.23.
func drain(ch chan time.Time) {
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
	}
}

type fakeTimer struct {
	f *Fake
	w *waiter
}

func (t *fakeTimer) C() <-chan time.Time { return t.w.ch }

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	drain(t.w.ch)
	return t.f.removeLocked(t.w)
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.f.mu.Lock()
	drain(t.w.ch)
	wasActive := t.f.removeLocked(t.w)
	fn := t.f.armLocked(t.w, d)
	t.f.mu.Unlock()
	if fn != nil {
		go fn()
	}
	return wasActive
}

type fakeTicker struct {
	f *Fake
	w *waiter
}

func (t *fakeTicker) C() <-chan time.Time { return t.w.ch }

func (t *fakeTicker) Stop() {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	drain(t.w.ch)
	t.f.removeLocked(t.w)
}

func (t *fakeTicker) Reset(d time.Duration) {
	if d <= 0 {
		panic("clock: non-positive interval for Ticker.Reset")
	}
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	drain(t.w.ch)
	t.f.removeLocked(t.w)
	t.w.period = d
	t.f.armLocked(t.w, d) // d > 0, so nothing fires here
}
