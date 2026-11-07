// repository_payments.go holds the SQL for paying for a hold.
//
// Where it fits (Service.ConfirmBooking, service_payments.go):
//
//	Tx A  BeginPayment     lock hold, check it, create/reuse a PENDING payment, COMMIT
//	      gateway.Charge   NO transaction open (see below)
//	Tx B  CompletePayment  lock hold + payment, then book OR mark refund owed, COMMIT
//
// *** NEVER HOLD A TRANSACTION OR ROW LOCK ACROSS A NETWORK CALL ***
// A gateway call can take seconds (or until the 5s timeout). If Tx A stayed
// open during it, the hold row would stay locked: every other request for
// that hold or seat (a retry, a cancel, a hold attempt by someone else)
// would queue behind it, and a slow gateway would turn into a pile-up of
// blocked database connections. So Tx A commits BEFORE the call, Tx B starts
// AFTER it, and everything in between is protected by state stored in the
// rows (payment = PENDING), not by locks.
//
// LOCK ORDER: every transaction here locks the HOLD row first and the
// PAYMENT row second. Two transactions that lock the same rows in the same
// order can never deadlock each other.
package booking

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// paymentColumns is the column list every payment query returns, in scanPayment order.
const paymentColumns = `id, hold_id, user_id, amount_paise, charge_key, COALESCE(provider_charge_id, ''), status`

func scanPayment(row pgx.Row) (Payment, error) {
	var p Payment
	var status string
	err := row.Scan(&p.ID, &p.HoldID, &p.UserID, &p.AmountPaise, &p.ChargeKey, &p.ProviderChargeID, &status)
	p.Status = PaymentStatus(status)
	return p, err
}

// lockedHold is the hold state that the payment transactions need.
type lockedHold struct {
	ID, ShowID, SeatID int64
	UserID             string
	Status             HoldStatus
	Expired            bool // expires_at <= NOW(), by the database clock
}

// lockHold reads a hold and locks its row until the transaction ends.
func lockHold(ctx context.Context, tx pgx.Tx, holdID int64) (lockedHold, error) {
	var h lockedHold
	var status string
	err := tx.QueryRow(ctx, `
		SELECT id, show_id, seat_id, user_id, status, expires_at <= NOW()
		FROM holds WHERE id = $1
		FOR UPDATE`, holdID).Scan(&h.ID, &h.ShowID, &h.SeatID, &h.UserID, &status, &h.Expired)
	h.Status = HoldStatus(status)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedHold{}, ErrHoldNotFound
	}
	if err != nil {
		return lockedHold{}, dbError(err, "lock hold %d", holdID)
	}
	return h, nil
}

// latestPaymentStatus returns the status of the hold's most recent payment
// ("" if none). Used to tell the user, on a 410, whether money was refunded.
func latestPaymentStatus(ctx context.Context, q pgx.Tx, holdID int64) (PaymentStatus, error) {
	var status string
	err := q.QueryRow(ctx, `
		SELECT status FROM payments WHERE hold_id = $1
		ORDER BY id DESC LIMIT 1`, holdID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", dbError(err, "latest payment of hold %d", holdID)
	}
	return PaymentStatus(status), nil
}

