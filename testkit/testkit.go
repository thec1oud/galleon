// Package testkit boots a real Galleon app inside a test, with fakes in place
// of the seams that touch time and the outside world.
package testkit

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/clock"
	"github.com/thec1oud/galleon/coord/local"
)

// Epoch is where every Harness clock starts, so tests are reproducible.
var Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Harness is a started app plus the fakes it runs on.
type Harness struct {
	*galleon.App
	// Clock drives time for the app, unless opts passed another galleon.Clock.
	Clock *clock.Fake
	// Logs holds everything the app logged, at every level.
	Logs *Logs
}

// New builds and starts an app for t and shuts it down when t ends. The app
// runs on a fake clock starting at Epoch, the in-process coordination backend
// as a single node, no drain grace, and a logger that records into Logs and
// prints them only if the test fails. opts are applied after those defaults,
// so any of them can be overridden; use galleon.Replace to swap in fakes.
//
// The in-process coordination backend keeps real time: advancing Clock does
// not expire its leases, TTLs or counter windows.
func New(t testing.TB, opts ...galleon.Option) *Harness {
	t.Helper()
	h := &Harness{Clock: clock.NewFake(Epoch), Logs: &Logs{}}
	defaults := []galleon.Option{
		galleon.Name("test"),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
		galleon.DrainGrace(0),
		galleon.Clock(h.Clock),
		galleon.Logger(slog.New(slog.NewTextHandler(h.Logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	}
	app, err := galleon.New(append(defaults, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("starting app: %v\n%s", err, h.Logs)
	}
	h.App = app
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("shutting down app: %v", err)
		}
		if t.Failed() {
			t.Logf("app logs:\n%s", h.Logs)
		}
	})
	return h
}

// Get returns the app's instance of T.
func Get[T any](h *Harness) T { return galleon.MustResolve[T](h.App) }

// Logs is a concurrency-safe record of log output.
type Logs struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *Logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *Logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Contains reports whether any log output contains s.
func (l *Logs) Contains(s string) bool { return strings.Contains(l.String(), s) }
