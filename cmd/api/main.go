// Command api starts the SeatLock HTTP server.
//
// This is where everything is wired together, in this order:
//
//	config -> logger -> migrations -> Postgres pool -> repository -> service -> router -> HTTP server
//
// A request then flows the other way:
//
//	HTTP request -> router/middleware -> handler (internal/httpapi)
//	             -> booking.Service (business rules) -> PostgresRepository (SQL) -> PostgreSQL
//
// The server shuts down gracefully on Ctrl+C (SIGINT) or SIGTERM (what Docker
// sends on `docker compose down`): it stops accepting new connections,
// finishes in-flight requests, then closes the database pool.
//
// The @-lines below are general swag annotations for the generated API docs
// (served at /swagger/index.html; regenerate with `make swagger`).
//
//	@title			SeatLock API
//	@version		1.0
//	@description	High-concurrency ticket booking backend. A seat can never be booked twice:
//	@description	PostgreSQL's UNIQUE(show_id, seat_id) constraint is the final authority.
//	@BasePath		/
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Blank import: registers the generated OpenAPI spec so /swagger can serve it.
	_ "github.com/shreyas0917/seatlock/docs"
	"github.com/shreyas0917/seatlock/internal/booking"
	"github.com/shreyas0917/seatlock/internal/config"
	"github.com/shreyas0917/seatlock/internal/db"
	"github.com/shreyas0917/seatlock/internal/httpapi"
	"github.com/shreyas0917/seatlock/internal/lock"
)

// How long to wait for in-flight requests to finish during shutdown.
const shutdownTimeout = 15 * time.Second

func main() {
	// run holds the real logic and returns an error, which is easier to read
	// and test than calling os.Exit from many places.
	if err := run(); err != nil {
		slog.Error("server failed", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// JSON logs: one object per line, easy for log tools to search and filter.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	// ctx is cancelled when the process receives SIGINT or SIGTERM.
	// That cancellation is our signal to start shutting down.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.RunMigrations {
		logger.Info("running database migrations")
		if err := db.MigrateUp(cfg.DatabaseURL); err != nil {
			return err
		}
	}

	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBMinConns)
	if err != nil {
		return err
	}
	// Runs last, after the HTTP server has finished shutting down.
	defer pool.Close()

	// Redis client for the seat lock. go-redis connects lazily, so the API
	// starts even if Redis is down: new holds then fail closed (503), while
	// confirms and reads keep working from Postgres.
	redisClient := lock.NewClient(lock.Options{
		Addr:         cfg.RedisAddr,
		DialTimeout:  cfg.RedisDialTimeout,
		ReadTimeout:  cfg.RedisReadTimeout,
		WriteTimeout: cfg.RedisWriteTimeout,
	})
	defer redisClient.Close()
	locker := lock.NewRedisLocker(redisClient, cfg.HoldTTL, cfg.RedisLockExtra)

	repo := booking.NewPostgresRepository(pool)
	svc := booking.NewService(repo, locker, cfg.HoldTTL)
	router := httpapi.NewRouter(svc, pool, logger)

	server := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: router,
		// Timeouts protect the server from slow or stuck clients, which could
		// otherwise hold connections (and goroutines) open forever.
		ReadHeaderTimeout: 5 * time.Second,  // time to read request headers
		ReadTimeout:       10 * time.Second, // time to read the whole request
		WriteTimeout:      15 * time.Second, // time to write the response
		IdleTimeout:       60 * time.Second, // how long a keep-alive connection may sit idle
	}

	// ListenAndServe blocks, so it runs in its own goroutine while main waits
	// for either a server error or a shutdown signal. The buffered channel
	// lets that goroutine send its result without waiting for a receiver.
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTPAddr)
		serverErr <- server.ListenAndServe()
	}()

	// select waits until one of the channels has a value.
	select {
	case err := <-serverErr:
		// The server stopped on its own (e.g. port already in use).
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, finishing in-flight requests")
	}

	// Give in-flight requests up to shutdownTimeout to finish. A fresh
	// context is used because ctx is already cancelled.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}

	// After Shutdown, ListenAndServe returns http.ErrServerClosed: expected, not an error.
	if err := <-serverErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("server stopped cleanly")
	return nil
}
