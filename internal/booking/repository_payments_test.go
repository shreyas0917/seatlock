//go:build integration

// repository_payments_test.go tests Tx A (BeginPayment), Tx B
// (CompletePayment) and the payment status updates against the real
// PostgreSQL. It reuses setupTestDB and the hold helpers from the other
// repository test files.
package booking

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newHold creates an ACTIVE hold for the user on seat A1.
func newHold(t *testing.T, f testFixture, userID string) Hold {
	t.Helper()
	h, _, err := f.repo.CreateHold(context.Background(),
		HoldSeatInput{UserID: userID, ShowID: f.showID, SeatID: f.seatIDs[0]}, testHoldTTL)
	require.NoError(t, err)
	return h
}

func paymentStatus(t *testing.T, f testFixture, paymentID int64) PaymentStatus {
	t.Helper()
	var s string
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT status FROM payments WHERE id = $1`, paymentID).Scan(&s))
	return PaymentStatus(s)
}

func countOutbox(t *testing.T, f testFixture, kind string) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox WHERE kind = $1`, kind).Scan(&n))
	return n
}

func TestBeginPayment_CreatesThenReuses(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	h := newHold(t, f, "alice")

	first, created, err := f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, PaymentPending, first.Status)
	assert.Equal(t, int64(25000), first.AmountPaise, "the amount is the seat's price")
	assert.Equal(t, "pay-1", first.ChargeKey, "charge_key is derived from the payment id")

	second, created, err := f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)
	assert.False(t, created, "a payment in flight is reused, never duplicated")
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, first.ChargeKey, second.ChargeKey)
}

func TestBeginPayment_ConcurrentForOneHold_OnePayment(t *testing.T) {
	f := setupTestDB(t)
	h := newHold(t, f, "alice")

	ids := make([]int64, 10)
	errs := make([]error, 10)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, _, err := f.repo.BeginPayment(context.Background(), ConfirmInput{UserID: "alice", HoldID: h.ID})
			ids[i], errs[i] = p.ID, err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "request %d", i)
		assert.Equal(t, ids[0], ids[i], "every request gets the same payment")
	}
	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM payments`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestBeginPayment_Rejections(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	h := newHold(t, f, "alice")

	_, _, err := f.repo.BeginPayment(ctx, ConfirmInput{UserID: "mallory", HoldID: h.ID})
	assert.ErrorIs(t, err, ErrHoldNotFound, "another user's hold looks like a missing one")

	_, _, err = f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: 99999})
	assert.ErrorIs(t, err, ErrHoldNotFound)

	expireHold(t, f, h.ID)
	_, _, err = f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: h.ID})
	assert.ErrorIs(t, err, ErrHoldExpired)
	var expired *HoldExpiredError
	require.True(t, errors.As(err, &expired))
	assert.Empty(t, expired.PaymentStatus, "no money was taken, so no refund to report")
}

func TestCompletePayment_Books(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	h := newHold(t, f, "alice")
	p, _, err := f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)

	res, err := f.repo.CompletePayment(ctx, p.ID, "ch_1")

	require.NoError(t, err)
	assert.Equal(t, Booked, res.Kind)
	assert.Equal(t, h.ID, res.Booking.HoldID)
	assert.Equal(t, PaymentSucceeded, res.Payment.Status)
	assert.Equal(t, "ch_1", res.Payment.ProviderChargeID)
	assert.Equal(t, HoldConsumed, holdStatus(t, f, h.ID))

	_, _, err = f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: h.ID})
	assert.ErrorIs(t, err, ErrHoldAlreadyUsed, "a consumed hold cannot be paid for again")
}

// TestCompletePayment_Twice_SameBookingNoRefund is RULE 1: a payment that
// already backs a booking is never refunded. Even if the hold has expired by
// the time the second Tx B runs, it returns the existing booking.
func TestCompletePayment_Twice_SameBookingNoRefund(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	h := newHold(t, f, "alice")
	p, _, err := f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)
	first, err := f.repo.CompletePayment(ctx, p.ID, "ch_1")
	require.NoError(t, err)
	expireHold(t, f, h.ID)

	second, err := f.repo.CompletePayment(ctx, p.ID, "ch_1")

	require.NoError(t, err)
	assert.Equal(t, Booked, second.Kind)
	assert.Equal(t, first.Booking.ID, second.Booking.ID)
	assert.Equal(t, PaymentSucceeded, paymentStatus(t, f, p.ID))
	assert.Zero(t, countOutbox(t, f, "REFUND"), "no refund for a payment that backs a booking")
}

func TestCompletePayment_HoldExpired_RefundRequired(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	h := newHold(t, f, "alice")
	p, _, err := f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)
	expireHold(t, f, h.ID) // the hold runs out while the gateway works

	res, err := f.repo.CompletePayment(ctx, p.ID, "ch_9")

	require.NoError(t, err)
	assert.Equal(t, RefundRequired, res.Kind)
	assert.Equal(t, PaymentRefundPending, paymentStatus(t, f, p.ID))
	assert.Equal(t, HoldExpired, holdStatus(t, f, h.ID))
	var bookings int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM bookings`).Scan(&bookings))
	assert.Zero(t, bookings, "no booking from an expired hold")

	// The outbox row was written in the same transaction.
	var payload []byte
	var status string
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT payload, status FROM outbox WHERE id = $1`, res.OutboxID).Scan(&payload, &status))
	assert.Equal(t, "PENDING", status)
	var body map[string]any
	require.NoError(t, json.Unmarshal(payload, &body))
	assert.Equal(t, "ch_9", body["charge_id"])
	assert.Equal(t, p.RefundKey(), body["refund_key"])

	// A second Tx B for the same payment changes nothing.
	again, err := f.repo.CompletePayment(ctx, p.ID, "ch_9")
	require.NoError(t, err)
	assert.Equal(t, AlreadyRefunding, again.Kind)
	assert.Equal(t, 1, countOutbox(t, f, "REFUND"), "no second refund is queued")

	refunded, err := f.repo.MarkRefunded(ctx, p.ID, res.OutboxID)
	require.NoError(t, err)
	assert.Equal(t, PaymentRefunded, refunded.Status)
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT status FROM outbox WHERE id = $1`, res.OutboxID).Scan(&status))
	assert.Equal(t, "DONE", status)

	// A later confirm attempt reports the refund.
	_, _, err = f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: h.ID})
	var expired *HoldExpiredError
	require.True(t, errors.As(err, &expired))
	assert.Equal(t, PaymentRefunded, expired.PaymentStatus)
}

