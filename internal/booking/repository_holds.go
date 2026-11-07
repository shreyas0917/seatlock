// repository_holds.go holds the SQL for seat holds: create a hold, confirm a
// hold into a booking, and release (cancel) a hold.
//
// Where it fits: Service.HoldSeat calls CreateHold AFTER the Redis lock was
// acquired; Service.CancelHold calls ReleaseHold. Paying for a hold and
// turning it into a booking is in repository_payments.go. PostgreSQL is the AUTHORITY: whatever
// Redis said, these transactions make the final decision.
//
// Every expiry decision uses the DATABASE clock (NOW()), never the Go
// server's clock. Several API servers may disagree by a few seconds about
// what time it is, but they all ask the same database.
//
// Two new database ideas appear here:
//
// pgx.Tx and `defer tx.Rollback(ctx)`: a transaction groups statements so
// they all happen or none do. We start one with pool.Begin and immediately
// `defer tx.Rollback(ctx)`. If the function returns early (an error, or a
// business rule says no), the deferred Rollback undoes everything. If we
// reach tx.Commit, the changes are saved, and the deferred Rollback becomes a
// harmless no-op. So there is no path that leaves a half-done transaction.
//
// SELECT ... FOR UPDATE: reads a row AND locks it until the transaction ends.
// Any other transaction that tries to lock the same row waits. This
// serialises everything that touches one seat (or one hold), so "check, then
// write" is safe INSIDE the transaction: nobody can change the row between
// our check and our write.
package booking

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// holdColumns is the column list every hold query returns, in scanHold order.
// seconds_left is computed by Postgres from its own clock, rounded up, never negative.
const holdColumns = `id, show_id, seat_id, user_id, status, expires_at, created_at,
	GREATEST(0, CEIL(EXTRACT(EPOCH FROM (expires_at - NOW()))))::bigint AS seconds_left`

// scanHold reads one row selected with holdColumns.
// pgx.Row is anything with a Scan method: a QueryRow result from the pool or a tx.
func scanHold(row pgx.Row) (Hold, error) {
	var h Hold
	var status string
	err := row.Scan(&h.ID, &h.ShowID, &h.SeatID, &h.UserID, &status, &h.ExpiresAt, &h.CreatedAt, &h.SecondsLeft)
	h.Status = HoldStatus(status)
	return h, err
}

// CreateHold creates an ACTIVE hold for a seat, inside one transaction.
//
// It returns created=true for a new hold (201), or created=false with the
// user's EXISTING active hold (200), which makes retrying a hold request safe.
//
// Why PostgreSQL re-checks even though Redis already said yes: Redis can lose
// its data (restart, FLUSHALL, failover). Then user B can get the Redis lock
// for a seat that user A still holds in Postgres. Step 4 below catches that and
// returns ErrSeatHeld: Postgres wins, Redis never overrides it.
func (r *PostgresRepository) CreateHold(ctx context.Context, in HoldSeatInput, ttl time.Duration) (Hold, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Hold{}, false, dbError(err, "begin create hold")
	}
	defer tx.Rollback(ctx) // undoes everything unless we reach Commit

	// 1. Lock the seat row. Every hold attempt for this seat now queues here,
	//    one at a time, until the current transaction commits or rolls back.
	var seatID int64
	err = tx.QueryRow(ctx, `
		SELECT id FROM seats
		WHERE id = $1 AND show_id = $2
		FOR UPDATE`, in.SeatID, in.ShowID).Scan(&seatID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, false, ErrSeatNotInShow
	}
	if err != nil {
		return Hold{}, false, dbError(err, "lock seat %d", in.SeatID)
	}

	// 2. Lazy expiry: an ACTIVE hold that is dead (deadHoldCondition: time
	//    up, and no payment in flight or that payment stuck past
	//    PendingHoldMax) is marked EXPIRED now, so it no longer blocks the
	//    seat or the one-active-hold-per-seat index. A hold whose payment is
	//    still in flight is NOT expired here: the paying user keeps the seat.
	if _, err := tx.Exec(ctx, `
		UPDATE holds h SET status = 'EXPIRED', updated_at = NOW()
		WHERE h.show_id = $1 AND h.seat_id = $2
		  AND h.status = 'ACTIVE' AND `+deadHoldCondition(3), in.ShowID, in.SeatID, r.limits.PendingHoldMax.Seconds()); err != nil {
		return Hold{}, false, dbError(err, "expire old holds for seat %d", in.SeatID)
	}

	// 3. A booked seat can never be held again.
	var booked bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM bookings WHERE show_id = $1 AND seat_id = $2)`,
		in.ShowID, in.SeatID).Scan(&booked); err != nil {
		return Hold{}, false, dbError(err, "check booking for seat %d", in.SeatID)
	}
	if booked {
		return Hold{}, false, ErrSeatAlreadyBooked
	}

	// 4. Is there still an ACTIVE (unexpired) hold? This is THE authoritative check.
	existing, err := scanHold(tx.QueryRow(ctx, `
		SELECT `+holdColumns+`
		FROM holds
		WHERE show_id = $1 AND seat_id = $2 AND status = 'ACTIVE'`, in.ShowID, in.SeatID))
	if err == nil && existing.UserID == in.UserID {
		// The user is already paying for this hold: a new hold would be
		// meaningless (and the old one may show seconds_left 0). Say so.
		var paying bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM payments WHERE hold_id = $1 AND status = 'PENDING')`,
			existing.ID).Scan(&paying); err != nil {
			return Hold{}, false, dbError(err, "check payments of hold %d", existing.ID)
		}
		if paying {
			return Hold{}, false, ErrPaymentInProgress
		}
	}
	switch {
	case err == nil && existing.UserID == in.UserID:
		// Same user asked again (e.g. a retry after a timeout): give back
		// the same hold instead of creating a duplicate.
		if err := tx.Commit(ctx); err != nil {
			return Hold{}, false, dbError(err, "commit existing hold")
		}
		return existing, false, nil
	case err == nil:
		// Someone else holds it in Postgres, whatever Redis said.
		return Hold{}, false, ErrSeatHeld
	case !errors.Is(err, pgx.ErrNoRows):
		return Hold{}, false, dbError(err, "find active hold for seat %d", in.SeatID)
	}

	// 5. Create the hold. expires_at uses the database clock.
	hold, err := scanHold(tx.QueryRow(ctx, `
		INSERT INTO holds (show_id, seat_id, user_id, status, expires_at)
		VALUES ($1, $2, $3, 'ACTIVE', NOW() + make_interval(secs => $4))
		RETURNING `+holdColumns, in.ShowID, in.SeatID, in.UserID, ttl.Seconds()))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == constraintHoldsOneActivePerSeat {
			// Cannot happen while step 1 holds the row lock; the partial
			// unique index is the database-level backstop.
			return Hold{}, false, ErrSeatHeld
		}
		return Hold{}, false, dbError(err, "insert hold")
	}

	if err := tx.Commit(ctx); err != nil {
		return Hold{}, false, dbError(err, "commit hold")
	}
	return hold, true, nil
}

