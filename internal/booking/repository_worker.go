// repository_worker.go holds the SQL the background worker (cmd/worker) uses
// to FIND and CLAIM work. What to DO with a claimed row is not here: that is
// in service_worker.go, which reuses the same repository methods as the API
// (CompletePayment, MarkRefunded, ...).
//
// *** FOR UPDATE SKIP LOCKED: many workers, no leader ***
// Every claim query below has the shape
//
//	UPDATE t SET <lease> WHERE id IN (
//	    SELECT id FROM t WHERE <due> ORDER BY ... LIMIT n
//	    FOR UPDATE SKIP LOCKED)
//
// FOR UPDATE locks the selected rows. SKIP LOCKED means: if another
// transaction already locked a row, do not WAIT for it, just skip it and
// take the next one. So when two workers claim at the same moment, each gets
// a DIFFERENT set of rows, and neither blocks the other. No leader election,
// no coordination service: Postgres row locks are the coordination.
//
// *** CLAIM-THEN-WORK with a lease ***
// The claim is one short statement that commits at once. It moves the row's
// "due" time into the future (the LEASE, e.g. NOW() + 60s) so no other worker
// picks it up, and counts the attempt. The network call (gateway, Redis)
// happens AFTER the commit, with no transaction open (Phase 3's golden rule).
// The result is recorded in a new short statement. If the worker crashes in
// between, nothing is lost: when the lease passes, the row is due again and
// any worker claims it.
package booking

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// backoffSQL is the retry delay after the attempts-th failure:
//
//	min(2^attempts, 300) seconds, plus 0-20% random jitter.
//
// Exponential so a broken dependency is not hammered; capped at 5 minutes so
// recovery is noticed quickly; jitter so many rows that failed together
// (e.g. during a gateway outage) do not all retry in the same second.
// Computed in SQL so every worker uses the database clock.
func backoffSQL(attemptsColumn string) string {
	return fmt.Sprintf("NOW() + make_interval(secs => LEAST(power(2, %s), 300) * (1 + random() * 0.2))", attemptsColumn)
}

// ClaimPaymentsToReconcile claims up to limit PENDING payments created more
// than olderThan ago that are due for a check, leasing each for lease.
func (r *PostgresRepository) ClaimPaymentsToReconcile(ctx context.Context, olderThan, lease time.Duration, limit int) ([]Payment, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	rows, err := r.pool.Query(ctx, `
		UPDATE payments
		SET next_check_at = NOW() + make_interval(secs => $2),
		    reconcile_attempts = reconcile_attempts + 1,
		    updated_at = NOW()
		WHERE id IN (
			SELECT id FROM payments
			WHERE status = 'PENDING'
			  AND created_at <= NOW() - make_interval(secs => $1)
			  AND (next_check_at IS NULL OR next_check_at <= NOW())
			ORDER BY created_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED)
		RETURNING `+paymentColumns, olderThan.Seconds(), lease.Seconds(), limit)
	if err != nil {
		return nil, dbError(err, "claim payments to reconcile")
	}
	return collectPayments(rows)
}

// RecordReconcileFailure notes why a check failed (e.g. gateway down) and
// schedules the next check with backoff. The payment stays PENDING.
func (r *PostgresRepository) RecordReconcileFailure(ctx context.Context, paymentID int64, failure string) error {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	_, err := r.pool.Exec(ctx, `
		UPDATE payments
		SET last_error = $2, next_check_at = `+backoffSQL("reconcile_attempts")+`, updated_at = NOW()
		WHERE id = $1 AND status = 'PENDING'`, paymentID, failure)
	if err != nil {
		return dbError(err, "record reconcile failure for payment %d", paymentID)
	}
	return nil
}

func collectPayments(rows pgx.Rows) ([]Payment, error) {
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, dbError(err, "scan payment")
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError(err, "read payments")
	}
	return out, nil
}

// OutboxKind is what an outbox row asks for.
type OutboxKind string

