// Package process is the multi-process model: a supervisor that runs N copies
// of the app on one host, sharing listening ports with SO_REUSEPORT, restarting
// crashed workers, and upgrading to a new binary without dropping traffic.
//
// Multi-node deployments use the same binary with no supervisor, or one per
// host; the code never changes, only the coordination backend does.
package process

import (
	"os"
	"strconv"
	"sync"
)

// Environment variables a supervisor sets on each worker.
const (
	EnvWorker     = "GALLEON_WORKER"     // the worker's slot, 0..N-1
	EnvGeneration = "GALLEON_GENERATION" // increases with every upgrade
	EnvReadyFD    = "GALLEON_READY_FD"   // where the worker reports readiness
)

// Supervised reports whether this process was started by a Supervisor.
func Supervised() bool {
	_, ok := os.LookupEnv(EnvWorker)
	return ok
}

// Slot returns this worker's slot, or -1 when not supervised.
func Slot() int {
	n, err := strconv.Atoi(os.Getenv(EnvWorker))
	if err != nil {
		return -1
	}
	return n
}

var readyOnce sync.Once

// NotifyReady tells the supervisor this worker is ready for traffic. During an
// upgrade the old generation is only stopped once every new worker has called
// it. It does nothing when the process is not supervised, and only the first
// call has an effect. The kernel calls it when the app first becomes ready.
func NotifyReady() error {
	var err error
	readyOnce.Do(func() {
		s := os.Getenv(EnvReadyFD)
		if s == "" {
			return
		}
		fd, perr := strconv.Atoi(s)
		if perr != nil {
			err = perr
			return
		}
		f := os.NewFile(uintptr(fd), "galleon-ready")
		if f == nil {
			return
		}
		defer f.Close()
		_, err = f.Write([]byte("ready\n"))
	})
	return err
}
