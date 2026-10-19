package clock

import (
	"sort"
	"sync"
	"time"
)

// Fake is a Clock that only moves when told to. Timers, tickers and AfterFunc
// callbacks fire in deadline order as Advance passes them.
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
// way, each at its own deadline.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	target := f.now.Add(d)
	for {
		w := f.next(target)
		if w == nil {
			break
		}
		f.now = w.at
		f.fire(w)
	}
	f.now = target
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

// next returns the earliest waiter due at or before target. Callers hold mu.
func (f *Fake) next(target time.Time) *waiter {
	if len(f.waiters) == 0 || f.waiters[0].at.After(target) {
		return nil
	}
	return f.waiters[0]
}

// fire delivers a due waiter and re-arms tickers. Callers hold mu.
func (f *Fake) fire(w *waiter) {
	f.remove(w)
	if w.fn != nil {
		go w.fn()
	} else {
		select {
		case w.ch <- f.now:
		default: // like time.Ticker, drop ticks the reader hasn't taken
		}
	}
	if w.period > 0 {
		w.at = f.now.Add(w.period)
		f.insert(w)
	}
}

func (f *Fake) arm(w *waiter, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.at = f.now.Add(d)
	f.insert(w)
	if d <= 0 {
		f.fire(w)
	}
}

func (f *Fake) insert(w *waiter) {
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
	f.notify()
}

// remove reports whether w was pending.
func (f *Fake) remove(w *waiter) bool {
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
	f.notify()
	return true
}

func (f *Fake) notify() {
	close(f.changed)
	f.changed = make(chan struct{})
}

type fakeTimer struct {
	f *Fake
	w *waiter
}

func (t *fakeTimer) C() <-chan time.Time { return t.w.ch }

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	return t.f.remove(t.w)
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.f.mu.Lock()
	wasActive := t.f.remove(t.w)
	t.f.mu.Unlock()
	t.f.arm(t.w, d)
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
	t.f.remove(t.w)
}

func (t *fakeTicker) Reset(d time.Duration) {
	if d <= 0 {
		panic("clock: non-positive interval for Ticker.Reset")
	}
	t.f.mu.Lock()
	t.f.remove(t.w)
	t.w.period = d
	t.f.mu.Unlock()
	t.f.arm(t.w, d)
}