const (
	OutboxRefund      OutboxKind = "REFUND"
	OutboxReleaseLock OutboxKind = "RELEASE_LOCK"
)

// OutboxStatus is an outbox row's lifecycle state.
type OutboxStatus string

const (
	OutboxPending OutboxStatus = "PENDING"
	OutboxDone    OutboxStatus = "DONE"
	OutboxFailed  OutboxStatus = "FAILED" // dead letter: no automatic retries
)

// OutboxRow is one claimed outbox row.
type OutboxRow struct {
	ID       int64
	Kind     OutboxKind
	Payload  json.RawMessage
	Attempts int // including the current one
}

// ClaimOutbox claims up to limit PENDING rows that are due, leasing each for
// lease. The attempt is counted AT CLAIM TIME, so a worker that crashes
// mid-call still uses up an attempt: a row that keeps crashing workers ends
// as a FAILED dead letter instead of looping forever.
func (r *PostgresRepository) ClaimOutbox(ctx context.Context, lease time.Duration, limit int) ([]OutboxRow, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	rows, err := r.pool.Query(ctx, `
		UPDATE outbox
		SET attempts = attempts + 1, next_attempt_at = NOW() + make_interval(secs => $1)
		WHERE id IN (
			SELECT id FROM outbox
			WHERE status = 'PENDING' AND next_attempt_at <= NOW()
			ORDER BY next_attempt_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED)
		RETURNING id, kind, payload, attempts`, lease.Seconds(), limit)
	if err != nil {
		return nil, dbError(err, "claim outbox rows")
	}
	defer rows.Close()

	var out []OutboxRow
	for rows.Next() {
		var row OutboxRow
		var kind string
		if err := rows.Scan(&row.ID, &kind, &row.Payload, &row.Attempts); err != nil {
			return nil, dbError(err, "scan outbox row")
		}
		row.Kind = OutboxKind(kind)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError(err, "read outbox rows")
	}
	return out, nil
}

// FailClaimedOutbox records a failed attempt on a row claimed with
// ClaimOutbox (whose attempt was already counted): it stores the error and
// schedules a retry with backoff, or, once OutboxMaxAttempts is reached,
// turns the row into a FAILED dead letter. It returns the row's new status.
//
// Why dead letters: retrying forever is dangerous. A row that can never
// succeed (bad payload, a charge the gateway no longer knows) would be
// retried for all eternity, hiding the problem and wasting work. After N
// attempts we stop, log at ERROR level, and let a human look
// (`make outbox-retry ID=<id>` re-queues it).
func (r *PostgresRepository) FailClaimedOutbox(ctx context.Context, outboxID int64, failure string) (OutboxStatus, error) {
	return r.failOutbox(ctx, outboxID, failure, false)
}

// RecordOutboxFailure records a failed INLINE attempt (the request path
// tried the side effect right away, without claiming the row): it counts the
// attempt, stores the error and schedules the worker's retry with backoff.
func (r *PostgresRepository) RecordOutboxFailure(ctx context.Context, outboxID int64, failure string) error {
	_, err := r.failOutbox(ctx, outboxID, failure, true)
	return err
}

func (r *PostgresRepository) failOutbox(ctx context.Context, outboxID int64, failure string, countAttempt bool) (OutboxStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	increment := 0
	if countAttempt {
		increment = 1
	}
	var status string
	err := r.pool.QueryRow(ctx, `
		UPDATE outbox
		SET attempts = attempts + $3,
		    last_error = $2,
		    status = CASE WHEN attempts + $3 >= $4 THEN 'FAILED' ELSE status END,
		    next_attempt_at = `+backoffSQL("(attempts + $3)")+`
		WHERE id = $1 AND status = 'PENDING'
		RETURNING status`, outboxID, failure, increment, r.limits.OutboxMaxAttempts).Scan(&status)
	if err != nil {
		return "", dbError(err, "record failure for outbox %d", outboxID)
	}
	return OutboxStatus(status), nil
}