// BeginPayment is Tx A: it checks the hold and returns the PENDING payment
// to charge, creating it if needed. It returns created=true for a new payment.
//
// One live payment per hold: if the hold already has a PENDING payment (an
// earlier request is in flight, or its outcome was unknown), that SAME
// payment is returned, with the SAME charge_key. Sending it to the gateway
// again can never charge twice: the gateway replays its stored result. This
// holds even when the second request uses a different Idempotency-Key.
func (r *PostgresRepository) BeginPayment(ctx context.Context, in ConfirmInput) (Payment, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Payment{}, false, dbError(err, "begin payment")
	}
	defer tx.Rollback(ctx)

	h, err := lockHold(ctx, tx, in.HoldID)
	if err != nil {
		return Payment{}, false, err
	}
	// Another user's hold gets the same answer as a missing one (no probing).
	if h.UserID != in.UserID {
		return Payment{}, false, ErrHoldNotFound
	}

	// 1. A payment already in flight for this hold? Reuse it, even if the
	//    hold has expired meanwhile: its outcome is unknown, and only asking
	//    the gateway again (same charge_key) tells us whether money was
	//    taken, so we can book or refund. Giving up here would leave money
	//    possibly taken and nobody refunding it.
	pending, err := scanPayment(tx.QueryRow(ctx, `
		SELECT `+paymentColumns+` FROM payments
		WHERE hold_id = $1 AND status = 'PENDING'`, h.ID))
	switch {
	case err == nil:
		if err := tx.Commit(ctx); err != nil {
			return Payment{}, false, dbError(err, "commit payment reuse")
		}
		return pending, false, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Payment{}, false, dbError(err, "find pending payment of hold %d", h.ID)
	}

	// 2. No payment in flight: the hold itself must be usable.
	switch {
	case h.Status == HoldConsumed:
		return Payment{}, false, ErrHoldAlreadyUsed
	case h.Status != HoldActive, h.Expired:
		status, err := latestPaymentStatus(ctx, tx, h.ID)
		if err != nil {
			return Payment{}, false, err
		}
		return Payment{}, false, &HoldExpiredError{PaymentStatus: refundStatusOnly(status)}
	}

	// 3. Start a new payment for the seat's price. charge_key ("pay-<id>")
	//    is generated by the database from the new id.
	p, err := scanPayment(tx.QueryRow(ctx, `
		INSERT INTO payments (hold_id, user_id, amount_paise)
		SELECT $1, $2, s.price_paise FROM seats s WHERE s.id = $3
		RETURNING `+paymentColumns, h.ID, h.UserID, h.SeatID))
	if err != nil {
		return Payment{}, false, dbError(err, "insert payment for hold %d", h.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		return Payment{}, false, dbError(err, "commit payment")
	}
	return p, true, nil
}

// refundStatusOnly keeps only refund states, the ones worth telling a user
// whose hold expired ("" otherwise, e.g. for an old DECLINED attempt).
func refundStatusOnly(s PaymentStatus) PaymentStatus {
	if s == PaymentRefundPending || s == PaymentRefunded {
		return s
	}
	return ""
}

