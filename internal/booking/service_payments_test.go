// service_payments_test.go unit-tests ConfirmBooking (service_payments.go):
// the four payment outcomes, the inline refund, and "no answer is not failed",
// using the fakes from service_test.go (no database, Redis or gateway).
package booking

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// holdFor creates an active hold for alice on seat 1 of show 1.
func holdFor(t *testing.T, svc *Service) Hold {
	t.Helper()
	h, _, err := svc.HoldSeat(context.Background(), HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1})
	require.NoError(t, err)
	return h
}

func TestConfirm_Succeeded_Books(t *testing.T) {
	svc, repo, locker, gateway := newTestServiceWithGateway()
	h := holdFor(t, svc)

	b, err := svc.ConfirmBooking(context.Background(), ConfirmInput{UserID: "alice", HoldID: h.ID})

	require.NoError(t, err)
	assert.Equal(t, h.ID, b.HoldID)
	require.Len(t, gateway.chargeKeys, 1)
	assert.Regexp(t, `^pay-\d+$`, gateway.chargeKeys[0], "the gateway gets the payment's own key")
	assert.Equal(t, 1, locker.releaseCalls, "the Redis lock is released after booking (best effort)")
	for _, p := range repo.payments {
		assert.Equal(t, PaymentSucceeded, p.Status)
	}
}

func TestConfirm_Declined_KeepsHold(t *testing.T) {
	svc, repo, _, gateway := newTestServiceWithGateway()
	gateway.decline = true
	h := holdFor(t, svc)

	_, err := svc.ConfirmBooking(context.Background(), ConfirmInput{UserID: "alice", HoldID: h.ID})

	assert.ErrorIs(t, err, ErrPaymentDeclined)
	assert.Equal(t, HoldActive, repo.holds[h.ID].Status, "a decline keeps the hold so the user can retry")
	assert.Empty(t, repo.bookings)

	// The retry starts a NEW payment with a NEW charge key.
	gateway.decline = false
	_, err = svc.ConfirmBooking(context.Background(), ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)
	require.Len(t, gateway.chargeKeys, 2)
	assert.NotEqual(t, gateway.chargeKeys[0], gateway.chargeKeys[1])
}

// TestConfirm_NoAnswer_StaysPending: a timeout is NOT a failure. The payment
// stays PENDING, and the retry re-sends the SAME charge key.
func TestConfirm_NoAnswer_StaysPending(t *testing.T) {
	svc, repo, _, gateway := newTestServiceWithGateway()
	gateway.chargeErr = fmt.Errorf("charge pay-1: %w: context deadline exceeded", ErrGatewayNoAnswer)
	h := holdFor(t, svc)

	_, err := svc.ConfirmBooking(context.Background(), ConfirmInput{UserID: "alice", HoldID: h.ID})

	assert.ErrorIs(t, err, ErrPaymentPending)
	assert.Empty(t, repo.bookings)
	for _, p := range repo.payments {
		assert.Equal(t, PaymentPending, p.Status, "unknown outcome: never mark it failed")
	}

	gateway.chargeErr = nil
	b, err := svc.ConfirmBooking(context.Background(), ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)
	assert.Equal(t, h.ID, b.HoldID)
	require.Len(t, gateway.chargeKeys, 2)
	assert.Equal(t, gateway.chargeKeys[0], gateway.chargeKeys[1], "the retry re-sends the same charge key")
}

func TestConfirm_TxBFails_IsPending(t *testing.T) {
	svc, repo, _, _ := newTestServiceWithGateway()
	repo.completeErr = fmt.Errorf("commit: %w", ErrBusy)
	h := holdFor(t, svc)

	_, err := svc.ConfirmBooking(context.Background(), ConfirmInput{UserID: "alice", HoldID: h.ID})

	assert.ErrorIs(t, err, ErrPaymentPending, "money taken but not recorded: outcome unknown to the client")
	assert.ErrorIs(t, err, ErrBusy, "the cause is kept for the logs")
}

func TestConfirm_HoldExpiredDuringPayment_Refunds(t *testing.T) {
	svc, repo, _, gateway := newTestServiceWithGateway()
	repo.expireDuringPayment = true
	h := holdFor(t, svc)

	_, err := svc.ConfirmBooking(context.Background(), ConfirmInput{UserID: "alice", HoldID: h.ID})

	var expired *HoldExpiredError
	require.True(t, errors.As(err, &expired), "got %v", err)
	assert.ErrorIs(t, err, ErrHoldExpired)
	assert.Equal(t, PaymentRefunded, expired.PaymentStatus)
	assert.Equal(t, 1, gateway.refunds)
	assert.Len(t, repo.outboxDone, 1, "the refund outbox row is marked DONE")
	assert.Empty(t, repo.bookings, "no booking")
}

func TestConfirm_RefundFails_StaysRefundPending(t *testing.T) {
	svc, repo, _, gateway := newTestServiceWithGateway()
	repo.expireDuringPayment = true
	gateway.refundErr = fmt.Errorf("refund: %w", ErrGatewayNoAnswer)
	h := holdFor(t, svc)

	_, err := svc.ConfirmBooking(context.Background(), ConfirmInput{UserID: "alice", HoldID: h.ID})

	var expired *HoldExpiredError
	require.True(t, errors.As(err, &expired))
	assert.Equal(t, PaymentRefundPending, expired.PaymentStatus, "the outbox row stays for the worker")
	assert.Empty(t, repo.outboxDone)
}

func TestConfirm_BeginPaymentErrorsPassThrough(t *testing.T) {
	svc, _, _, gateway := newTestServiceWithGateway()

	_, err := svc.ConfirmBooking(context.Background(), ConfirmInput{UserID: "alice", HoldID: 999})

	assert.ErrorIs(t, err, ErrHoldNotFound)
	assert.Empty(t, gateway.chargeKeys, "no charge without a valid hold")
}
