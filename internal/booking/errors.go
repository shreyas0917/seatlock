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

import "errors"

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

	// ErrSeatAlreadyBooked: someone else already booked this seat. Produced
	// when PostgreSQL rejects the insert with a unique-constraint violation
	// (error code 23505) on bookings_show_seat_unique.
	ErrSeatAlreadyBooked = errors.New("seat already booked")
)
