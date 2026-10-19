package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Supervisor runs Processes copies of a binary on one host and keeps them
// running. Each worker gets its slot and generation in the environment and
// shares listening ports through Listen.
//
// On SIGHUP (or Upgrade), it starts a new generation from the binary currently
// on disk, waits until every new worker reports ready, then stops the old
// generation. If the new generation fails to become ready, it is stopped and
// the old one keeps serving. A worker asked to stop gets SIGTERM, then SIGKILL
// if it is still running after StopTimeout.
type Supervisor struct {
	Processes int
	// Path is the binary to run. Empty means the running executable, resolved
	// once per generation, so an upgrade picks up a replaced binary while
	// crash restarts keep their generation's binary.
	Path string
	// Args are the worker's arguments. Nil means this process's arguments.
	Args []string
	// Env is added to this process's environment for every worker.
	Env    []string
	Logger *slog.Logger

	ReadyTimeout time.Duration // how long a new generation has to become ready; default 60s
	StopTimeout  time.Duration // how long a worker has to exit after SIGTERM; default 30s
	MinBackoff   time.Duration // delay before restarting a crashed worker; default 100ms
	MaxBackoff   time.Duration // the delay doubles per crash up to this; default 10s
	// StableAfter is how long a worker must run for its restart backoff to reset; default 1m.
	StableAfter time.Duration

	initOnce sync.Once
	upgrades chan chan error
	done     chan struct{} // closed when Run returns
}

type worker struct {
	slot    int
	gen     uint64
	path    string
	cmd     *exec.Cmd
	started time.Time
	ready   chan struct{}
	exited  chan struct{}
	err     error // set before exited is closed
	stopped bool  // the supervisor asked it to exit; don't restart it
}

func (s *Supervisor) defaults() {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if s.ReadyTimeout == 0 {
		s.ReadyTimeout = 60 * time.Second
	}
	if s.StopTimeout == 0 {
		s.StopTimeout = 30 * time.Second
	}
	if s.MinBackoff == 0 {
		s.MinBackoff = 100 * time.Millisecond
	}
	if s.MaxBackoff == 0 {
		s.MaxBackoff = 10 * time.Second
	}
	if s.StableAfter == 0 {
		s.StableAfter = time.Minute
	}
	if s.Args == nil {
		s.Args = os.Args[1:]
	}
}

func (s *Supervisor) init() {
	s.initOnce.Do(func() {
		s.upgrades = make(chan chan error)
		s.done = make(chan struct{})
	})
}

// ErrStopped is returned by Upgrade when the supervisor has stopped, or stops
// before the upgrade finishes.
var ErrStopped = errors.New("process: supervisor stopped")

