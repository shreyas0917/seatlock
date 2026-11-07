// store.go runs the invariant checks against PostgreSQL (SQL only; the
// worker's invariants job turns the results into metrics).
//
// COST, and why it is acceptable: every 15s, each check runs once
// (COUNT(*) over its query) plus four small aggregate queries. Most checks
// use partial indexes (ACTIVE holds, PENDING payments/outbox); the join
// checks (bookings x payments) scan tables that are small at this project's
// scale (thousands of rows: a few milliseconds). Two guards keep it from
// ever loading the database:
//   - each check runs in its own short transaction with
//     SET LOCAL statement_timeout (default 2s): a check that becomes slow is
//     cancelled by Postgres itself and reported as an error, never left
//     running;
//   - checks run one after another on a single connection, never in parallel.
//
// At much larger scale the heavy checks would move to a read replica or run
// less often; the gauges and alerts would stay the same.
package invariants

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store runs checks on a pool.
type Store struct {
	Pool             *pgxpool.Pool
	Checks           []Check       // normally invariants.Checks; tests may pass others
	StatementTimeout time.Duration // per query (INVARIANT_STATEMENT_TIMEOUT)
}

// Result is the outcome of one check: the number of violating rows, or the
// error that prevented counting them.
type Result struct {
	Check      string
	Violations int
	Err        error
}

// Run runs every check, one after another, and returns one Result each.
func (s *Store) Run(ctx context.Context) []Result {
	out := make([]Result, 0, len(s.Checks))
	for _, c := range s.Checks {
		n, err := s.count(ctx, "SELECT COUNT(*) FROM ("+c.SQL+") AS violations")
		out = append(out, Result{Check: c.Name, Violations: int(n), Err: err})
	}
	return out
}

// Stats are cheap health numbers exported next to the checks.
type Stats struct {
	PendingPaymentOldestAge float64 // seconds, 0 if none
	OutboxPending           float64
	OutboxFailed            float64
	OutboxOldestPendingAge  float64 // seconds, 0 if none
	HoldsActive             float64
}

// Stats reads the gauges for the dashboard. Each query is index-backed
// (partial indexes from migration 0005).
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	queries := []struct {
		dst *float64
		sql string
	}{
		{&st.PendingPaymentOldestAge, `SELECT COALESCE(EXTRACT(EPOCH FROM NOW() - MIN(created_at)), 0) FROM payments WHERE status = 'PENDING'`},
		{&st.OutboxPending, `SELECT COUNT(*) FROM outbox WHERE status = 'PENDING'`},
		{&st.OutboxFailed, `SELECT COUNT(*) FROM outbox WHERE status = 'FAILED'`},
		{&st.OutboxOldestPendingAge, `SELECT COALESCE(EXTRACT(EPOCH FROM NOW() - MIN(created_at)), 0) FROM outbox WHERE status = 'PENDING'`},
		{&st.HoldsActive, `SELECT COUNT(*) FROM holds WHERE status = 'ACTIVE'`},
	}
	for _, q := range queries {
		v, err := s.count(ctx, q.sql)
		if err != nil {
			return Stats{}, err
		}
		*q.dst = v
	}
	return st, nil
}

// count runs one single-number query in its own transaction with a
// statement timeout, so no query can run longer than StatementTimeout.
func (s *Store) count(ctx context.Context, sql string) (float64, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // read-only: nothing to commit
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", s.StatementTimeout.Milliseconds())); err != nil {
		return 0, fmt.Errorf("set statement_timeout: %w", err)
	}
	var v float64
	if err := tx.QueryRow(ctx, sql).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}
