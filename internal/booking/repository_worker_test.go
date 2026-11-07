//go:build integration

// repository_worker_test.go tests the worker's claim/backoff/expiry SQL
// (repository_worker.go) and the "hold with a payment in flight" rules
// (deadHoldCondition) against the real PostgreSQL.
package booking

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingPayment creates a hold for userID on seat i and a PENDING payment for it.
func pendingPayment(t *testing.T, f testFixture, userID string, seat int) (Hold, Payment) {
	t.Helper()
	ctx := context.Background()
	h, _, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: userID, ShowID: f.showID, SeatID: f.seatIDs[seat]}, testHoldTTL)
	require.NoError(t, err)
	p, _, err := f.repo.BeginPayment(ctx, ConfirmInput{UserID: userID, HoldID: h.ID})
	require.NoError(t, err)
	return h, p
}

func exec(t *testing.T, f testFixture, sql string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

func TestClaimPaymentsToReconcile_OnlyOldAndLeased(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	_, old := pendingPayment(t, f, "alice", 0)
	pendingPayment(t, f, "bob", 1) // too young to reconcile
	exec(t, f, `UPDATE payments SET created_at = NOW() - interval '5 minutes' WHERE id = $1`, old.ID)

	claimed, err := f.repo.ClaimPaymentsToReconcile(ctx, 2*time.Minute, time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1, "only the payment older than 2 minutes")
	assert.Equal(t, old.ID, claimed[0].ID)

	again, err := f.repo.ClaimPaymentsToReconcile(ctx, 2*time.Minute, time.Minute, 10)
	require.NoError(t, err)
	assert.Empty(t, again, "a leased payment is not claimed twice")

	require.NoError(t, f.repo.RecordReconcileFailure(ctx, old.ID, "gateway down"))
	var lastError string
	var inFuture bool
	var attempts int
	require.NoError(t, f.pool.QueryRow(ctx, `
		SELECT last_error, next_check_at > NOW(), reconcile_attempts FROM payments WHERE id = $1`, old.ID).
		Scan(&lastError, &inFuture, &attempts))
	assert.Equal(t, "gateway down", lastError)
	assert.True(t, inFuture, "the next check is pushed back (backoff)")
	assert.Equal(t, 1, attempts)
	assert.Equal(t, PaymentPending, paymentStatus(t, f, old.ID), "a failed check changes nothing else")
}

// TestClaimOutbox_TwoClaimersGetDisjointRows: SKIP LOCKED in action.
func TestClaimOutbox_TwoClaimersGetDisjointRows(t *testing.T) {
	f := setupTestDB(t)
	for i := 0; i < 30; i++ {
		exec(t, f, `INSERT INTO outbox (kind, payload) VALUES ('RELEASE_LOCK', '{}')`)
	}

	seen := map[int64]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				rows, err := f.repo.ClaimOutbox(context.Background(), time.Minute, 4)
				if !assert.NoError(t, err) || len(rows) == 0 {
					return
				}
				mu.Lock()
				for _, r := range rows {
					seen[r.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Len(t, seen, 30, "every row claimed")
	for id, n := range seen {
		assert.Equal(t, 1, n, "row %d claimed exactly once", id)
	}
}

func TestOutbox_BackoffThenDeadLetter(t *testing.T) {
	f := setupTestDB(t)
	f.repo.SetLimits(Limits{PendingHoldMax: DefaultLimits.PendingHoldMax, OutboxMaxAttempts: 3})
	ctx := context.Background()
	exec(t, f, `INSERT INTO outbox (kind, payload) VALUES ('REFUND', '{}')`)

	var delays []float64
	for attempt := 1; attempt <= 3; attempt++ {
		exec(t, f, `UPDATE outbox SET next_attempt_at = NOW()`) // make it due now
		rows, err := f.repo.ClaimOutbox(ctx, time.Minute, 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, attempt, rows[0].Attempts)

		status, err := f.repo.FailClaimedOutbox(ctx, rows[0].ID, "refund failed")
		require.NoError(t, err)
		var delay float64
		require.NoError(t, f.pool.QueryRow(ctx,
			`SELECT EXTRACT(EPOCH FROM next_attempt_at - NOW()) FROM outbox WHERE id = $1`, rows[0].ID).Scan(&delay))
		delays = append(delays, delay)
		if attempt < 3 {
			assert.Equal(t, OutboxPending, status)
		} else {
			assert.Equal(t, OutboxFailed, status, "the 3rd failure makes it a dead letter")
		}
	}
	t.Logf("backoff delays (s): %.1f, %.1f, %.1f", delays[0], delays[1], delays[2])
	assert.Greater(t, delays[1], delays[0], "the delay grows")
	assert.Greater(t, delays[2], delays[1])

	exec(t, f, `UPDATE outbox SET next_attempt_at = NOW()`)
	rows, err := f.repo.ClaimOutbox(ctx, time.Minute, 10)
	require.NoError(t, err)
	assert.Empty(t, rows, "a FAILED row is never claimed again automatically")

	var id int64
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT id FROM outbox`).Scan(&id))
	require.NoError(t, f.repo.RequeueOutbox(ctx, id))
	rows, err = f.repo.ClaimOutbox(ctx, time.Minute, 10)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "a human can re-queue a dead letter")
}

func TestRecordOutboxFailure_InlineCountsAndBacksOff(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	var id int64
	require.NoError(t, f.pool.QueryRow(ctx, `INSERT INTO outbox (kind, payload) VALUES ('REFUND', '{}') RETURNING id`).Scan(&id))

	require.NoError(t, f.repo.RecordOutboxFailure(ctx, id, "gateway 500"))

	var attempts int
	var inFuture bool
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT attempts, next_attempt_at > NOW() FROM outbox WHERE id = $1`, id).Scan(&attempts, &inFuture))
	assert.Equal(t, 1, attempts)
	assert.True(t, inFuture, "the inline failure now schedules a backoff too")
}

func TestExpireHolds_ExpiresDeadHoldsWithReleaseLock_SkipsPaying(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	idle := newHold(t, f, "alice") // seat 0, nobody paying
	paying, _ := pendingPayment(t, f, "bob", 1)
	exec(t, f, `UPDATE holds SET expires_at = NOW() - interval '1 second'`)

	expired, err := f.repo.ExpireHolds(ctx, 50)

	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, idle.ID, expired[0].HoldID)
	assert.Equal(t, HoldExpired, holdStatus(t, f, idle.ID))
	assert.Equal(t, HoldActive, holdStatus(t, f, paying.ID), "a hold with a PENDING payment is skipped")
	var kind, owner string
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT kind, payload->>'owner' FROM outbox WHERE id = $1`, expired[0].OutboxID).Scan(&kind, &owner))
	assert.Equal(t, "RELEASE_LOCK", kind)
	assert.Equal(t, "alice", owner)

	// Past PendingHoldMax the paying hold is expired too; the payment stays PENDING.
	f.repo.SetLimits(Limits{PendingHoldMax: time.Second, OutboxMaxAttempts: 20})
	exec(t, f, `UPDATE holds SET expires_at = NOW() - interval '5 seconds' WHERE id = $1`, paying.ID)
	expired, err = f.repo.ExpireHolds(ctx, 50)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, paying.ID, expired[0].HoldID)
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM payments WHERE status = 'PENDING'`))
}

