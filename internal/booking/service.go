// service.go holds the business rules for shows, seats and bookings.
// The hold -> confirm flow lives in service_holds.go.
//
// Where it fits: HTTP handlers (internal/httpapi) call the Service; the
// Service validates input, applies the rules and calls the Repository for
// data. The Service knows nothing about HTTP (no status codes, no JSON) and
// nothing about SQL (no queries). That keeps each layer small and testable.
package booking

import (
	"context"
	"fmt"
	"time"
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
//   - CreateHold returns ErrSeatHeld / ErrSeatAlreadyBooked when the seat is
//     not free, and created=false with the existing hold for a same-user retry;
//   - BeginPayment and ReleaseHold return ErrHoldNotFound, ErrHoldAlreadyUsed
//     or ErrHoldExpired when the hold cannot be used; ReleaseHold returns
//     ErrPaymentInProgress while the hold has a PENDING payment.
type Repository interface {
	ListShows(ctx context.Context) ([]Show, error)
	GetShow(ctx context.Context, showID int64) (Show, error)
	GetSeat(ctx context.Context, seatID int64) (Seat, error)
	ListSeatsWithStatus(ctx context.Context, showID int64) ([]SeatWithStatus, error)
	GetBooking(ctx context.Context, bookingID int64) (Booking, error)
	CreateHold(ctx context.Context, in HoldSeatInput, ttl time.Duration) (hold Hold, created bool, err error)
	ReleaseHold(ctx context.Context, userID string, holdID int64) (hold Hold, outboxID int64, err error)
	MarkOutboxDone(ctx context.Context, outboxID int64) error
	HasLiveHold(ctx context.Context, showID, seatID int64, owner string) (bool, error)

	// Payments (repository_payments.go).
	BeginPayment(ctx context.Context, in ConfirmInput) (payment Payment, created bool, err error)
	CompletePayment(ctx context.Context, paymentID int64, chargeID string) (CompleteResult, error)
	MarkPaymentDeclined(ctx context.Context, paymentID int64) error
	MarkPaymentAbandoned(ctx context.Context, paymentID int64) error
	RecordReconcileFailure(ctx context.Context, paymentID int64, failure string) error
	MarkRefunded(ctx context.Context, paymentID, outboxID int64) (Payment, error)
	RecordOutboxFailure(ctx context.Context, outboxID int64, failure string) error
}

// Service applies SeatLock's business rules.
type Service struct {
	repo    Repository
	locker  SeatLocker
	gateway PaymentGateway
	holdTTL time.Duration
	// observer receives hold/confirm outcomes for metrics (no-op by
	// default; see SetObserver in observer.go).
	observer Observer
}

// NewService builds a Service that stores data through repo, locks seats
// through locker (Redis in production), takes payments through gateway and
// creates holds lasting holdTTL.
func NewService(repo Repository, locker SeatLocker, gateway PaymentGateway, holdTTL time.Duration) *Service {
	return &Service{repo: repo, locker: locker, gateway: gateway, holdTTL: holdTTL, observer: noopObserver{}}
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

// GetBooking returns one booking by ID.
func (s *Service) GetBooking(ctx context.Context, bookingID int64) (Booking, error) {
	if bookingID <= 0 {
		return Booking{}, fmt.Errorf("%w: booking_id must be a positive number", ErrInvalidInput)
	}
	return s.repo.GetBooking(ctx, bookingID)
}
