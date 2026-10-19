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
// the old one keeps serving.
type Supervisor struct {
	Processes int
	// Path is the binary to run. Empty means the running executable, resolved
	// again for each generation so a replaced binary is picked up by upgrades.
	Path string
	// Args are the worker's arguments. Nil means this process's arguments.
	Args []string
	// Env is added to this process's environment for every worker.
	Env    []string
	Logger *slog.Logger

	ReadyTimeout time.Duration // how long a new generation has to become ready; default 60s
	StopTimeout  time.Duration // how long a worker has to exit after SIGTERM; default 30s
	MinBackoff   time.Duration // first restart delay after a crash; default 100ms
	MaxBackoff   time.Duration // default 10s
	// StableAfter is how long a worker must run for its restart backoff to reset; default 1m.
	StableAfter time.Duration

	upgrades chan chan error
}

type worker struct {
	slot    int
	gen     uint64
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

// Upgrade starts a new generation and retires the old one once the new one is
// ready. It returns when the upgrade has finished or failed. It must be called
// while Run is running.
func (s *Supervisor) Upgrade(ctx context.Context) error {
	done := make(chan error, 1)
	select {
	case s.upgrades <- done:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Init prepares the supervisor so Upgrade can be called before Run starts
// serving it. Run calls it too.
func (s *Supervisor) Init() {
	if s.upgrades == nil {
		s.upgrades = make(chan chan error)
	}
}

// Run starts the workers and supervises them until ctx ends, then stops them
// all: SIGTERM, then SIGKILL for any still running after StopTimeout.
func (s *Supervisor) Run(ctx context.Context) error {
	if s.Processes < 1 {
		return errors.New("process: Supervisor.Processes must be at least 1")
	}
	if !reusePortSupported && s.Processes > 1 {
		return errors.New("process: several workers need SO_REUSEPORT, which this platform does not support")
	}
	s.defaults()
	s.Init()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	var (
		gen         uint64 = 1
		current            = make([]*worker, s.Processes)
		all                = map[*worker]bool{}
		exits              = make(chan *worker)
		restarts           = make(chan int)
		backoff            = make([]time.Duration, s.Processes)
		upgrading   bool
		upgradeDone = make(chan upgradeResult, 1) // one upgrade at a time; buffered so it never blocks after Run returns
	)
	scheduleRestart := func(slot int, d time.Duration) {
		time.AfterFunc(d, func() {
			select {
			case restarts <- slot:
			case <-ctx.Done():
			}
		})
	}
	start := func(slot int, gen uint64) (*worker, error) {
		w, err := s.start(slot, gen, exits)
		if err != nil {
			return nil, err
		}
		all[w] = true
		return w, nil
	}

	for i := range current {
		w, err := start(i, gen)
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
			return nil

		case w := <-exits:
			delete(all, w)
			if w.stopped || current[w.slot] != w {
				continue
			}
			if time.Since(w.started) >= s.StableAfter {
				backoff[w.slot] = 0
			}
			d := backoff[w.slot]
			backoff[w.slot] = min(max(2*d, s.MinBackoff), s.MaxBackoff)
			s.Logger.Warn("worker exited; restarting", "slot", w.slot, "generation", w.gen, "error", w.err, "after", d)
			scheduleRestart(w.slot, d)

		case slot := <-restarts:
			if old := current[slot]; old != nil && all[old] {
				continue // replaced by an upgrade meanwhile
			}
			w, err := start(slot, current[slot].gen)
			if err != nil {
				s.Logger.Error("restarting worker failed", "slot", slot, "error", err)
				d := backoff[slot]
				backoff[slot] = min(max(2*d, s.MinBackoff), s.MaxBackoff)
				scheduleRestart(slot, d)
				continue
			}
			current[slot] = w

		case <-hup:
			if !upgrading {
				upgrading = true
				gen++
				s.beginUpgrade(gen, start, upgradeDone, nil)
			}

		case reply := <-s.upgrades:
			if upgrading {
				reply <- errors.New("process: an upgrade is already in progress")
				continue
			}
			upgrading = true
			gen++
			s.beginUpgrade(gen, start, upgradeDone, reply)

		case res := <-upgradeDone:
			upgrading = false
			if res.err != nil {
				for _, w := range res.workers {
					if w != nil && all[w] {
						w.stopped = true
						signalWorker(w, syscall.SIGTERM)
					}
				}
				s.Logger.Error("upgrade failed; keeping the current generation", "generation", res.gen, "error", res.err)
			} else {
				for _, w := range current {
					if all[w] {
						w.stopped = true
						signalWorker(w, syscall.SIGTERM)
					}
				}
				copy(current, res.workers)
				clear(backoff)
				for _, w := range current {
					if !all[w] {
						// Became ready, then exited before the upgrade was applied:
						// its exit was ignored as not current, so restart it now.
						scheduleRestart(w.slot, 0)
					}
				}
				s.Logger.Info("upgrade complete", "generation", res.gen)
			}
			if res.reply != nil {
				res.reply <- res.err
			}
		}
	}
}

type upgradeResult struct {
	gen     uint64
	workers []*worker
	err     error
	reply   chan error
}

// beginUpgrade starts the new generation from the loop goroutine and waits for
// its readiness in another, so exits keep being handled meanwhile.
func (s *Supervisor) beginUpgrade(gen uint64, start func(int, uint64) (*worker, error), done chan<- upgradeResult, reply chan error) {
	s.Logger.Info("upgrade starting", "generation", gen)
	ws := make([]*worker, s.Processes)
	var err error
	for i := range ws {
		if ws[i], err = start(i, gen); err != nil {
			break
		}
	}
	go func() {
		if err == nil {
			err = waitReady(ws, s.ReadyTimeout)
		}
		done <- upgradeResult{gen: gen, workers: ws, err: err, reply: reply}
	}()
}

func waitReady(ws []*worker, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, w := range ws {
		select {
		case <-w.ready:
		case <-w.exited:
			return fmt.Errorf("worker %d of generation %d exited before it was ready: %v", w.slot, w.gen, w.err)
		case <-deadline.C:
			return fmt.Errorf("generation %d was not ready within %s", w.gen, timeout)
		}
	}
	return nil
}

func (s *Supervisor) start(slot int, gen uint64, exits chan<- *worker) (*worker, error) {
	path := s.Path
	if path == "" {
		var err error
		if path, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("process: finding the executable: %w", err)
		}
	}
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
	w := &worker{slot: slot, gen: gen, cmd: cmd, started: time.Now(), ready: make(chan struct{}), exited: make(chan struct{})}
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

// stopAll asks every worker to exit, kills those that don't in time, and waits
// for all of them.
func (s *Supervisor) stopAll(all map[*worker]bool, exits <-chan *worker) {
	for w := range all {
		w.stopped = true
		signalWorker(w, syscall.SIGTERM)
	}
	kill := time.NewTimer(s.StopTimeout)
	defer kill.Stop()
	for len(all) > 0 {
		select {
		case w := <-exits:
			delete(all, w)
		case <-kill.C:
			for w := range all {
				s.Logger.Warn("worker did not stop in time; killing it", "slot", w.slot, "generation", w.gen)
				_ = w.cmd.Process.Kill()
			}
		}
	}
}

func signalWorker(w *worker, sig os.Signal) {
	_ = w.cmd.Process.Signal(sig)
}
