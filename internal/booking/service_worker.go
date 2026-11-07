// service_worker.go holds the business actions of the background worker
// (cmd/worker): what to do with ONE claimed payment or outbox row.
//
// Where it fits:
//
//	internal/worker (tickers, claiming with SKIP LOCKED, logging)
//	    -> Service.ReconcilePayment / Service.ProcessOutboxRow   (this file)
//	        -> the SAME repository methods and helpers the API uses
//	           (completeCharged = CompletePayment + lock release,
//	            MarkPaymentDeclined, MarkPaymentAbandoned, MarkRefunded, ...)
//
// There is deliberately no second version of the booking/refund rules here.
// "The charge succeeded; book it or refund it" lives only in CompletePayment.
// If the worker had its own copy, the two would drift apart, and one day the
// worker would book a seat the API would have refunded (or the reverse).
//
// Network calls here are made with NO transaction open: the worker claimed
// the row in a short transaction that is already committed.
package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ReconcileOutcome says what the reconciler did with one payment.
type ReconcileOutcome string

const (
	ReconcileBooked           ReconcileOutcome = "booked"            // charge succeeded, seat booked
	ReconcileRefundRequired   ReconcileOutcome = "refund_required"   // charge succeeded, hold dead: refund queued
	ReconcileAlreadyRefunding ReconcileOutcome = "already_refunding" // decided earlier
	ReconcileDeclined         ReconcileOutcome = "declined"          // gateway declined: no money taken
	ReconcileAbandoned        ReconcileOutcome = "abandoned"         // no charge; key voided: no money taken
	ReconcileStillPending     ReconcileOutcome = "still_pending"     // gateway unreachable: try again later
)

// ReconcilePayment finds out what really happened to a PENDING payment whose
// request was abandoned (e.g. a 504 nobody retried), and finishes it: booked,
// refunded (via the outbox), declined, or abandoned. Never stuck.
//
// THE VOID RACE. "The gateway has no charge for this key" does NOT prove
// that no charge will ever happen: a slow request (or a client retry with the
// old Idempotency-Key) might still be on its way to the gateway with this
// very key. If we just marked the payment ABANDONED, that late charge could
// succeed AFTERWARDS: money taken, and nobody would ever book or refund it.
// So we first CLOSE the key at the gateway (Void). The gateway serialises
// "void" and "charge" for one key, so exactly one of two things happens:
//   - the void wins: the key is voided, any later charge with it is refused
//     ("voided", no money taken), and ABANDONED is safe;
//   - the late charge won (it got there just before the void): Void answers
//     "succeeded", and we take the succeeded path like any other.
func (s *Service) ReconcilePayment(ctx context.Context, payment Payment) (ReconcileOutcome, error) {
	result, err := s.gateway.Lookup(ctx, payment.ChargeKey)
	if errors.Is(err, ErrChargeNotFound) {
		result, err = s.gateway.Void(ctx, payment.ChargeKey)
	}
	if err != nil {
		// Unreachable gateway or a 5xx: we still do not know. The payment
		// stays PENDING; record why and back off.
		if recErr := s.repo.RecordReconcileFailure(ctx, payment.ID, err.Error()); recErr != nil {
			return ReconcileStillPending, fmt.Errorf("%w (and recording it failed: %v)", err, recErr)
		}
		return ReconcileStillPending, err
	}

	switch result.Status {
	case ChargeSucceeded:
		done, err := s.completeCharged(ctx, payment, result.ChargeID)
		if err != nil {
			return ReconcileStillPending, err // nothing committed; the next run tries again
		}
		switch done.Kind {
		case Booked:
			return ReconcileBooked, nil
		case RefundRequired:
			// The outbox REFUND row (written in Tx B) is drained by the
			// outbox job; no inline refund here, nobody is waiting.
			return ReconcileRefundRequired, nil
		default:
			return ReconcileAlreadyRefunding, nil
		}
	case ChargeDeclined:
		return ReconcileDeclined, s.repo.MarkPaymentDeclined(ctx, payment.ID)
	case ChargeVoided:
		return ReconcileAbandoned, s.repo.MarkPaymentAbandoned(ctx, payment.ID)
	default:
		return ReconcileStillPending, fmt.Errorf("unexpected charge status %q", result.Status)
	}
}

// refundPayload is the payload of a REFUND outbox row (written by CompletePayment).
type refundPayload struct {
	PaymentID int64  `json:"payment_id"`
	ChargeID  string `json:"charge_id"`
	RefundKey string `json:"refund_key"`
}

// releaseLockPayload is the payload of a RELEASE_LOCK outbox row.
type releaseLockPayload struct {
	ShowID int64  `json:"show_id"`
	SeatID int64  `json:"seat_id"`
	Owner  string `json:"owner"`
}

// ProcessOutboxRow performs the side effect one outbox row asks for. It
// returns nil when the row is DONE, or an error when it should be retried
// (the worker then records the failure with backoff, or dead-letters it).
//
// Both side effects are idempotent, which is what makes retries (and a
// crashed worker repeating one) safe:
//   - REFUND uses the payment's fixed refund_key: the gateway pays it once.
//   - RELEASE_LOCK is owner-checked; deleting a lock that is already gone
//     (expired by TTL, or released inline) is a success.
func (s *Service) ProcessOutboxRow(ctx context.Context, row OutboxRow) error {
	switch row.Kind {
	case OutboxRefund:
		var p refundPayload
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			return fmt.Errorf("bad REFUND payload: %w", err)
		}
		if _, err := s.gateway.Refund(ctx, RefundRequest{IdempotencyKey: p.RefundKey, ChargeID: p.ChargeID}); err != nil {
			return err
		}
		// The same method the inline refund uses: payment REFUNDED + row DONE.
		_, err := s.repo.MarkRefunded(ctx, p.PaymentID, row.ID)
		return err

	case OutboxReleaseLock:
		var p releaseLockPayload
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			return fmt.Errorf("bad RELEASE_LOCK payload: %w", err)
		}

		// STALE RELEASE GUARD. The Redis lock's value is the USER, not the
		// hold. If this row is stale (written when an older hold expired or
		// was cancelled) and the same user has since held the same seat
		// again, the owner check alone would pass and we would delete the
		// user's CURRENT lock. Postgres knows which hold is current, so ask
		// it first: a live hold of the same owner means "this release is
		// stale", so skip it and mark the row DONE.
		//
		// A tiny window remains: the user could re-hold between this check
		// and the Release call below. That only costs the lock (Redis says
		// "free" while Postgres holds the seat for the user); anyone else is
		// still rejected by Postgres, the authority, so it is safe. A
		// hold-scoped lock token would close it (see docs/learning/phase-5.md).
		live, err := s.repo.HasLiveHold(ctx, p.ShowID, p.SeatID, p.Owner)
		if err != nil {
			return err
		}
		if live {
			return s.repo.MarkOutboxDone(ctx, row.ID)
		}

		// Not the best-effort releaseLock helper: here a Redis error must
		// come back as an error, so the row is retried later.
		if err := s.locker.Release(ctx, p.ShowID, p.SeatID, p.Owner); err != nil {
			return err
		}
		return s.repo.MarkOutboxDone(ctx, row.ID)

	default:
		return fmt.Errorf("unknown outbox kind %q", row.Kind)
	}
}