// The "payment in flight" rules on the request path (CreateHold, seat map).
func TestHoldWithPendingPayment_BlocksSeatUntilPendingHoldMax(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	h, _ := pendingPayment(t, f, "alice", 0)
	exec(t, f, `UPDATE holds SET expires_at = NOW() - interval '5 seconds' WHERE id = $1`, h.ID)

	_, _, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)
	assert.ErrorIs(t, err, ErrPaymentInProgress, "the same user is told their payment is in progress")

	_, _, err = f.repo.CreateHold(ctx, HoldSeatInput{UserID: "bob", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)
	assert.ErrorIs(t, err, ErrSeatHeld, "another user cannot take a seat whose payment is in flight")

	seats, err := f.repo.ListSeatsWithStatus(ctx, f.showID)
	require.NoError(t, err)
	assert.Equal(t, SeatHeld, seats[0].Status, "the seat map shows it HELD")

	// After PendingHoldMax, the seat frees.
	f.repo.SetLimits(Limits{PendingHoldMax: time.Second, OutboxMaxAttempts: 20})
	seats, err = f.repo.ListSeatsWithStatus(ctx, f.showID)
	require.NoError(t, err)
	assert.Equal(t, SeatAvailable, seats[0].Status)
	_, created, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: "bob", ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, HoldExpired, holdStatus(t, f, h.ID))
}

// count is a small helper on the fixture.
func (f testFixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}
