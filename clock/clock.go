// Package clock is the time seam. Code that reads the time or waits takes a
// Clock instead of calling the time package, so tests can control time.
package clock

import "time"

// Clock is the source of time for everything the framework schedules.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	Until(t time.Time) time.Duration
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
	// AfterFunc calls f in its own goroutine once d has elapsed.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer mirrors *time.Timer. C is nil for timers created by AfterFunc.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Ticker mirrors *time.Ticker.
type Ticker interface {
	C() <-chan time.Time
	Stop()
	Reset(d time.Duration)
}

// System returns the clock backed by the time package.
func System() Clock { return system{} }

type system struct{}

func (system) Now() time.Time                            { return time.Now() }
func (system) Since(t time.Time) time.Duration           { return time.Since(t) }
func (system) Until(t time.Time) time.Duration           { return time.Until(t) }
func (system) NewTimer(d time.Duration) Timer            { return sysTimer{time.NewTimer(d)} }
func (system) NewTicker(d time.Duration) Ticker          { return sysTicker{time.NewTicker(d)} }
func (system) AfterFunc(d time.Duration, f func()) Timer { return sysTimer{time.AfterFunc(d, f)} }

type sysTimer struct{ t *time.Timer }

func (t sysTimer) C() <-chan time.Time        { return t.t.C }
func (t sysTimer) Stop() bool                 { return t.t.Stop() }
func (t sysTimer) Reset(d time.Duration) bool { return t.t.Reset(d) }

type sysTicker struct{ t *time.Ticker }

func (t sysTicker) C() <-chan time.Time   { return t.t.C }
func (t sysTicker) Stop()                 { t.t.Stop() }
func (t sysTicker) Reset(d time.Duration) { t.t.Reset(d) }
