package metrics

import (
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"
)

// NewServer returns the INTERNAL HTTP server of a binary (api :9091,
// worker :9092). It serves:
//
//	GET /metrics          Prometheus scrape endpoint for m's registry
//	    /debug/pprof/...  Go profiles (CPU, heap, goroutines, block, mutex)
//
// Neither may ever be on the public API port: metrics reveal traffic and
// error rates, and pprof reveals stack traces, command-line flags and can
// burn CPU on request (/debug/pprof/profile). Only Prometheus (inside the
// Compose network) and a developer with `docker compose exec` reach this port.
//
// The pprof handlers are registered on this server's OWN mux. Importing
// net/http/pprof also registers them on http.DefaultServeMux, but no server
// in SeatLock uses the default mux (the public API is a chi router), so that
// side effect exposes nothing.
func NewServer(addr string, m *Metrics) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())

	mux.HandleFunc("/debug/pprof/", pprof.Index) // also serves heap, goroutine, block, mutex, allocs
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile) // CPU profile, ?seconds=N
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// No WriteTimeout: a CPU profile streams for ?seconds=N (30 by default).
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

// EnableContentionProfiles turns on the block and mutex profiles, which are
// OFF by default because sampling every blocking event costs CPU.
//
//	blockRate:     sample one blocking event per blockRate nanoseconds spent
//	               blocked (1 = every event; 0 = off)
//	mutexFraction: sample 1 in mutexFraction contended mutex events (0 = off)
//
// Set them only while profiling (PPROF_BLOCK_RATE / PPROF_MUTEX_FRACTION),
// never for benchmark runs whose numbers are reported.
func EnableContentionProfiles(blockRate, mutexFraction int) {
	runtime.SetBlockProfileRate(blockRate)
	runtime.SetMutexProfileFraction(mutexFraction)
}
