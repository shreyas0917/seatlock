// service_worker_test.go unit-tests the worker's business actions
// (service_worker.go) with the fakes from service_test.go.
package booking

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// abandonedPayment simulates a confirm whose gateway call timed out and was
// never retried: a hold for alice on seat 1 with a PENDING payment.
func abandonedPayment(t *testing.T, svc *Service, repo *fakeRepository) Payment {
	t.Helper()
	h := holdFor(t, svc)
	p, _, err := repo.BeginPayment(context.Background(), ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)
	return p
}

func TestReconcile_ChargeSucceeded_Books(t *testing.T) {
	svc, repo, locker, gateway := newTestServiceWithGateway()
	p := abandonedPayment(t, svc, repo)
	gateway.lookupStatus = ChargeSucceeded

	outcome, err := svc.ReconcilePayment(context.Background(), p)

	require.NoError(t, err)
	assert.Equal(t, ReconcileBooked, outcome)
	assert.Equal(t, PaymentSucceeded, repo.payments[p.ID].Status)
	assert.Len(t, repo.bookings, 1)
	assert.Equal(t, 1, locker.releaseCalls, "the shared completeCharged path releases the lock")
	assert.Empty(t, gateway.chargeKeys, "the reconciler never charges, it only asks")
}

func TestReconcile_ChargeSucceeded_HoldDead_QueuesRefundOnly(t *testing.T) {
	svc, repo, _, gateway := newTestServiceWithGateway()
	p := abandonedPayment(t, svc, repo)
	gateway.lookupStatus = ChargeSucceeded
	repo.expireDuringPayment = true

	outcome, err := svc.ReconcilePayment(context.Background(), p)

	require.NoError(t, err)
	assert.Equal(t, ReconcileRefundRequired, outcome)
	assert.Equal(t, PaymentRefundPending, repo.payments[p.ID].Status)
	assert.Zero(t, gateway.refunds, "the refund is left to the outbox drainer")
	assert.Empty(t, repo.bookings)
}

func TestReconcile_NotFound_VoidsThenAbandons(t *testing.T) {
	svc, repo, _, gateway := newTestServiceWithGateway()
	p := abandonedPayment(t, svc, repo)

	outcome, err := svc.ReconcilePayment(context.Background(), p)

	require.NoError(t, err)
	assert.Equal(t, ReconcileAbandoned, outcome)
	assert.Equal(t, 1, gateway.voids, "the key is closed at the gateway BEFORE abandoning")
	assert.Equal(t, PaymentAbandoned, repo.payments[p.ID].Status)
}

// The void race: the late charge got to the gateway just before the void.
func TestReconcile_NotFound_VoidLosesRace_Books(t *testing.T) {
	svc, repo, _, gateway := newTestServiceWithGateway()
	p := abandonedPayment(t, svc, repo)
	gateway.voidStatus = ChargeSucceeded

	outcome, err := svc.ReconcilePayment(context.Background(), p)

	require.NoError(t, err)
	assert.Equal(t, ReconcileBooked, outcome)
	assert.Len(t, repo.bookings, 1)
}

func TestReconcile_Declined(t *testing.T) {
	svc, repo, _, gateway := newTestServiceWithGateway()
	p := abandonedPayment(t, svc, repo)
	gateway.lookupStatus = ChargeDeclined

	outcome, err := svc.ReconcilePayment(context.Background(), p)

	require.NoError(t, err)
	assert.Equal(t, ReconcileDeclined, outcome)
	assert.Equal(t, PaymentDeclined, repo.payments[p.ID].Status)
}

func TestReconcile_GatewayDown_StaysPendingAndRecordsError(t *testing.T) {
	svc, repo, _, gateway := newTestServiceWithGateway()
	p := abandonedPayment(t, svc, repo)
	gateway.lookupErr = fmt.Errorf("lookup: %w: connection refused", ErrGatewayNoAnswer)

	outcome, err := svc.ReconcilePayment(context.Background(), p)

	assert.ErrorIs(t, err, ErrGatewayNoAnswer)
	assert.Equal(t, ReconcileStillPending, outcome)
	assert.Equal(t, PaymentPending, repo.payments[p.ID].Status)
	assert.Zero(t, gateway.voids, "down is NOT 'not found': never void on an error")
	require.Len(t, repo.reconcileFailures, 1)
	assert.Contains(t, repo.reconcileFailures[0], "connection refused")
}

func TestProcessOutbox_Refund(t *testing.T) {
	svc, repo, _, gateway := newTestServiceWithGateway()
	payload, _ := json.Marshal(refundPayload{PaymentID: 7, ChargeID: "ch_1", RefundKey: "refund-7"})
	repo.payments[7] = Payment{ID: 7, Status: PaymentRefundPending}

	err := svc.ProcessOutboxRow(context.Background(), OutboxRow{ID: 3, Kind: OutboxRefund, Payload: payload})

	require.NoError(t, err)
	assert.Equal(t, 1, gateway.refunds)
	assert.Equal(t, PaymentRefunded, repo.payments[7].Status)
	assert.Equal(t, []int64{3}, repo.outboxDone)

	gateway.refundErr = fmt.Errorf("refund: %w", ErrGatewayNoAnswer)
	err = svc.ProcessOutboxRow(context.Background(), OutboxRow{ID: 4, Kind: OutboxRefund, Payload: payload})
	assert.ErrorIs(t, err, ErrGatewayNoAnswer, "a failed refund comes back as an error, to be retried")
}

func TestProcessOutbox_ReleaseLock(t *testing.T) {
	svc, repo, locker, _ := newTestServiceWithGateway()
	payload, _ := json.Marshal(releaseLockPayload{ShowID: 1, SeatID: 1, Owner: "alice"})

	require.NoError(t, svc.ProcessOutboxRow(context.Background(), OutboxRow{ID: 5, Kind: OutboxReleaseLock, Payload: payload}))
	assert.Equal(t, 1, locker.releaseCalls)
	assert.Equal(t, []int64{5}, repo.outboxDone)

	locker.releaseErr = ErrLockUnavailable // Redis down
	err := svc.ProcessOutboxRow(context.Background(), OutboxRow{ID: 6, Kind: OutboxReleaseLock, Payload: payload})
	assert.ErrorIs(t, err, ErrLockUnavailable, "Redis down is a failure: retry later")
	assert.Equal(t, []int64{5}, repo.outboxDone, "not marked done")
}

func TestProcessOutbox_UnknownKindIsAnError(t *testing.T) {
	svc, _, _, _ := newTestServiceWithGateway()
	assert.Error(t, svc.ProcessOutboxRow(context.Background(), OutboxRow{ID: 1, Kind: "NOPE", Payload: []byte(`{}`)}))
}