// ReleaseHold cancels the user's ACTIVE hold (status -> RELEASED), in one
// transaction that also writes an outbox RELEASE_LOCK row, so deleting the
// Redis lock is never forgotten even if Redis is down right now (Phase 4's
// worker drains the outbox). It returns the hold and that outbox row's id.
//
// Cancelling an already-released hold returns it again (safe to retry,
// outbox id 0). A hold that is missing or belongs to someone else ->
// ErrHoldNotFound, already confirmed -> ErrHoldAlreadyUsed, expired ->
// ErrHoldExpired, a payment in flight -> ErrPaymentInProgress.
func (r *PostgresRepository) ReleaseHold(ctx context.Context, userID string, holdID int64) (Hold, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Hold{}, 0, dbError(err, "begin release hold")
	}
	defer tx.Rollback(ctx)

	h, err := lockHold(ctx, tx, holdID)
	if err != nil {
		return Hold{}, 0, err
	}
	if h.UserID != userID {
		return Hold{}, 0, ErrHoldNotFound // same answer as "does not exist"
	}
	switch {
	case h.Status == HoldReleased:
		hold, err := scanHold(tx.QueryRow(ctx, `SELECT `+holdColumns+` FROM holds WHERE id = $1`, h.ID))
		if err != nil {
			return Hold{}, 0, dbError(err, "get hold %d", h.ID)
		}
		return hold, 0, tx.Commit(ctx) // already cancelled: retry-safe
	case h.Status == HoldConsumed:
		return Hold{}, 0, ErrHoldAlreadyUsed
	case h.Status != HoldActive, h.Expired:
		return Hold{}, 0, ErrHoldExpired
	}

	// Do not cancel in the middle of a payment: the charge may succeed a
	// moment later, and the user expects a booking, not a refund.
	var paying bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM payments WHERE hold_id = $1 AND status = 'PENDING')`,
		h.ID).Scan(&paying); err != nil {
		return Hold{}, 0, dbError(err, "check payments of hold %d", h.ID)
	}
	if paying {
		return Hold{}, 0, ErrPaymentInProgress
	}

	hold, err := scanHold(tx.QueryRow(ctx, `
		UPDATE holds SET status = 'RELEASED', updated_at = NOW()
		WHERE id = $1
		RETURNING `+holdColumns, h.ID))
	if err != nil {
		return Hold{}, 0, dbError(err, "release hold %d", h.ID)
	}
	payload, err := json.Marshal(map[string]any{"show_id": h.ShowID, "seat_id": h.SeatID, "owner": h.UserID})
	if err != nil {
		return Hold{}, 0, err
	}
	var outboxID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO outbox (kind, payload) VALUES ('RELEASE_LOCK', $1)
		RETURNING id`, payload).Scan(&outboxID); err != nil {
		return Hold{}, 0, dbError(err, "insert release outbox row for hold %d", h.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		return Hold{}, 0, dbError(err, "commit release hold %d", h.ID)
	}
	return hold, outboxID, nil
}
