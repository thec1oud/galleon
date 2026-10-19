package galleon

import (
	"net/http"
)

// HealthHandler serves Kubernetes-style probes:
//
//	/livez   200 while the process is up and not stopped
//	/readyz  200 only when started, not draining, and every readiness gate passes
//
// Failure reasons are only written to the response when verbose is set, since
// gate errors can describe internal topology.
func (a *App) HealthHandler(verbose bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		st := a.state
		a.mu.Unlock()
		if st == stateStopped {
			http.Error(w, "stopped", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Ready(r.Context()); err != nil {
			msg := "not ready"
			if verbose {
				msg = err.Error()
			}
			http.Error(w, msg, http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	return mux
}