// CompletePayment is Tx B, run after the gateway said the charge SUCCEEDED.
//
// RULE: it never refunds a payment that already backs a booking. It locks
// the hold and the payment, then decides by the PAYMENT's state first:
//
//	SUCCEEDED (booking exists)     -> return that booking. No refund, no error.
//	REFUND_PENDING / REFUNDED      -> already decided: AlreadyRefunding, nothing written.
//	PENDING + hold still valid     -> book: booking, hold CONSUMED, payment SUCCEEDED.
//	PENDING + hold expired/invalid -> REFUND_PENDING + outbox REFUND row (same tx).
//
// Two requests can reach Tx B for the same payment (same charge_key, e.g. two
// confirms with different Idempotency-Keys). Locking serialises them: the
// first books, the second sees SUCCEEDED and returns the same booking.
func (r *PostgresRepository) CompletePayment(ctx context.Context, paymentID int64, chargeID string) (CompleteResult, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return CompleteResult{}, dbError(err, "begin complete payment")
	}
	defer tx.Rollback(ctx)

	// hold_id never changes, so it is safe to read it before locking.
	var holdID int64
	if err := tx.QueryRow(ctx, `SELECT hold_id FROM payments WHERE id = $1`, paymentID).Scan(&holdID); err != nil {
		return CompleteResult{}, dbError(err, "find payment %d", paymentID)
	}
	h, err := lockHold(ctx, tx, holdID) // lock order: hold first...
	if err != nil {
		return CompleteResult{}, err
	}
	p, err := scanPayment(tx.QueryRow(ctx, `
		SELECT `+paymentColumns+` FROM payments WHERE id = $1
		FOR UPDATE`, paymentID)) // ...payment second
	if err != nil {
		return CompleteResult{}, dbError(err, "lock payment %d", paymentID)
	}

	switch p.Status {
	case PaymentSucceeded:
		// Someone already booked with this payment. Return that booking.
		b, err := bookingForHold(ctx, tx, h.ID)
		if err != nil {
			return CompleteResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return CompleteResult{}, dbError(err, "commit complete payment %d", p.ID)
		}
		return CompleteResult{Kind: Booked, Booking: b, Payment: p, ShowID: h.ShowID, SeatID: h.SeatID}, nil
	case PaymentRefundPending, PaymentRefunded:
		if err := tx.Commit(ctx); err != nil {
			return CompleteResult{}, dbError(err, "commit complete payment %d", p.ID)
		}
		return CompleteResult{Kind: AlreadyRefunding, Payment: p, ShowID: h.ShowID, SeatID: h.SeatID}, nil
	case PaymentDeclined:
		// The gateway cannot say "succeeded" for a key it declined (it
		// replays stored results), so this means a bug somewhere.
		return CompleteResult{}, dbError(errors.New("payment is DECLINED but the gateway reported success"), "complete payment %d", p.ID)
	}

	// p.Status == PENDING.
	holdValid := h.Status == HoldActive && !h.Expired
	if holdValid {
		var b Booking
		err = tx.QueryRow(ctx, `
			INSERT INTO bookings (show_id, seat_id, user_id, hold_id)
			VALUES ($1, $2, $3, $4)
			RETURNING id, show_id, seat_id, user_id, hold_id, created_at`,
			h.ShowID, h.SeatID, h.UserID, h.ID).
			Scan(&b.ID, &b.ShowID, &b.SeatID, &b.UserID, &b.HoldID, &b.CreatedAt)
		if err != nil {
			// The UNIQUE constraints still stand guard. With an ACTIVE hold
			// locked this cannot happen; if it ever does, nothing is
			// committed, the payment stays PENDING (money taken, outcome
			// recorded nowhere yet), and Phase 4's reconciler resolves it.
			return CompleteResult{}, mapInsertBookingError(err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE holds SET status = 'CONSUMED', updated_at = NOW() WHERE id = $1`, h.ID); err != nil {
			return CompleteResult{}, dbError(err, "consume hold %d", h.ID)
		}
		p, err = scanPayment(tx.QueryRow(ctx, `
			UPDATE payments SET status = 'SUCCEEDED', provider_charge_id = $2, updated_at = NOW()
			WHERE id = $1
			RETURNING `+paymentColumns, p.ID, chargeID))
		if err != nil {
			return CompleteResult{}, dbError(err, "mark payment %d succeeded", p.ID)
		}
		if err := tx.Commit(ctx); err != nil {
			return CompleteResult{}, mapInsertBookingError(err)
		}
		return CompleteResult{Kind: Booked, Booking: b, Payment: p, ShowID: h.ShowID, SeatID: h.SeatID}, nil
	}

	// Money was taken, but the hold expired (or was released) while the
	// gateway was working. There is no booking for this hold (a booking
	// would mean a SUCCEEDED payment, and only one live payment per hold can
	// exist). So: owe a refund. The status change and the outbox row are
	// written in ONE transaction, so the debt can never be forgotten.
	if h.Status == HoldActive && h.Expired {
		if _, err := tx.Exec(ctx, `
			UPDATE holds SET status = 'EXPIRED', updated_at = NOW() WHERE id = $1`, h.ID); err != nil {
			return CompleteResult{}, dbError(err, "expire hold %d", h.ID)
		}
	}
	p, err = scanPayment(tx.QueryRow(ctx, `
		UPDATE payments SET status = 'REFUND_PENDING', provider_charge_id = $2, updated_at = NOW()
		WHERE id = $1
		RETURNING `+paymentColumns, p.ID, chargeID))
	if err != nil {
		return CompleteResult{}, dbError(err, "mark payment %d refund pending", p.ID)
	}
	payload, err := json.Marshal(map[string]any{
		"payment_id": p.ID,
		"charge_id":  chargeID,
		"refund_key": p.RefundKey(),
	})
	if err != nil {
		return CompleteResult{}, err
	}
	var outboxID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO outbox (kind, payload) VALUES ('REFUND', $1)
		RETURNING id`, payload).Scan(&outboxID); err != nil {
		return CompleteResult{}, dbError(err, "insert refund outbox row for payment %d", p.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		return CompleteResult{}, dbError(err, "commit refund for payment %d", p.ID)
	}
	return CompleteResult{Kind: RefundRequired, Payment: p, OutboxID: outboxID, ShowID: h.ShowID, SeatID: h.SeatID}, nil
}

// bookingForHold returns the booking that was created from a hold.
func bookingForHold(ctx context.Context, tx pgx.Tx, holdID int64) (Booking, error) {
	var b Booking
	err := tx.QueryRow(ctx, `
		SELECT id, show_id, seat_id, user_id, hold_id, created_at
		FROM bookings WHERE hold_id = $1`, holdID).
		Scan(&b.ID, &b.ShowID, &b.SeatID, &b.UserID, &b.HoldID, &b.CreatedAt)
	if err != nil {
		return Booking{}, dbError(err, "find booking of hold %d", holdID)
	}
	return b, nil
}

// MarkPaymentDeclined records a definite decline. The hold stays ACTIVE, so
// the user may try again (which creates a NEW payment with a new charge_key).
func (r *PostgresRepository) MarkPaymentDeclined(ctx context.Context, paymentID int64) error {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	_, err := r.pool.Exec(ctx, `
		UPDATE payments SET status = 'DECLINED', updated_at = NOW()
		WHERE id = $1 AND status = 'PENDING'`, paymentID)
	if err != nil {
		return dbError(err, "mark payment %d declined", paymentID)
	}
	return nil
}

// MarkPaymentAbandoned records that a PENDING payment's key was voided at the
// gateway: no money was taken, and none can be for that key. Like DECLINED,
// ABANDONED is outside payments_one_live_per_hold, so the user may pay again
// with a NEW payment and a new charge_key.
func (r *PostgresRepository) MarkPaymentAbandoned(ctx context.Context, paymentID int64) error {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	_, err := r.pool.Exec(ctx, `
		UPDATE payments SET status = 'ABANDONED', updated_at = NOW()
		WHERE id = $1 AND status = 'PENDING'`, paymentID)
	if err != nil {
		return dbError(err, "mark payment %d abandoned", paymentID)
	}
	return nil
}

// MarkRefunded records a successful refund: payment REFUNDED and its outbox
// row DONE, together.
func (r *PostgresRepository) MarkRefunded(ctx context.Context, paymentID, outboxID int64) (Payment, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Payment{}, dbError(err, "begin mark refunded")
	}
	defer tx.Rollback(ctx)

	p, err := scanPayment(tx.QueryRow(ctx, `
		UPDATE payments SET status = 'REFUNDED', updated_at = NOW()
		WHERE id = $1 AND status IN ('REFUND_PENDING', 'REFUNDED')
		RETURNING `+paymentColumns, paymentID))
	if err != nil {
		return Payment{}, dbError(err, "mark payment %d refunded", paymentID)
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox SET status = 'DONE' WHERE id = $1`, outboxID); err != nil {
		return Payment{}, dbError(err, "mark outbox %d done", outboxID)
	}
	if err := tx.Commit(ctx); err != nil {
		return Payment{}, dbError(err, "commit mark refunded")
	}
	return p, nil
}

// MarkOutboxDone marks an outbox row as handled (e.g. the Redis lock was
// released inline, so the Phase 4 worker has nothing left to do).
func (r *PostgresRepository) MarkOutboxDone(ctx context.Context, outboxID int64) error {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	if _, err := r.pool.Exec(ctx, `UPDATE outbox SET status = 'DONE' WHERE id = $1`, outboxID); err != nil {
		return dbError(err, "mark outbox %d done", outboxID)
	}
	return nil
}
