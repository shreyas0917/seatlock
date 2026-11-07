//go:build integration

// repository_holds_test.go tests the hold / confirm / release SQL against the
// real PostgreSQL (seatlock_test database). It reuses setupTestDB from
// repository_postgres_test.go: one show with seats A1, A2, A3.
package booking

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testHoldTTL = 300 * time.Second

// expireHold moves a hold's expires_at into the past, as if 5 minutes had passed.
func expireHold(t *testing.T, f testFixture, holdID int64) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(),
		`UPDATE holds SET expires_at = NOW() - interval '1 second' WHERE id = $1`, holdID)
	require.NoError(t, err)
}

// holdStatus reads a hold's status straight from the table.
func holdStatus(t *testing.T, f testFixture, holdID int64) HoldStatus {
	t.Helper()
	var status string
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT status FROM holds WHERE id = $1`, holdID).Scan(&status))
	return HoldStatus(status)
}

// countHolds counts the hold rows for a seat with the given status.
func countHolds(t *testing.T, f testFixture, seatID int64, status HoldStatus) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM holds WHERE seat_id = $1 AND status = $2`, seatID, string(status)).Scan(&n))
	return n
}

func TestCreateHold_NewHold(t *testing.T) {
	f := setupTestDB(t)

	h, created, err := f.repo.CreateHold(context.Background(),
		HoldSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)

	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, HoldActive, h.Status)
	assert.Equal(t, "alice", h.UserID)
	assert.InDelta(t, 300, h.SecondsLeft, 2, "seconds_left comes from the database clock")
}

func TestCreateHold_SameUserGetsSameHold(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	in := HoldSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[0]}
	first, _, err := f.repo.CreateHold(ctx, in, testHoldTTL)
	require.NoError(t, err)

	second, created, err := f.repo.CreateHold(ctx, in, testHoldTTL)

	require.NoError(t, err)
	assert.False(t, created, "a retry must not create a new hold")
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, 1, countHolds(t, f, f.seatIDs[0], HoldActive))
}

func TestCreateHold_OtherUserRejected(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	_, _, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)
	require.NoError(t, err)

	_, _, err = f.repo.CreateHold(ctx, HoldSeatInput{UserID: "bob", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)

	assert.ErrorIs(t, err, ErrSeatHeld)
}

func TestCreateHold_ExpiredHoldIsReplaced(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	old, _, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)
	require.NoError(t, err)
	expireHold(t, f, old.ID)

	fresh, created, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "bob", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)

	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, "bob", fresh.UserID)
	assert.Equal(t, HoldExpired, holdStatus(t, f, old.ID), "the old hold is marked EXPIRED lazily")
}

func TestCreateHold_BookedSeatRejected(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	h, _, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)
	require.NoError(t, err)
	_, err = payAndBook(ctx, f, ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)

	_, _, err = f.repo.CreateHold(ctx, HoldSeatInput{UserID: "bob", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)

	assert.ErrorIs(t, err, ErrSeatAlreadyBooked)
}

func TestCreateHold_SeatFromAnotherShow(t *testing.T) {
	f := setupTestDB(t)

	_, _, err := f.repo.CreateHold(context.Background(),
		HoldSeatInput{UserID: "alice", ShowID: f.showID + 1000, SeatID: f.seatIDs[0]}, testHoldTTL)

	assert.ErrorIs(t, err, ErrSeatNotInShow)
}

func TestPayAndBook_Success(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	h, _, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)
	require.NoError(t, err)

	b, err := payAndBook(ctx, f, ConfirmInput{UserID: "alice", HoldID: h.ID})

	require.NoError(t, err)
	assert.Equal(t, h.ID, b.HoldID, "the booking records which hold it came from")
	assert.Equal(t, f.seatIDs[0], b.SeatID)
	assert.Equal(t, HoldConsumed, holdStatus(t, f, h.ID))
}

