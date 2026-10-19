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

// The supervisor's variables are read once at startup and removed from the
// environment, and the readiness pipe is closed on exec, so processes this
// worker starts don't believe they are supervised.
var (
	slot    = -1
	readyFD = -1
)

func init() {
	if s, ok := os.LookupEnv(EnvWorker); ok {
		if n, err := strconv.Atoi(s); err == nil {
			slot = n
		}
		os.Unsetenv(EnvWorker)
	}
	if s, ok := os.LookupEnv(EnvReadyFD); ok {
		if n, err := strconv.Atoi(s); err == nil {
			readyFD = n
			closeOnExec(n)
		}
		os.Unsetenv(EnvReadyFD)
	}
}

// Supervised reports whether this process was started by a Supervisor.
func Supervised() bool { return slot >= 0 }

// Slot returns this worker's slot, or -1 when not supervised.
func Slot() int { return slot }

var readyOnce sync.Once

// NotifyReady tells the supervisor this worker is ready for traffic. During an
// upgrade the old generation is only stopped once every new worker has called
// it. It does nothing when the process is not supervised, and only the first
// call has an effect. The kernel calls it when the app first becomes ready.
func NotifyReady() error {
	var err error
	readyOnce.Do(func() {
		if readyFD < 0 {
			return
		}
		f := os.NewFile(uintptr(readyFD), "galleon-ready")
		if f == nil {
			return
		}
		defer f.Close()
		_, err = f.Write([]byte("ready\n"))
	})
	return err
}
