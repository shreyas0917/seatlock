// service_holds_test.go unit-tests the hold -> confirm rules in
// service_holds.go, using the fakes from service_test.go (no Redis, no Postgres).
package booking

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHoldSeat_LockerError_FailsClosed is the unit-level proof of the
// fail-closed policy: when the lock cannot be asked, the answer is
// ErrLockUnavailable and the database is NEVER touched. No hold row can be
// created behind the lock's back.
func TestHoldSeat_LockerError_FailsClosed(t *testing.T) {
	svc, repo, locker := newTestService()
	locker.acquireErr = errors.New("dial tcp 127.0.0.1:6379: connection refused")

	_, _, err := svc.HoldSeat(context.Background(), HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1})

	assert.ErrorIs(t, err, ErrLockUnavailable, "any locker error must become ErrLockUnavailable")
	assert.Zero(t, repo.createHoldCalls, "the repository must never be called when the lock is unavailable")
}

func TestHoldSeat_NotAcquired_ErrSeatHeld(t *testing.T) {
	svc, repo, locker := newTestService()
	locker.heldBy = "bob"

	_, _, err := svc.HoldSeat(context.Background(), HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1})

	assert.ErrorIs(t, err, ErrSeatHeld)
	assert.Zero(t, repo.createHoldCalls, "a seat locked in Redis is rejected without a database round trip")
}

func TestHoldSeat_DatabaseRejects_ReleasesLock(t *testing.T) {
	svc, repo, locker := newTestService()
	// Redis gave us the lock, but Postgres says another user holds the seat
	// (e.g. Redis lost its data after a restart).
	repo.createHoldErr = ErrSeatHeld

	_, _, err := svc.HoldSeat(context.Background(), HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1})

	assert.ErrorIs(t, err, ErrSeatHeld)
	assert.Equal(t, 1, locker.releaseCalls, "the Redis lock we took must be given back")
}

func TestHoldSeat_ReleaseFailureIsIgnored(t *testing.T) {
	svc, repo, locker := newTestService()
	repo.createHoldErr = ErrSeatHeld
	locker.releaseErr = ErrLockUnavailable

	_, _, err := svc.HoldSeat(context.Background(), HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1})

	assert.ErrorIs(t, err, ErrSeatHeld, "the caller sees the real reason, not the release failure")
}

func TestHoldSeat_SuccessAndRetry(t *testing.T) {
	svc, _, locker := newTestService()
	ctx := context.Background()
	in := HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1}

	first, created, err := svc.HoldSeat(ctx, in)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, HoldActive, first.Status)
	assert.Zero(t, locker.releaseCalls, "a successful hold keeps its lock")

	again, created, err := svc.HoldSeat(ctx, in)
	require.NoError(t, err)
	assert.False(t, created, "a retry returns the existing hold")
	assert.Equal(t, first.ID, again.ID)
}

func TestHoldSeat_ValidationHappensBeforeLocking(t *testing.T) {
	tests := []struct {
		name    string
		input   HoldSeatInput
		wantErr error
	}{
		{"missing user", HoldSeatInput{UserID: " ", ShowID: 1, SeatID: 1}, ErrMissingUserID},
		{"bad show id", HoldSeatInput{UserID: "alice", ShowID: 0, SeatID: 1}, ErrInvalidInput},
		{"bad seat id", HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: -1}, ErrInvalidInput},
		{"unknown show", HoldSeatInput{UserID: "alice", ShowID: 99, SeatID: 1}, ErrShowNotFound},
		{"unknown seat", HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 99}, ErrSeatNotFound},
		{"seat of another show", HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 3}, ErrSeatNotInShow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, locker := newTestService()

			_, _, err := svc.HoldSeat(context.Background(), tc.input)

			assert.ErrorIs(t, err, tc.wantErr)
			assert.Zero(t, locker.acquireCalls, "bad requests must not take a lock")
		})
	}
}

func TestConfirmBooking_DoesNotNeedTheLock(t *testing.T) {
	svc, _, locker := newTestService()
	ctx := context.Background()
	hold, _, err := svc.HoldSeat(ctx, HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1})
	require.NoError(t, err)

	// Now Redis goes down completely.
	locker.acquireErr = ErrLockUnavailable
	locker.releaseErr = ErrLockUnavailable
	acquiresBefore := locker.acquireCalls

	b, err := svc.ConfirmBooking(ctx, ConfirmInput{UserID: "alice", HoldID: hold.ID})

	require.NoError(t, err, "an existing hold confirms even with Redis down")
	assert.Equal(t, hold.ID, b.HoldID)
	assert.Equal(t, acquiresBefore, locker.acquireCalls, "confirm never calls Acquire")
	assert.Equal(t, 1, locker.releaseCalls, "confirm tries a best-effort release afterwards")
}

func TestConfirmBooking_Validation(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()

	_, err := svc.ConfirmBooking(ctx, ConfirmInput{UserID: "", HoldID: 1})
	assert.ErrorIs(t, err, ErrMissingUserID)

	_, err = svc.ConfirmBooking(ctx, ConfirmInput{UserID: "alice", HoldID: 0})
	assert.ErrorIs(t, err, ErrInvalidInput)

	_, err = svc.ConfirmBooking(ctx, ConfirmInput{UserID: "alice", HoldID: 42})
	assert.ErrorIs(t, err, ErrHoldNotFound)
}

func TestCancelHold(t *testing.T) {
	svc, _, locker := newTestService()
	ctx := context.Background()
	hold, _, err := svc.HoldSeat(ctx, HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1})
	require.NoError(t, err)

	_, err = svc.CancelHold(ctx, "mallory", hold.ID)
	assert.ErrorIs(t, err, ErrHoldNotFound)
	assert.Zero(t, locker.releaseCalls, "a rejected cancel must not touch the lock")

	locker.releaseErr = ErrLockUnavailable // Redis down: the cancel must still succeed
	released, err := svc.CancelHold(ctx, "alice", hold.ID)
	require.NoError(t, err)
	assert.Equal(t, HoldReleased, released.Status)
	assert.Equal(t, 1, locker.releaseCalls)
}
