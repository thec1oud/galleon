//go:build unix

package process_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/process"
)

// TestHelperWorker is the worker binary: the supervisor re-runs this test
// binary with GALLEON_TEST_HELPER set. It boots a real app that serves its
// generation and PID on a shared port.
func TestHelperWorker(t *testing.T) {
	if os.Getenv("GALLEON_TEST_HELPER") != "1" {
		t.Skip("only runs as a supervised worker")
	}
	node := mustNode()
	switch os.Getenv("HELPER_MODE") {
	case "crash-once":
		marker := filepath.Join(os.Getenv("HELPER_DIR"), "crashed")
		if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
			_ = os.WriteFile(marker, nil, 0o600)
			os.Exit(3)
		}
	case "never-ready-after-1":
		if node > 1 {
			<-make(chan struct{}) // never becomes ready; the supervisor stops it
		}
	}

	var srv *http.Server
	mod := galleon.ModuleFunc("web", func(r *galleon.Registry) error {
		if !r.Active("web") {
			return nil
		}
		r.OnStart(func(ctx context.Context) error {
			ln, err := process.Listen(ctx, "tcp", os.Getenv("HELPER_ADDR"))
			if err != nil {
				return err
			}
			srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, "%d %d", node, os.Getpid())
			})}
			go srv.Serve(ln)
			return nil
		})
		r.OnDrain(func(ctx context.Context) error { return srv.Shutdown(ctx) })
		return nil
	})
	app, err := galleon.New(
		galleon.Logger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		galleon.Coordination(local.New()),
		galleon.Deploy(galleon.SingleNode),
		galleon.Roles("web"),
		galleon.DrainGrace(0),
		galleon.Modules(mod),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := app.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func mustNode() uint64 {
	var gen uint64
	fmt.Sscan(os.Getenv(process.EnvGeneration), &gen)
	return gen
}

type harness struct {
	t    *testing.T
	addr string
	sup  *process.Supervisor
	logs *syncBuf
	done chan error
	stop context.CancelFunc
}

func newHarness(t *testing.T, n int, mode string, configure ...func(*process.Supervisor)) *harness {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	logs := &syncBuf{}
	h := &harness{t: t, addr: addr, logs: logs, done: make(chan error, 1)}
	h.sup = &process.Supervisor{
		Processes:    n,
		Path:         os.Args[0],
		Args:         []string{"-test.run=^TestHelperWorker$"},
		Env:          []string{"GALLEON_TEST_HELPER=1", "HELPER_MODE=" + mode, "HELPER_ADDR=" + addr, "HELPER_DIR=" + t.TempDir()},
		Logger:       slog.New(slog.NewTextHandler(logs, nil)),
		ReadyTimeout: 10 * time.Second,
		StopTimeout:  5 * time.Second,
		MinBackoff:   10 * time.Millisecond,
	}
	for _, c := range configure {
		c(h.sup)
	}
	h.sup.Init()
	ctx, cancel := context.WithCancel(context.Background())
	h.stop = cancel
	go func() { h.done <- h.sup.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			t.Error("supervisor did not stop")
		}
	})
	return h
}

// get makes one request on a new connection, so SO_REUSEPORT may pick any worker.
func (h *harness) get() (gen string, pid string, err error) {
	c := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get("http://" + h.addr)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	gen, pid, _ = strings.Cut(string(b), " ")
	return gen, pid, nil
}

// pids samples responses until it has seen want distinct workers of generation gen.
func (h *harness) waitWorkers(gen string, want int) map[string]bool {
	h.t.Helper()
	seen := map[string]bool{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		g, pid, err := h.get()
		if err == nil && g == gen {
			seen[pid] = true
			if len(seen) >= want {
				return seen
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("saw %d workers of generation %s, want %d\nsupervisor log:\n%s", len(seen), gen, want, h.logs)
	return nil
}

func TestWorkersShareOnePort(t *testing.T) {
	h := newHarness(t, 3, "")
	h.waitWorkers("1", 3)
}

func TestCrashedWorkerIsRestarted(t *testing.T) {
	h := newHarness(t, 1, "crash-once")
	h.waitWorkers("1", 1)
	if !strings.Contains(h.logs.String(), "worker exited; restarting") {
		t.Fatalf("restart not logged:\n%s", h.logs)
	}
}

func TestUpgradeReplacesTheGeneration(t *testing.T) {
	h := newHarness(t, 2, "")
	old := h.waitWorkers("1", 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.sup.Upgrade(ctx); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, h.logs)
	}
	h.waitWorkers("2", 2)
	// After the old generation drains, every response comes from generation 2.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		alive := 0
		for pid := range old {
			var p int
			fmt.Sscan(pid, &p)
			if proc, err := os.FindProcess(p); err == nil && proc.Signal(syscall.Signal(0)) == nil {
				alive++
			}
		}
		if alive == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for range 20 {
		if g, _, err := h.get(); err != nil || g != "2" {
			t.Fatalf("after upgrade got generation %q, err %v", g, err)
		}
	}
}

func TestFailedUpgradeKeepsTheOldGeneration(t *testing.T) {
	h := newHarness(t, 2, "never-ready-after-1", func(s *process.Supervisor) { s.ReadyTimeout = time.Second })
	h.waitWorkers("1", 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.sup.Upgrade(ctx); err == nil {
		t.Fatal("upgrade to a generation that never gets ready should fail")
	}
	for range 20 {
		if g, _, err := h.get(); err != nil || g != "1" {
			t.Fatalf("old generation should keep serving: generation %q, err %v", g, err)
		}
	}
}

func TestStopEndsEveryWorker(t *testing.T) {
	h := newHarness(t, 2, "")
	h.waitWorkers("1", 2)
	h.stop()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not stop")
	}
	h.done <- nil // for the cleanup
	if _, _, err := h.get(); err == nil {
		t.Fatal("port still served after stop")
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
