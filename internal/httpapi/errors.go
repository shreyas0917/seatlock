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
	default:
		return httpError{http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error"}
	}
}

// writeError sends the JSON error response for err, and logs unexpected
// (500) errors together with the request ID so they can be found in the logs.
func (h *handlers) writeError(w http.ResponseWriter, r *http.Request, err error) {
	he := mapError(err)

	if he.status == http.StatusInternalServerError {
		h.logger.Error("request failed",
			"request_id", middleware.GetReqID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"error", err.Error(),
		)
	}

	writeJSON(w, he.status, ErrorResponse{Error: ErrorBody{Code: he.code, Message: he.message}})
}
