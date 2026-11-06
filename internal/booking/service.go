// service.go holds the business rules for shows, seats and bookings.
//
// Where it fits: HTTP handlers (internal/httpapi) call the Service; the
// Service validates input, applies the rules and calls the Repository for
// data. The Service knows nothing about HTTP (no status codes, no JSON) and
// nothing about SQL (no queries). That keeps each layer small and testable.
package booking

import (
	"context"
	"fmt"
	"strings"
)

// Repository is everything the Service needs from storage.
//
// About interfaces: an interface lists method signatures only. Any type that
// has those methods automatically satisfies it (no "implements" keyword).
// The Service depends on this interface, not on Postgres directly, so:
//   - unit tests can pass a fake in-memory repository (service_test.go);
//   - Phase 2 can add Redis holds next to Postgres without rewriting the Service.
//
// Error contract (implementations must follow it):
//   - GetShow returns ErrShowNotFound, GetSeat ErrSeatNotFound,
//     GetBooking ErrBookingNotFound when the row does not exist;
//   - CreateBooking returns ErrSeatAlreadyBooked when the seat is taken.
type Repository interface {
	ListShows(ctx context.Context) ([]Show, error)
	GetShow(ctx context.Context, showID int64) (Show, error)
	GetSeat(ctx context.Context, seatID int64) (Seat, error)
	ListSeatsWithStatus(ctx context.Context, showID int64) ([]SeatWithStatus, error)
	CreateBooking(ctx context.Context, in BookSeatInput) (Booking, error)
	GetBooking(ctx context.Context, bookingID int64) (Booking, error)
}

// Service applies SeatLock's business rules.
type Service struct {
	repo Repository
}

// NewService builds a Service that stores data through repo.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// ListShows returns every show.
func (s *Service) ListShows(ctx context.Context) ([]Show, error) {
	return s.repo.ListShows(ctx)
}

// ListSeats returns the seat map of a show: every seat and whether it is booked.
func (s *Service) ListSeats(ctx context.Context, showID int64) ([]SeatWithStatus, error) {
	if showID <= 0 {
		return nil, fmt.Errorf("%w: show_id must be a positive number", ErrInvalidInput)
	}
	// Check the show exists first, so an unknown show is a clear 404
	// instead of an empty seat list.
	if _, err := s.repo.GetShow(ctx, showID); err != nil {
		return nil, err
	}
	return s.repo.ListSeatsWithStatus(ctx, showID)
}

// BookSeat books one seat for one user.
//
// The validation below gives clients clear errors (400/404), but it is NOT what
// prevents double booking. Double booking is prevented by the database's
// UNIQUE(show_id, seat_id) constraint, enforced inside CreateBooking.
//
// Why we do NOT write "if seat is free { insert }" here (check-then-insert):
//
//	time  user A                       user B
//	t1    SELECT -> seat 7 is free
//	t2                                 SELECT -> seat 7 is free
//	t3    INSERT booking (seat 7)
//	t4                                 INSERT booking (seat 7)   <- double booking!
//
// Between the check and the insert there is a gap, and another request can
// slip into it. Adding a mutex in Go would not help either: with 2+ API
// servers, each has its own mutex. The database is the one place every
// request goes through, and a UNIQUE constraint is checked atomically inside
// Postgres: of two concurrent inserts for the same seat, exactly one commits
// and the other gets error 23505, which we turn into ErrSeatAlreadyBooked.
func (s *Service) BookSeat(ctx context.Context, in BookSeatInput) (Booking, error) {
	in.UserID = strings.TrimSpace(in.UserID)
	if in.UserID == "" {
		return Booking{}, ErrMissingUserID
	}
	if in.ShowID <= 0 {
		return Booking{}, fmt.Errorf("%w: show_id must be a positive number", ErrInvalidInput)
	}
	if in.SeatID <= 0 {
		return Booking{}, fmt.Errorf("%w: seat_id must be a positive number", ErrInvalidInput)
	}

	if _, err := s.repo.GetShow(ctx, in.ShowID); err != nil {
		return Booking{}, err
	}
	seat, err := s.repo.GetSeat(ctx, in.SeatID)
	if err != nil {
		return Booking{}, err
	}
	if seat.ShowID != in.ShowID {
		return Booking{}, ErrSeatNotInShow
	}

	// The real safety check happens here, inside the database.
	return s.repo.CreateBooking(ctx, in)
}

// GetBooking returns one booking by ID.
func (s *Service) GetBooking(ctx context.Context, bookingID int64) (Booking, error) {
	if bookingID <= 0 {
		return Booking{}, fmt.Errorf("%w: booking_id must be a positive number", ErrInvalidInput)
	}
	return s.repo.GetBooking(ctx, bookingID)
}
