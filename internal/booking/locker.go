// locker.go defines the SeatLocker interface: the fast, temporary seat lock
// that sits in front of PostgreSQL when a user holds a seat.
//
// Where it fits:
//
//	POST /v1/holds -> handler -> Service.HoldSeat -> SeatLocker.Acquire (Redis)
//	                                              -> Repository.CreateHold (Postgres, the authority)
//
// The real implementation is internal/lock.RedisLocker. The interface lives
// HERE, in the package that uses it, not in internal/lock: the service says
// "this is what I need", and anything that provides it can be plugged in.
// That is what lets service_test.go pass a fake locker that can pretend Redis
// is down, without starting Redis at all.
package booking

import "context"

// SeatLocker is a short-lived, per-seat lock.
//
// Contract (implementations MUST follow it, because the service relies on it
// to fail closed):
//   - Acquire returns (true, nil) if owner now holds the lock (including when
//     owner already held it), (false, nil) if someone else holds it, and
//     (false, ErrLockUnavailable) if the lock store cannot be asked. An error
//     must NEVER be reported as "acquired".
//   - Release deletes the lock only if owner holds it.
type SeatLocker interface {
	Acquire(ctx context.Context, showID, seatID int64, owner string) (acquired bool, err error)
	Release(ctx context.Context, showID, seatID int64, owner string) error
}
