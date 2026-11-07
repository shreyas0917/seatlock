// observer.go lets the service report business OUTCOMES (for metrics)
// without knowing anything about Prometheus.
//
// Where it fits:
//
//	Service.HoldSeat / ConfirmBooking --outcome name--> Observer --> internal/metrics counters
//
// The service only names what happened ("created", "seat_held", ...); the
// metrics package decides how to record it. Tests use the no-op default or
// a fake. Outcome names come from the fixed lists below, never from user
// input, so the metric labels stay low-cardinality.
package booking

import (
	"errors"
)

// Observer receives one outcome per hold / confirm attempt.
type Observer interface {
	HoldOutcome(outcome string)
	ConfirmOutcome(outcome string)
}

// noopObserver is the default: record nothing.
type noopObserver struct{}

func (noopObserver) HoldOutcome(string)    {}
func (noopObserver) ConfirmOutcome(string) {}

// SetObserver installs the metrics observer. It must be called BEFORE the
// service starts handling requests (main wires it before ListenAndServe):
// the field is read by every request without locking.
func (s *Service) SetObserver(o Observer) {
	if o == nil {
		o = noopObserver{}
	}
	s.observer = o
}

// Hold outcomes (seatlock_holds_total{outcome}).
const (
	HoldCreated           = "created"             // 201: new hold
	HoldReused            = "reused"              // 200: the user's existing hold
	HoldSeatHeld          = "seat_held"           // 409: someone else holds it
	HoldAlreadyBooked     = "already_booked"      // 409: the seat is sold
	HoldLockUnavailable   = "lock_unavailable"    // 503: Redis down -> FAIL CLOSED
	HoldPaymentInProgress = "payment_in_progress" // 409: user re-holds while paying
	HoldInvalid           = "invalid"             // 400/404: bad request, unknown show/seat
	HoldError             = "error"               // anything else (e.g. database busy)
)

// HoldOutcomeOf maps a HoldSeat result to its outcome name.
func HoldOutcomeOf(created bool, err error) string {
	switch {
	case err == nil && created:
		return HoldCreated
	case err == nil:
		return HoldReused
	case errors.Is(err, ErrSeatHeld):
		return HoldSeatHeld
	case errors.Is(err, ErrSeatAlreadyBooked):
		return HoldAlreadyBooked
	case errors.Is(err, ErrLockUnavailable):
		return HoldLockUnavailable
	case errors.Is(err, ErrPaymentInProgress):
		return HoldPaymentInProgress
	case isInvalidRequest(err):
		return HoldInvalid
	default:
		return HoldError
	}
}

// Confirm outcomes (seatlock_confirms_total{outcome}).
const (
	ConfirmBooked               = "booked"                 // 201
	ConfirmDeclined             = "declined"               // 402
	ConfirmPending              = "pending"                // 504 PAYMENT_PENDING: outcome unknown
	ConfirmNotCompleted         = "not_completed"          // 504 PAYMENT_NOT_COMPLETED: key voided
	ConfirmExpiredRefunded      = "expired_refunded"       // 410, money taken and given back
	ConfirmExpiredRefundPending = "expired_refund_pending" // 410, refund queued in the outbox
	ConfirmExpired              = "expired"                // 410, no money was taken
	ConfirmAlreadyUsed          = "already_used"           // 409: hold used / seat booked
	ConfirmInvalid              = "invalid"                // 400/404
	ConfirmError                = "error"                  // anything else
)

// ConfirmOutcomeOf maps a ConfirmBooking result to its outcome name.
func ConfirmOutcomeOf(err error) string {
	var expired *HoldExpiredError
	switch {
	case err == nil:
		return ConfirmBooked
	case errors.Is(err, ErrPaymentDeclined):
		return ConfirmDeclined
	case errors.Is(err, ErrPaymentNotCompleted):
		return ConfirmNotCompleted
	case errors.Is(err, ErrPaymentPending):
		return ConfirmPending
	case errors.As(err, &expired) && expired.PaymentStatus == PaymentRefunded:
		return ConfirmExpiredRefunded
	case errors.As(err, &expired) && expired.PaymentStatus == PaymentRefundPending:
		return ConfirmExpiredRefundPending
	case errors.Is(err, ErrHoldExpired):
		return ConfirmExpired
	case errors.Is(err, ErrHoldAlreadyUsed), errors.Is(err, ErrSeatAlreadyBooked):
		return ConfirmAlreadyUsed
	case isInvalidRequest(err):
		return ConfirmInvalid
	default:
		return ConfirmError
	}
}

// isInvalidRequest: the client asked for something malformed or unknown.
func isInvalidRequest(err error) bool {
	for _, target := range []error{ErrInvalidInput, ErrMissingUserID, ErrShowNotFound, ErrSeatNotFound, ErrSeatNotInShow, ErrHoldNotFound} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
