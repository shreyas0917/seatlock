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

	// ErrBusy: the database did not answer in time. Either a query was slow, or
	// every pooled connection was in use and we timed out waiting for one.
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

	// ErrSeatAlreadyBooked: someone else already booked this seat. Produced
	// when PostgreSQL rejects the insert with a unique-constraint violation
	// (error code 23505) on bookings_show_seat_unique.
	ErrSeatAlreadyBooked = errors.New("seat already booked")
)
