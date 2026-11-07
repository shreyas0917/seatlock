// errors.go defines every error the booking domain can return on purpose.
//
// Where it fits: the repository and the service return these errors; ONE
// function in internal/httpapi/errors.go turns each of them into an HTTP
// status code and error code. Handlers never decide status codes themselves.
//
// These are "sentinel errors": package-level values created once with
// errors.New. Other code checks for them with errors.Is(err, ErrX), which also
// works when the error has been wrapped with fmt.Errorf("...: %w", ErrX).
package booking

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalidInput: the request is malformed (e.g. a non-positive ID).
	// Usually wrapped with details: fmt.Errorf("%w: show_id must be positive", ErrInvalidInput).
	ErrInvalidInput = errors.New("invalid input")

	// ErrMissingUserID: a write request did not say who the user is (X-User-ID header).
	ErrMissingUserID = errors.New("missing user id")

	// ErrShowNotFound: no show with that ID exists.
	ErrShowNotFound = errors.New("show not found")

	// ErrSeatNotFound: no seat with that ID exists.
	ErrSeatNotFound = errors.New("seat not found")

	// ErrSeatNotInShow: the seat exists but belongs to a different show.
	ErrSeatNotInShow = errors.New("seat does not belong to this show")

	// ErrBookingNotFound: no booking with that ID exists.
	ErrBookingNotFound = errors.New("booking not found")

	// ErrBusy: the database could not serve the request right now. A query was
	// slow, every pooled connection was in use and we timed out waiting for
	// one, or Postgres was briefly unavailable (restart, dropped connection):
	// see IsDBUnavailable.
	// Mapped to 503 SERVICE_BUSY (see internal/httpapi/errors.go for why).
	ErrBusy = errors.New("service busy")

	// ErrLockUnavailable: Redis (the seat lock) could not be reached or did not
	// answer in time. This is NOT "the seat is free": when we cannot ask the
	// lock, we refuse the hold (fail closed). Mapped to 503 LOCK_UNAVAILABLE.
	ErrLockUnavailable = errors.New("seat lock unavailable")

	// ErrSeatHeld: another user holds this seat right now (Redis said so, or
	// PostgreSQL has an ACTIVE hold for it). Mapped to 409 SEAT_HELD.
	ErrSeatHeld = errors.New("seat is held by another user")

	// ErrHoldNotFound: no such hold, OR it belongs to another user (we do not
	// reveal which). Mapped to 404 HOLD_NOT_FOUND.
	ErrHoldNotFound = errors.New("hold not found")

	// ErrHoldAlreadyUsed: the hold was already confirmed into a booking.
	// Mapped to 409 HOLD_ALREADY_USED.
	ErrHoldAlreadyUsed = errors.New("hold already used")

	// ErrHoldExpired: the hold ran out of time or was cancelled; it can no
	// longer be confirmed. Mapped to 410 HOLD_EXPIRED.
	ErrHoldExpired = errors.New("hold expired")

	// ErrPaymentDeclined: the gateway declined the charge. No money was
	// taken and the hold stays ACTIVE, so the user may retry with a new
	// Idempotency-Key. Mapped to 402 PAYMENT_DECLINED.
	ErrPaymentDeclined = errors.New("payment declined")

	// ErrPaymentPending: the charge was sent but its outcome is UNKNOWN
	// (timeout, lost answer). The payment stays PENDING; retrying with the
	// same Idempotency-Key finds out what happened. Mapped to 504 PAYMENT_PENDING.
	ErrPaymentPending = errors.New("payment outcome unknown")

	// ErrPaymentNotCompleted: the charge was REFUSED because its key had been
	// voided by the reconciler (the old attempt was abandoned). We KNOW no
	// money was taken; the payment is now ABANDONED, and a retry starts a new
	// payment. Mapped to 504 PAYMENT_NOT_COMPLETED (not stored, so a retry
	// with the same Idempotency-Key runs again).
	ErrPaymentNotCompleted = errors.New("payment not completed; no money was taken")

	// ErrPaymentInProgress: the hold has a payment in flight, so it cannot
	// be cancelled right now. Mapped to 409 PAYMENT_IN_PROGRESS.
	ErrPaymentInProgress = errors.New("payment in progress for this hold")

	// ErrSeatAlreadyBooked: someone else already booked this seat. Produced
	// when PostgreSQL rejects the insert with a unique-constraint violation
	// (error code 23505) on bookings_show_seat_unique.
	ErrSeatAlreadyBooked = errors.New("seat already booked")
)

// HoldExpiredError is ErrHoldExpired plus, when money had already been taken
// for the hold, the state of its refund. errors.Is(err, ErrHoldExpired) is
// true for it, so the error mapping treats it like any expired hold (410),
// and the handler can add "payment": "REFUNDED" / "REFUND_PENDING".
type HoldExpiredError struct {
	PaymentStatus PaymentStatus // "" when no money was taken
}

func (e *HoldExpiredError) Error() string {
	if e.PaymentStatus == "" {
		return ErrHoldExpired.Error()
	}
	return fmt.Sprintf("%s (payment %s)", ErrHoldExpired, e.PaymentStatus)
}

// Is makes errors.Is(err, ErrHoldExpired) match this error type.
func (e *HoldExpiredError) Is(target error) bool { return target == ErrHoldExpired }
