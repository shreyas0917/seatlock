// service_holds.go holds the business rules for the hold -> confirm flow.
//
//	POST   /v1/holds          -> HoldSeat       (Redis lock, then Postgres)
//	POST   /v1/bookings       -> ConfirmBooking (Postgres only, no Redis)
//	DELETE /v1/holds/{holdID} -> CancelHold     (Postgres, then best-effort Redis)
//
// The policy, enforced here: SeatLock FAILS CLOSED. If the seat lock (Redis)
// cannot be asked, NO new hold is created: there is no fallback that skips
// the lock. But a hold that already exists in PostgreSQL can still be
// confirmed, because confirming only needs the authoritative database.
package booking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// releaseTimeout bounds the best-effort Redis release after a request.
const releaseTimeout = 500 * time.Millisecond

// HoldSeat reserves a seat for the user for holdTTL.
//
// It returns created=true for a new hold, or created=false when the user
// already held this seat (a retry), with that same hold.
func (s *Service) HoldSeat(ctx context.Context, in HoldSeatInput) (Hold, bool, error) {
	in.UserID = strings.TrimSpace(in.UserID)
	if err := validateSeatRequest(in.UserID, in.ShowID, in.SeatID); err != nil {
		return Hold{}, false, err
	}

	// Clear 404 / 400 answers before touching the lock.
	if _, err := s.repo.GetShow(ctx, in.ShowID); err != nil {
		return Hold{}, false, err
	}
	seat, err := s.repo.GetSeat(ctx, in.SeatID)
	if err != nil {
		return Hold{}, false, err
	}
	if seat.ShowID != in.ShowID {
		return Hold{}, false, ErrSeatNotInShow
	}

	// 1. Fast filter: the Redis lock.
	acquired, err := s.locker.Acquire(ctx, in.ShowID, in.SeatID, in.UserID)
	if err != nil {
		// FAIL CLOSED. We could not ask the lock, so we do NOT know whether
		// someone else is holding this seat. We stop here: the repository is
		// never called, and the client gets 503 LOCK_UNAVAILABLE.
		return Hold{}, false, asLockUnavailable(err)
	}
	if !acquired {
		return Hold{}, false, ErrSeatHeld
	}

	// 2. The authority: PostgreSQL decides. Redis saying "yes" is not enough
	//    (it may have lost its data); CreateHold re-checks everything.
	hold, created, err := s.repo.CreateHold(ctx, in, s.holdTTL)
	if err != nil {
		// We took the Redis lock but Postgres rejected the hold (or failed).
		// Give the lock back so it does not block the seat for 310s for
		// nothing. If the release itself fails we ignore it: the lock
		// expires by its TTL anyway, and Postgres stays correct either way.
		s.releaseLock(ctx, in.ShowID, in.SeatID, in.UserID)
		return Hold{}, false, err
	}
	return hold, created, nil
}

// ConfirmBooking turns the user's active hold into a booking.
//
// There is deliberately NO Redis call before the database work: the hold row
// in PostgreSQL is the proof of the reservation. So confirming still works
// while Redis is down: existing valid reservations may complete.
func (s *Service) ConfirmBooking(ctx context.Context, in ConfirmInput) (Booking, error) {
	in.UserID = strings.TrimSpace(in.UserID)
	if in.UserID == "" {
		return Booking{}, ErrMissingUserID
	}
	if in.HoldID <= 0 {
		return Booking{}, fmt.Errorf("%w: hold_id must be a positive number", ErrInvalidInput)
	}

	b, err := s.repo.ConfirmHold(ctx, in)
	if err != nil {
		return Booking{}, err
	}

	// The seat is booked now, so its Redis lock is no longer needed. Best
	// effort: a leftover lock on a BOOKED seat is harmless (Postgres
	// rejects any new hold on a booked seat) and expires by its TTL.
	s.releaseLock(ctx, b.ShowID, b.SeatID, b.UserID)
	return b, nil
}

// CancelHold releases the user's active hold.
//
// PostgreSQL is updated first, and that is what counts: once the hold is
// RELEASED there, the seat is free for everyone. The Redis release is best
// effort. If Redis is down, the cancel still succeeds; the lock just lingers
// until its TTL, and new holds on that seat get 409 until then.
func (s *Service) CancelHold(ctx context.Context, userID string, holdID int64) (Hold, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return Hold{}, ErrMissingUserID
	}
	if holdID <= 0 {
		return Hold{}, fmt.Errorf("%w: hold_id must be a positive number", ErrInvalidInput)
	}

	hold, err := s.repo.ReleaseHold(ctx, userID, holdID)
	if err != nil {
		return Hold{}, err
	}

	// Phase 4: record this release in the transactional outbox, so a worker
	// reliably deletes the Redis lock even if Redis is down right now.
	s.releaseLock(ctx, hold.ShowID, hold.SeatID, userID)
	return hold, nil
}

// validateSeatRequest checks the fields every seat request needs.
func validateSeatRequest(userID string, showID, seatID int64) error {
	if userID == "" {
		return ErrMissingUserID
	}
	if showID <= 0 {
		return fmt.Errorf("%w: show_id must be a positive number", ErrInvalidInput)
	}
	if seatID <= 0 {
		return fmt.Errorf("%w: seat_id must be a positive number", ErrInvalidInput)
	}
	return nil
}

// asLockUnavailable guarantees the error matches ErrLockUnavailable, even if a
// SeatLocker implementation forgot to wrap it. Any locker error means "no
// answer", and no answer must always end as 503, never as success.
func asLockUnavailable(err error) error {
	if errors.Is(err, ErrLockUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrLockUnavailable, err)
}

// releaseLock gives the Redis lock back, best effort.
//
// context.WithoutCancel keeps the request's values but drops its
// cancellation: if the client hangs up right after the database commit, we
// still try to release. The short timeout keeps a slow Redis from delaying
// the response. Errors are logged, never returned: correctness does not
// depend on this call, only on PostgreSQL.
func (s *Service) releaseLock(ctx context.Context, showID, seatID int64, owner string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	if err := s.locker.Release(ctx, showID, seatID, owner); err != nil {
		slog.Warn("best-effort lock release failed; the lock will expire by its TTL",
			"show_id", showID, "seat_id", seatID, "error", err.Error())
	}
}