// ExpiredHold is one hold the expirer just marked EXPIRED.
type ExpiredHold struct {
	HoldID, ShowID, SeatID int64
	UserID                 string
	OutboxID               int64 // its RELEASE_LOCK row
}

// ExpireHolds marks up to limit dead ACTIVE holds EXPIRED (deadHoldCondition:
// the same rule CreateHold and the seat map use) and writes an outbox
// RELEASE_LOCK row for each, in ONE statement, so ONE transaction: a hold is
// never expired without its Redis lock release being recorded.
//
// A hold whose payment is still PENDING is skipped (until PendingHoldMax):
// the payment flow or the reconciler owns it. Expiring it here would not
// change the payment's outcome (CompletePayment checks expires_at itself),
// but releasing its Redis lock would let another user take a seat whose
// payment may yet succeed.
func (r *PostgresRepository) ExpireHolds(ctx context.Context, limit int) ([]ExpiredHold, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	rows, err := r.pool.Query(ctx, `
		WITH due AS (
			SELECT h.id FROM holds h
			WHERE h.status = 'ACTIVE' AND `+deadHoldCondition(2)+`
			ORDER BY h.expires_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		), expired AS (
			UPDATE holds SET status = 'EXPIRED', updated_at = NOW()
			FROM due WHERE holds.id = due.id
			RETURNING holds.id, holds.show_id, holds.seat_id, holds.user_id
		), released AS (
			INSERT INTO outbox (kind, payload)
			SELECT 'RELEASE_LOCK', json_build_object(
				'show_id', show_id, 'seat_id', seat_id, 'owner', user_id, 'hold_id', id)
			FROM expired
			RETURNING id, (payload->>'hold_id')::bigint AS hold_id
		)
		SELECT e.id, e.show_id, e.seat_id, e.user_id, r.id
		FROM expired e JOIN released r ON r.hold_id = e.id`,
		limit, r.limits.PendingHoldMax.Seconds())
	if err != nil {
		return nil, dbError(err, "expire holds")
	}
	defer rows.Close()

	var out []ExpiredHold
	for rows.Next() {
		var h ExpiredHold
		if err := rows.Scan(&h.HoldID, &h.ShowID, &h.SeatID, &h.UserID, &h.OutboxID); err != nil {
			return nil, dbError(err, "scan expired hold")
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError(err, "read expired holds")
	}
	return out, nil
}

// HasLiveHold reports whether the seat currently has an ACTIVE hold owned by
// owner that is not dead (deadHoldCondition: the same rule as CreateHold, the
// seat map and the expirer). The outbox drainer asks this before releasing a
// Redis lock, so a STALE release can never delete the owner's newer lock.
func (r *PostgresRepository) HasLiveHold(ctx context.Context, showID, seatID int64, owner string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	var live bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM holds h
			WHERE h.show_id = $1 AND h.seat_id = $2 AND h.user_id = $3
			  AND h.status = 'ACTIVE' AND NOT `+deadHoldCondition(4)+`)`,
		showID, seatID, owner, r.limits.PendingHoldMax.Seconds()).Scan(&live)
	if err != nil {
		return false, dbError(err, "check live hold for seat %d", seatID)
	}
	return live, nil
}

// RequeueOutbox puts a FAILED dead letter back in the queue (attempts reset),
// for a human who fixed the cause. Used by `make outbox-retry ID=<id>`.
func (r *PostgresRepository) RequeueOutbox(ctx context.Context, outboxID int64) error {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	_, err := r.pool.Exec(ctx, `
		UPDATE outbox SET status = 'PENDING', attempts = 0, next_attempt_at = NOW()
		WHERE id = $1 AND status = 'FAILED'`, outboxID)
	if err != nil {
		return dbError(err, "requeue outbox %d", outboxID)
	}
	return nil
}
