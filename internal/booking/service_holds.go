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
		// We took the Redis lock, but no hold came back. Give the lock back
		// ONLY if Postgres DEFINITELY said no; otherwise keep it.
		if isDefiniteHoldRejection(err) {
			// Postgres answered and the answer was "no hold": the lock
			// guards nothing, so release it rather than block the seat for
			// 310s. A failed release is ignored: the TTL cleans up.
			s.releaseLock(ctx, in.ShowID, in.SeatID, in.UserID)
		}
		// Any other error (ErrBusy, a dropped connection, ...) means the
		// outcome is UNKNOWN: the COMMIT may have reached Postgres and
		// succeeded just before our timeout fired. If we released the lock
		// now, Redis would say "free" while Postgres holds the seat for this
		// user. Still safe (Postgres rejects others), but the lock would stop
		// doing its job. So we keep it and let its TTL expire it.
		return Hold{}, false, err
	}
	return hold, created, nil
}

// isDefiniteHoldRejection reports whether err is a business answer from
// Postgres that guarantees NO hold was created for this request.
func isDefiniteHoldRejection(err error) bool {
	return errors.Is(err, ErrSeatHeld) ||
		errors.Is(err, ErrSeatAlreadyBooked) ||
		errors.Is(err, ErrSeatNotInShow)
}

// ConfirmBooking turns the user's active hold into a booking.
//
// There is deliberately NO Redis call before the database work: the hold row
// in PostgreSQL is the proof of the reservation. So confirming still works
// while Redis is down: existing valid reservations may complete.
//
// The final guard against double booking is still the Phase 1 one: the
// database's UNIQUE(show_id, seat_id) constraint on bookings. We never write
// "if the seat is free { insert }" (check-then-insert) in Go:
//
//	time  user A                       user B
//	t1    SELECT -> seat 7 is free
//	t2                                 SELECT -> seat 7 is free
//	t3    INSERT booking (seat 7)
//	t4                                 INSERT booking (seat 7)   <- double booking!
//
// Between the check and the insert there is a gap, and another request can
// slip into it. A Go mutex would not help either: with 2+ API servers, each
// has its own mutex. The database is the one place every request goes
// through, and a UNIQUE constraint is checked atomically inside Postgres: of
// two concurrent inserts for the same seat, exactly one commits and the other
// gets error 23505, which becomes ErrSeatAlreadyBooked.
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
// RELEASED there, the seat is free for everyone. In the same transaction an
// outbox RELEASE_LOCK row is written, so the Redis lock release is never
// forgotten. The inline release below is best effort: if it works, the
// outbox row is marked DONE; if Redis is down, the cancel still succeeds,
// and the PENDING outbox row waits for the Phase 4 worker (until then the
// lock lingers until its TTL and new holds on that seat get 409).
func (s *Service) CancelHold(ctx context.Context, userID string, holdID int64) (Hold, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return Hold{}, ErrMissingUserID
	}
	if holdID <= 0 {
		return Hold{}, fmt.Errorf("%w: hold_id must be a positive number", ErrInvalidInput)
	}

	hold, outboxID, err := s.repo.ReleaseHold(ctx, userID, holdID)
	if err != nil {
		return Hold{}, err
	}

	// outboxID is 0 when the hold was already released earlier (a retry).
	if outboxID != 0 && s.releaseLock(ctx, hold.ShowID, hold.SeatID, userID) {
		// Phase 4: the worker drains RELEASE_LOCK rows that are still PENDING.
		s.markOutboxDone(ctx, outboxID)
	}
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
// depend on this call, only on PostgreSQL. It reports whether it worked.
func (s *Service) releaseLock(ctx context.Context, showID, seatID int64, owner string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	if err := s.locker.Release(ctx, showID, seatID, owner); err != nil {
		slog.Warn("best-effort lock release failed; the lock will expire by its TTL",
			"show_id", showID, "seat_id", seatID, "error", err.Error())
		return false
	}
	return true
}

// markOutboxDone marks an outbox row as handled, best effort: if it fails,
// the row stays PENDING and the Phase 4 worker simply repeats the (harmless,
// idempotent) side effect.
func (s *Service) markOutboxDone(ctx context.Context, outboxID int64) {
	if err := s.repo.MarkOutboxDone(context.WithoutCancel(ctx), outboxID); err != nil {
		slog.Warn("could not mark outbox row done; the worker will repeat it",
			"outbox_id", outboxID, "error", err.Error())
	}
}
