// service_payments.go holds the business rules for confirming a hold:
// pay, then book (or refund).
//
//	POST /v1/bookings -> ConfirmBooking:
//
//	  Tx A   repo.BeginPayment     lock hold, check it, create/reuse PENDING payment, COMMIT
//	  ---    gateway.Charge        network call, NO transaction open
//	  Tx B   repo.CompletePayment  book, or record that a refund is owed (+ outbox), COMMIT
//	  ---    gateway.Refund        only if a refund is owed (best effort, inline)
//
// Nothing here touches Redis before the database work: confirming an
// existing hold must keep working while Redis is down. Idempotency of the
// HTTP request (Idempotency-Key) is handled at the HTTP edge
// (internal/idempotency); THIS layer makes the payment itself safe to
// repeat: one PENDING payment per hold, one charge_key per payment, and a
// gateway that never charges a key twice.
package booking

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// ConfirmBooking charges the user for their active hold and turns it into a
// booking. The four payment outcomes:
//
//	succeeded, hold still valid   -> the booking (201)
//	succeeded, hold expired/gone  -> refund, *HoldExpiredError with the refund state (410)
//	declined                      -> ErrPaymentDeclined (402); the hold stays ACTIVE
//	no answer (timeout, error)    -> ErrPaymentPending (504); the payment stays PENDING
//
// The final guard against double booking is still the Phase 1 one: the
// database's UNIQUE(show_id, seat_id) constraint on bookings. We never write
// "if the seat is free { insert }" (check-then-insert) in Go:
//
//	time  user A                       user B
//	t1    SELECT -> seat 7 is free
//	t2                                 SELECT -> seat 7 is free
//	t3    INSERT booking (seat 7)
//	t4                                 INSERT booking (seat 7)   <- double booking!
//
// Between the check and the insert there is a gap, and another request can
// slip into it. A Go mutex would not help either: with 2+ API servers, each
// has its own mutex. The database is the one place every request goes
// through, and a UNIQUE constraint is checked atomically inside Postgres: of
// two concurrent inserts for the same seat, exactly one commits and the other
// gets error 23505, which becomes ErrSeatAlreadyBooked.
func (s *Service) ConfirmBooking(ctx context.Context, in ConfirmInput) (Booking, error) {
	in.UserID = strings.TrimSpace(in.UserID)
	if in.UserID == "" {
		return Booking{}, ErrMissingUserID
	}
	if in.HoldID <= 0 {
		return Booking{}, fmt.Errorf("%w: hold_id must be a positive number", ErrInvalidInput)
	}

	// Tx A: commits before the network call below.
	payment, _, err := s.repo.BeginPayment(ctx, in)
	if err != nil {
		return Booking{}, err
	}

	// The network call. No transaction is open and no row is locked.
	// charge_key is the payment's own idempotency key at the gateway: if
	// this exact payment was charged before (a retry, a concurrent request),
	// the gateway answers with the stored result instead of charging again.
	charge, err := s.gateway.Charge(ctx, ChargeRequest{
		IdempotencyKey: payment.ChargeKey,
		AmountPaise:    payment.AmountPaise,
		Reference:      fmt.Sprintf("hold-%d", payment.HoldID),
	})
	if err != nil {
		// NO ANSWER IS NOT "FAILED". The gateway may have taken the money
		// and only the reply was lost. Marking the payment failed now could
		// leave the user charged with no booking and no refund. So the
		// payment stays PENDING, and the client gets 504: retrying with the
		// same Idempotency-Key re-sends the SAME charge_key and learns the
		// real outcome. (Phase 4's worker reconciles abandoned PENDINGs.)
		return Booking{}, fmt.Errorf("%w: payment %d: %w", ErrPaymentPending, payment.ID, err)
	}

	if charge.Status == ChargeDeclined {
		// A definite "no": no money was taken. The hold stays ACTIVE so the
		// user can try again; the next attempt is a NEW payment with a new
		// charge_key (a declined key would only replay "declined").
		if err := s.repo.MarkPaymentDeclined(ctx, payment.ID); err != nil {
			// The decline is real, but not recorded: the payment is still
			// PENDING. A retry re-sends the same key, gets "declined"
			// again, and records it then. Report the error, not a decline.
			return Booking{}, err
		}
		return Booking{}, ErrPaymentDeclined
	}

	// Tx B: the money was taken. Book, or record that a refund is owed.
	result, err := s.repo.CompletePayment(ctx, payment.ID, charge.ChargeID)
	if err != nil {
		// Money taken, but we could not record the outcome (e.g. a database
		// timeout). The payment is still PENDING; a retry replays the charge
		// and runs Tx B again. To the client the outcome is unknown: 504.
		return Booking{}, fmt.Errorf("%w: payment %d: %w", ErrPaymentPending, payment.ID, err)
	}

	switch result.Kind {
	case Booked:
		// The seat is sold, so its Redis lock is no longer needed. Best
		// effort: a leftover lock on a BOOKED seat is harmless (Postgres
		// rejects new holds on it) and expires by its TTL.
		s.releaseLock(ctx, result.Booking.ShowID, result.Booking.SeatID, in.UserID)
		return result.Booking, nil

	case RefundRequired:
		status := s.refundInline(ctx, result.Payment, charge.ChargeID, result.OutboxID)
		return Booking{}, &HoldExpiredError{PaymentStatus: status}

	default: // AlreadyRefunding: an earlier request already decided to refund.
		return Booking{}, &HoldExpiredError{PaymentStatus: result.Payment.Status}
	}
}

// refundInline tries to refund right away, so the user gets their money back
// without waiting for the Phase 4 worker. It returns the payment's status
// afterwards: REFUNDED if it worked, REFUND_PENDING if not. It never returns
// an error: the refund is ALREADY recorded as owed (outbox row, committed in
// Tx B), so a failure here only means "later, by the worker".
func (s *Service) refundInline(ctx context.Context, payment Payment, chargeID string, outboxID int64) PaymentStatus {
	ctx = context.WithoutCancel(ctx) // finish even if the client hung up

	if _, err := s.gateway.Refund(ctx, RefundRequest{IdempotencyKey: payment.RefundKey(), ChargeID: chargeID}); err != nil {
		slog.Warn("inline refund failed; the outbox row stays PENDING for the worker",
			"payment_id", payment.ID, "outbox_id", outboxID, "error", err.Error())
		if err := s.repo.RecordOutboxFailure(ctx, outboxID, err.Error()); err != nil {
			slog.Warn("could not record outbox failure", "outbox_id", outboxID, "error", err.Error())
		}
		return PaymentRefundPending
	}

	refunded, err := s.repo.MarkRefunded(ctx, payment.ID, outboxID)
	if err != nil {
		// The money WAS returned, but we could not record it. The outbox
		// row stays PENDING; the worker will repeat the refund with the same
		// refund key, the gateway replays "refunded", and then it is recorded.
		slog.Warn("refund done but not recorded; the worker will reconcile",
			"payment_id", payment.ID, "error", err.Error())
		return PaymentRefundPending
	}
	return refunded.Status
}
