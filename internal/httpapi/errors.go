// errors.go is the ONE place that turns domain errors into HTTP responses.
//
// Where it fits: when the service returns an error, a handler calls
// h.writeError(w, r, err). This file decides the status code and the error
// code, so handlers never guess status codes, and every error response has
// the same shape:
//
//	{ "error": { "code": "SEAT_ALREADY_BOOKED", "message": "..." } }
//
// Unknown errors (for example a database outage) become 500 INTERNAL_ERROR
// with a generic message. The real error is logged, never sent to the
// client, because raw database errors can leak table names, SQL or hostnames.
package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/shreyas0917/seatlock/internal/booking"
)

// httpError is the status code and public error details for one error.
type httpError struct {
	status  int
	code    string
	message string
}

// mapError converts an error from the booking package into an httpError.
//
// errors.Is checks the whole chain of wrapped errors, so a wrapped error like
// "invalid input: show_id must be a positive number" still matches
// booking.ErrInvalidInput.
func mapError(err error) httpError {
	switch {
	case errors.Is(err, booking.ErrInvalidInput):
		// err.Error() is safe to show here: we wrote these messages ourselves.
		return httpError{http.StatusBadRequest, "INVALID_INPUT", err.Error()}
	case errors.Is(err, booking.ErrMissingUserID):
		return httpError{http.StatusBadRequest, "MISSING_USER_ID", "the X-User-ID header is required"}
	case errors.Is(err, booking.ErrSeatNotInShow):
		return httpError{http.StatusBadRequest, "SEAT_NOT_IN_SHOW", "the seat does not belong to this show"}
	case errors.Is(err, booking.ErrShowNotFound):
		return httpError{http.StatusNotFound, "SHOW_NOT_FOUND", "show not found"}
	case errors.Is(err, booking.ErrSeatNotFound):
		return httpError{http.StatusNotFound, "SEAT_NOT_FOUND", "seat not found"}
	case errors.Is(err, booking.ErrBookingNotFound):
		return httpError{http.StatusNotFound, "BOOKING_NOT_FOUND", "booking not found"}
	case errors.Is(err, booking.ErrSeatAlreadyBooked):
		return httpError{http.StatusConflict, "SEAT_ALREADY_BOOKED", "this seat has already been booked"}
	case errors.Is(err, booking.ErrSeatHeld):
		return httpError{http.StatusConflict, "SEAT_HELD", "this seat is currently held by another user"}
	case errors.Is(err, booking.ErrHoldNotFound):
		return httpError{http.StatusNotFound, "HOLD_NOT_FOUND", "hold not found"}
	case errors.Is(err, booking.ErrHoldAlreadyUsed):
		return httpError{http.StatusConflict, "HOLD_ALREADY_USED", "this hold has already been used for a booking"}
	case errors.Is(err, booking.ErrHoldExpired):
		// 410 Gone: the hold existed, but it is gone for good. Retrying the
		// same request will never work; the user must hold the seat again.
		return httpError{http.StatusGone, "HOLD_EXPIRED", "this hold has expired or was cancelled"}
	case errors.Is(err, booking.ErrPaymentInProgress):
		return httpError{http.StatusConflict, "PAYMENT_IN_PROGRESS", "a payment for this hold is in progress; it cannot be cancelled now"}
	case errors.Is(err, booking.ErrPaymentDeclined):
		// 402 Payment Required: the card was declined, nothing was charged,
		// and the hold is still valid. Retry with a NEW Idempotency-Key.
		return httpError{http.StatusPaymentRequired, "PAYMENT_DECLINED", "the payment was declined; no money was taken"}
	case errors.Is(err, booking.ErrPaymentPending):
		// Checked BEFORE ErrBusy: a Tx B timeout wraps both, and the honest
		// answer is "payment outcome unknown". 504 Gateway Timeout: retry
		// with the SAME Idempotency-Key to learn the outcome; you will not
		// be charged twice.
		return httpError{http.StatusGatewayTimeout, "PAYMENT_PENDING", "the payment outcome is not known yet; retry with the same Idempotency-Key"}
	case errors.Is(err, booking.ErrLockUnavailable):
		// Fail closed: we could not ask the seat lock, so no new hold was
		// created. 503 because it is temporary and the client may retry.
		return httpError{http.StatusServiceUnavailable, "LOCK_UNAVAILABLE", "seat holds are temporarily unavailable, please retry shortly"}
	case errors.Is(err, booking.ErrBusy):
		// Why 503 and not 500: 500 says "the server is broken", so clients and
		// on-call engineers treat it as a bug. A timeout under load is not a
		// bug: the server is healthy but temporarily overloaded, and the same
		// request will probably succeed in a moment. 503 Service Unavailable
		// (with Retry-After) says exactly that, so clients can back off and
		// retry instead of giving up, and alerts can tell "overload" apart
		// from "broken code".
		return httpError{http.StatusServiceUnavailable, "SERVICE_BUSY", "the service is busy, please retry shortly"}
	default:
		return httpError{http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error"}
	}
}

// writeError sends the JSON error response for err, and logs 500 and 503
// errors (with the full error text and the request ID) so they can be found
// in the logs.
func (h *handlers) writeError(w http.ResponseWriter, r *http.Request, err error) {
	he := mapError(err)

	switch he.status {
	case http.StatusInternalServerError:
		h.logger.Error("request failed",
			"request_id", middleware.GetReqID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"error", err.Error(),
		)
	case http.StatusServiceUnavailable:
		// Not a bug, but we still log the full error so overload is visible.
		h.logger.Warn("service unavailable",
			"request_id", middleware.GetReqID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"error", err.Error(),
		)
		// Tells well-behaved clients how many seconds to wait before retrying.
		w.Header().Set("Retry-After", "1")
	}

	writeJSON(w, he.status, ErrorResponse{Error: ErrorBody{Code: he.code, Message: he.message}})
}
