// Package db creates the PostgreSQL connection pool and runs migrations.
//
// Where it fits: cmd/api/main.go calls NewPool once at startup and hands the
// pool to the booking repository (internal/booking/repository_postgres.go),
// which runs all SQL through it.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool opens a pgx connection pool and checks that the database answers.
//
// What is a pool? Opening a Postgres connection is slow (TCP + auth handshake).
// A pool keeps a set of open connections and lends one to each query, then
// takes it back. *pgxpool.Pool is safe to use from many goroutines at once,
// so the whole app shares ONE pool.
//
// About context.Context (first appearance in the code): almost every function
// that does I/O takes a ctx as its first argument. A context carries a
// deadline and a cancellation signal. If the deadline passes or the caller
// gives up (e.g. the HTTP client disconnects), every function using that ctx
// stops waiting and returns an error, instead of hanging forever.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// %w "wraps" the original error inside ours. Callers can still find
		// the original with errors.Is / errors.As, and the message gets a
		// helpful prefix describing what we were doing.
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	// Close idle connections after a while so Postgres is not holding
	// connections nobody uses.
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	// pgxpool connects lazily, so ping once to fail fast at startup if the
	// database is unreachable, instead of failing on the first request.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	// defer runs cancel() when this function returns, whatever path it takes.
	// Always cancel a context you created, to free its timer.
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}
