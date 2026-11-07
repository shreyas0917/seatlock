// observer_test.go checks that every hold/confirm attempt increments EXACTLY
// ONE outcome counter, with the right label, using the real metrics package
// as the observer and prometheus/testutil to read the counters.
package booking

import (
	"context"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shreyas0917/seatlock/internal/metrics"
)

// total sums every child of a counter vector.
func total(t *testing.T, vec *prometheus.CounterVec) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(vec))
	families, err := reg.Gather()
	require.NoError(t, err)
	sum := 0.0
	for _, f := range families {
		for _, m := range f.GetMetric() {
			sum += m.GetCounter().GetValue()
		}
	}
	return sum
}

func TestHoldOutcomes_ExactlyOneCounterPerAttempt(t *testing.T) {
	tests := []struct {
		want    string
		prepare func(svc *Service, repo *fakeRepository, locker *fakeLocker) HoldSeatInput
	}{
		{HoldCreated, func(*Service, *fakeRepository, *fakeLocker) HoldSeatInput {
			return HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1}
		}},
		{HoldReused, func(svc *Service, _ *fakeRepository, _ *fakeLocker) HoldSeatInput {
			in := HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1}
			_, _, _ = svc.holdSeat(context.Background(), in) // first hold, not observed
			return in
		}},
		{HoldSeatHeld, func(_ *Service, _ *fakeRepository, l *fakeLocker) HoldSeatInput {
			l.heldBy = "bob"
			return HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1}
		}},
		{HoldAlreadyBooked, func(_ *Service, r *fakeRepository, _ *fakeLocker) HoldSeatInput {
			r.createHoldErr = ErrSeatAlreadyBooked
			return HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1}
		}},
		{HoldLockUnavailable, func(_ *Service, _ *fakeRepository, l *fakeLocker) HoldSeatInput {
			l.acquireErr = ErrLockUnavailable
			return HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1}
		}},
		{HoldPaymentInProgress, func(_ *Service, r *fakeRepository, _ *fakeLocker) HoldSeatInput {
			r.createHoldErr = ErrPaymentInProgress
			return HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1}
		}},
		{HoldInvalid, func(*Service, *fakeRepository, *fakeLocker) HoldSeatInput {
			return HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 99}
		}},
		{HoldError, func(_ *Service, r *fakeRepository, _ *fakeLocker) HoldSeatInput {
			r.createHoldErr = fmt.Errorf("commit: %w", ErrBusy)
			return HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			svc, repo, locker := newTestService()
			m := metrics.NewAPI()
			svc.SetObserver(m)
			in := tc.prepare(svc, repo, locker)

			_, _, _ = svc.HoldSeat(context.Background(), in)

			assert.Equal(t, 1.0, testutil.ToFloat64(m.Holds.WithLabelValues(tc.want)))
			assert.Equal(t, 1.0, total(t, m.Holds), "exactly one counter for one attempt")
		})
	}
}

func TestConfirmOutcomes_ExactlyOneCounterPerAttempt(t *testing.T) {
	tests := []struct {
		want    string
		prepare func(repo *fakeRepository, gw *fakeGateway) int64 // returns hold id offset
	}{
		{ConfirmBooked, func(*fakeRepository, *fakeGateway) int64 { return 0 }},
		{ConfirmDeclined, func(_ *fakeRepository, g *fakeGateway) int64 { g.decline = true; return 0 }},
		{ConfirmPending, func(_ *fakeRepository, g *fakeGateway) int64 {
			g.chargeErr = fmt.Errorf("charge: %w", ErrGatewayNoAnswer)
			return 0
		}},
		{ConfirmNotCompleted, func(_ *fakeRepository, g *fakeGateway) int64 { g.voided = true; return 0 }},
		{ConfirmExpiredRefunded, func(r *fakeRepository, _ *fakeGateway) int64 { r.expireDuringPayment = true; return 0 }},
		{ConfirmExpiredRefundPending, func(r *fakeRepository, g *fakeGateway) int64 {
			r.expireDuringPayment = true
			g.refundErr = fmt.Errorf("refund: %w", ErrGatewayNoAnswer)
			return 0
		}},
		{ConfirmInvalid, func(*fakeRepository, *fakeGateway) int64 { return 999 }},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			svc, repo, _, gateway := newTestServiceWithGateway()
			h, _, err := svc.holdSeat(context.Background(), HoldSeatInput{UserID: "alice", ShowID: 1, SeatID: 1})
			require.NoError(t, err)
			m := metrics.NewAPI()
			svc.SetObserver(m)
			offset := tc.prepare(repo, gateway)

			_, _ = svc.ConfirmBooking(context.Background(), ConfirmInput{UserID: "alice", HoldID: h.ID + offset})

			assert.Equal(t, 1.0, testutil.ToFloat64(m.Confirms.WithLabelValues(tc.want)))
			assert.Equal(t, 1.0, total(t, m.Confirms), "exactly one counter for one attempt")
		})
	}
}

func TestConfirmOutcomeOf_AlreadyUsedAndExpired(t *testing.T) {
	assert.Equal(t, ConfirmAlreadyUsed, ConfirmOutcomeOf(ErrHoldAlreadyUsed))
	assert.Equal(t, ConfirmAlreadyUsed, ConfirmOutcomeOf(ErrSeatAlreadyBooked))
	assert.Equal(t, ConfirmExpired, ConfirmOutcomeOf(&HoldExpiredError{}))
	assert.Equal(t, ConfirmError, ConfirmOutcomeOf(fmt.Errorf("x: %w", ErrBusy)))
}
