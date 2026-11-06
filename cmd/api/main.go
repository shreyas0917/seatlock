// Command api starts the SeatLock HTTP server.
//
// Phase 0 version: it only loads config and answers GET /healthz, which proves
// the build, the Docker image and the Compose wiring work. Phase 1 replaces
// this with the full booking API.
package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/shreyas0917/seatlock/internal/config"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	logger.Info("listening", "addr", cfg.HTTPAddr)
	if err := http.ListenAndServe(cfg.HTTPAddr, mux); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