// A retry after an UNKNOWN outcome must reach the gateway again even if the
// hold has expired meanwhile: only the gateway knows whether money was taken.
func TestBeginPayment_ExpiredHoldWithPendingPayment_IsReused(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	h := newHold(t, f, "alice")
	p, _, err := f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)
	expireHold(t, f, h.ID)

	again, created, err := f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: h.ID})

	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, p.ID, again.ID)
}

func TestDeclinedPayment_AllowsANewAttempt(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	h := newHold(t, f, "alice")
	first, _, err := f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)

	require.NoError(t, f.repo.MarkPaymentDeclined(ctx, first.ID))
	assert.Equal(t, PaymentDeclined, paymentStatus(t, f, first.ID))
	assert.Equal(t, HoldActive, holdStatus(t, f, h.ID), "a decline keeps the hold")

	second, created, err := f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)
	assert.True(t, created)
	assert.NotEqual(t, first.ChargeKey, second.ChargeKey, "a new attempt gets a new charge key")
}

func TestReleaseHold_WhilePaymentPending_ErrPaymentInProgress(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	h := newHold(t, f, "alice")
	_, _, err := f.repo.BeginPayment(ctx, ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)

	_, _, err = f.repo.ReleaseHold(ctx, "alice", h.ID)

	assert.ErrorIs(t, err, ErrPaymentInProgress)
	assert.Equal(t, HoldActive, holdStatus(t, f, h.ID))
	assert.Zero(t, countOutbox(t, f, "RELEASE_LOCK"))
}

func TestReleaseHold_WritesReleaseLockOutboxRow(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()
	h := newHold(t, f, "alice")

	_, outboxID, err := f.repo.ReleaseHold(ctx, "alice", h.ID)

	require.NoError(t, err)
	var payload []byte
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT payload FROM outbox WHERE id = $1 AND kind = 'RELEASE_LOCK'`, outboxID).Scan(&payload))
	var body map[string]any
	require.NoError(t, json.Unmarshal(payload, &body))
	assert.Equal(t, "alice", body["owner"])

	require.NoError(t, f.repo.MarkOutboxDone(ctx, outboxID))
	var status string
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT status FROM outbox WHERE id = $1`, outboxID).Scan(&status))
	assert.Equal(t, "DONE", status)
}