// Upgrade starts a new generation and retires the old one once the new one is
// ready. It returns when the upgrade has finished or failed, when ctx ends, or
// with ErrStopped when Run returns first. Called before Run, it waits for Run
// to start.
func (s *Supervisor) Upgrade(ctx context.Context) error {
	s.init()
	reply := make(chan error, 1) // buffered: the loop never blocks answering
	select {
	case s.upgrades <- reply:
	case <-s.done:
		return ErrStopped
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-s.done:
		select { // Run may have answered just before returning
		case err := <-reply:
			return err
		default:
			return ErrStopped
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run starts the workers and supervises them until ctx ends, then stops them
// all. It can be called once.
func (s *Supervisor) Run(ctx context.Context) error {
	s.init()
	defer close(s.done)
	if s.Processes < 1 {
		return errors.New("process: Supervisor.Processes must be at least 1")
	}
	if !reusePortSupported && s.Processes > 1 {
		return errors.New("process: several workers need SO_REUSEPORT, which this platform does not support")
	}
	s.defaults()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	var (
		gen          uint64 = 1
		current             = make([]*worker, s.Processes)
		all                 = map[*worker]bool{}
		exits               = make(chan *worker)
		restarts            = make(chan int)
		backoff             = make([]time.Duration, s.Processes)
		upgrading    bool
		pendingReply chan error
		upgradeDone  = make(chan upgradeResult, 1) // one upgrade at a time; buffered so it never blocks after Run returns
	)
	scheduleRestart := func(slot int, d time.Duration) {
		time.AfterFunc(d, func() {
			select {
			case restarts <- slot:
			case <-ctx.Done():
			}
		})
	}
	nextBackoff := func(slot int) time.Duration {
		d := max(backoff[slot], s.MinBackoff)
		backoff[slot] = min(2*d, s.MaxBackoff)
		return d
	}
	start := func(slot int, gen uint64, path string) (*worker, error) {
		w, err := s.start(slot, gen, path, exits)
		if err != nil {
			return nil, err
		}
		all[w] = true
		return w, nil
	}
	upgrade := func(reply chan error) {
		if upgrading {
			if reply != nil {
				reply <- errors.New("process: an upgrade is already in progress")
			}
			return
		}
		path, err := s.resolvePath()
		if err != nil {
			if reply != nil {
				reply <- err
			}
			return
		}
		upgrading, pendingReply = true, reply
		gen++
		s.beginUpgrade(gen, path, start, upgradeDone)
	}

	path, err := s.resolvePath()
	if err != nil {
		return err
	}
	for i := range current {
		w, err := start(i, gen, path)
		if err != nil {
			s.stopAll(all, exits)
			return err
		}
		current[i] = w
	}
	s.Logger.Info("supervisor started", "processes", s.Processes, "generation", gen)

	for {
		select {
		case <-ctx.Done():
			s.Logger.Info("supervisor stopping", "workers", len(all))
			s.stopAll(all, exits)
			if pendingReply != nil {
				pendingReply <- ErrStopped
			}
			return nil

		case w := <-exits:
			delete(all, w)
			if w.stopped || current[w.slot] != w {
				continue
			}
			if time.Since(w.started) >= s.StableAfter {
				backoff[w.slot] = 0
			}
			d := nextBackoff(w.slot)
			s.Logger.Warn("worker exited; restarting", "slot", w.slot, "generation", w.gen, "error", w.err, "after", d)
			scheduleRestart(w.slot, d)

		case slot := <-restarts:
			old := current[slot]
			if all[old] {
				continue // replaced by an upgrade meanwhile
			}
			w, err := start(slot, old.gen, old.path)
			if err != nil {
				d := nextBackoff(slot)
				s.Logger.Error("restarting worker failed", "slot", slot, "error", err, "retry_after", d)
				scheduleRestart(slot, d)
				continue
			}
			current[slot] = w

		case <-hup:
			upgrade(nil)

		case reply := <-s.upgrades:
			upgrade(reply)

		case res := <-upgradeDone:
			upgrading = false
			if res.err != nil {
				for _, w := range res.workers {
					if w != nil && all[w] {
						s.retire(w)
					}
				}
				s.Logger.Error("upgrade failed; keeping the current generation", "generation", res.gen, "error", res.err)
			} else {
				for _, w := range current {
					if all[w] {
						s.retire(w)
					}
				}
				copy(current, res.workers)
				clear(backoff)
				for _, w := range current {
					if !all[w] {
						// Became ready, then exited before the upgrade was applied:
						// its exit was ignored as not current, so restart it now.
						scheduleRestart(w.slot, nextBackoff(w.slot))
					}
				}
				s.Logger.Info("upgrade complete", "generation", res.gen)
			}
			if pendingReply != nil {
				pendingReply <- res.err
				pendingReply = nil
			}
		}
	}
}

type upgradeResult struct {
	gen     uint64
	workers []*worker
	err     error
}

func (s *Supervisor) resolvePath() (string, error) {
	if s.Path != "" {
		return s.Path, nil
	}
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("process: finding the executable: %w", err)
	}
	return path, nil
}

// beginUpgrade starts the new generation from the loop goroutine and waits for
// its readiness in another, so exits keep being handled meanwhile.
func (s *Supervisor) beginUpgrade(gen uint64, path string, start func(int, uint64, string) (*worker, error), done chan<- upgradeResult) {
	s.Logger.Info("upgrade starting", "generation", gen)
	ws := make([]*worker, s.Processes)
	var err error
	for i := range ws {
		if ws[i], err = start(i, gen, path); err != nil {
			break
		}
	}
	go func() {
		if err == nil {
			err = waitReady(ws, s.ReadyTimeout)
		}
		done <- upgradeResult{gen: gen, workers: ws, err: err}
	}()
}

func waitReady(ws []*worker, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, w := range ws {
		select {
		case <-w.ready:
			continue // ready counts even if it has exited since
		default:
		}
		select {
		case <-w.ready:
		case <-w.exited:
			select {
			case <-w.ready:
				continue
			default:
			}
			return fmt.Errorf("worker %d of generation %d exited before it was ready: %v", w.slot, w.gen, w.err)
		case <-deadline.C:
			return fmt.Errorf("generation %d was not ready within %s", w.gen, timeout)
		}
	}
	return nil
}

func (s *Supervisor) start(slot int, gen uint64, path string, exits chan<- *worker) (*worker, error) {
	r, wr, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, s.Args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{wr} // fd 3 in the worker
	cmd.Env = append(append(os.Environ(), s.Env...),
		EnvWorker+"="+strconv.Itoa(slot),
		EnvGeneration+"="+strconv.FormatUint(gen, 10),
		EnvReadyFD+"=3",
	)
	if err := cmd.Start(); err != nil {
		r.Close()
		wr.Close()
		return nil, fmt.Errorf("process: starting worker %d: %w", slot, err)
	}
	wr.Close()
	w := &worker{slot: slot, gen: gen, path: path, cmd: cmd, started: time.Now(), ready: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		defer r.Close()
		line, _ := bufio.NewReader(r).ReadString('\n')
		if line == "ready\n" {
			close(w.ready)
		}
	}()
	go func() {
		w.err = cmd.Wait()
		close(w.exited)
		exits <- w
	}()
	return w, nil
}

// retire asks a worker to exit and kills it if it hasn't after StopTimeout,
// so a worker stuck draining can't keep serving old code on a shared port.
func (s *Supervisor) retire(w *worker) {
	w.stopped = true
	_ = w.cmd.Process.Signal(syscall.SIGTERM)
	go func() {
		t := time.NewTimer(s.StopTimeout)
		defer t.Stop()
		select {
		case <-w.exited:
		case <-t.C:
			s.Logger.Warn("worker did not stop in time; killing it", "slot", w.slot, "generation", w.gen)
			_ = w.cmd.Process.Kill()
		}
	}()
}

// stopAll retires every worker and waits for all of them to exit.
func (s *Supervisor) stopAll(all map[*worker]bool, exits <-chan *worker) {
	for w := range all {
		if !w.stopped {
			s.retire(w)
		}
	}
	for len(all) > 0 {
		delete(all, <-exits)
	}
}