func TestPayAndBook_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, f testFixture, holdID int64)
		userID  string
		holdID  func(real int64) int64
		wantErr error
	}{
		{"another user's hold", nil, "mallory", func(id int64) int64 { return id }, ErrHoldNotFound},
		{"fake hold id", nil, "alice", func(id int64) int64 { return id + 9999 }, ErrHoldNotFound},
		{"expired hold", func(t *testing.T, f testFixture, id int64) { expireHold(t, f, id) },
			"alice", func(id int64) int64 { return id }, ErrHoldExpired},
		{"released hold", func(t *testing.T, f testFixture, id int64) {
			_, _, err := f.repo.ReleaseHold(context.Background(), "alice", id)
			require.NoError(t, err)
		}, "alice", func(id int64) int64 { return id }, ErrHoldExpired},
		{"already consumed hold", func(t *testing.T, f testFixture, id int64) {
			_, err := payAndBook(context.Background(), f, ConfirmInput{UserID: "alice", HoldID: id})
			require.NoError(t, err)
		}, "alice", func(id int64) int64 { return id }, ErrHoldAlreadyUsed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := setupTestDB(t)
			ctx := context.Background()
			h, _, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)
			require.NoError(t, err)
			if tc.prepare != nil {
				tc.prepare(t, f, h.ID)
			}

			_, err = payAndBook(ctx, f, ConfirmInput{UserID: tc.userID, HoldID: tc.holdID(h.ID)})

			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestReleaseHold(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	h, _, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)
	require.NoError(t, err)

	_, _, err = f.repo.ReleaseHold(ctx, "mallory", h.ID)
	assert.ErrorIs(t, err, ErrHoldNotFound, "another user cannot cancel the hold")

	released, outboxID, err := f.repo.ReleaseHold(ctx, "alice", h.ID)
	require.NoError(t, err)
	assert.Equal(t, HoldReleased, released.Status)
	assert.NotZero(t, outboxID, "a RELEASE_LOCK outbox row is written with the release")

	again, againOutbox, err := f.repo.ReleaseHold(ctx, "alice", h.ID)
	require.NoError(t, err, "cancelling twice is safe")
	assert.Equal(t, HoldReleased, again.Status)
	assert.Zero(t, againOutbox, "a repeated cancel writes no second outbox row")

	_, _, err = f.repo.ReleaseHold(ctx, "alice", 99999)
	assert.ErrorIs(t, err, ErrHoldNotFound)

	// After release, someone else can hold the seat.
	_, created, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "bob", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)
	require.NoError(t, err)
	assert.True(t, created)
}

func TestReleaseHold_ConsumedAndExpired(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	consumed, _, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)
	require.NoError(t, err)
	_, err = payAndBook(ctx, f, ConfirmInput{UserID: "alice", HoldID: consumed.ID})
	require.NoError(t, err)
	expired, _, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[1]}, testHoldTTL)
	require.NoError(t, err)
	expireHold(t, f, expired.ID)

	_, _, err = f.repo.ReleaseHold(ctx, "alice", consumed.ID)
	assert.ErrorIs(t, err, ErrHoldAlreadyUsed)
	_, _, err = f.repo.ReleaseHold(ctx, "alice", expired.ID)
	assert.ErrorIs(t, err, ErrHoldExpired)
}

func TestSeatMap_AvailableHeldBookedExpired(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	// A1 booked, A2 held, A3 held-then-expired.
	h1, _, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)
	require.NoError(t, err)
	_, err = payAndBook(ctx, f, ConfirmInput{UserID: "alice", HoldID: h1.ID})
	require.NoError(t, err)
	_, _, err = f.repo.CreateHold(ctx, HoldSeatInput{UserID: "bob", ShowID: f.showID, SeatID: f.seatIDs[1]}, testHoldTTL)
	require.NoError(t, err)
	h3, _, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "carol", ShowID: f.showID, SeatID: f.seatIDs[2]}, testHoldTTL)
	require.NoError(t, err)
	expireHold(t, f, h3.ID)

	seats, err := f.repo.ListSeatsWithStatus(ctx, f.showID)

	require.NoError(t, err)
	require.Len(t, seats, 3)
	assert.Equal(t, SeatBooked, seats[0].Status)
	assert.Equal(t, SeatHeld, seats[1].Status)
	assert.Equal(t, SeatAvailable, seats[2].Status, "an expired hold (still ACTIVE in the table) shows as AVAILABLE")
}
