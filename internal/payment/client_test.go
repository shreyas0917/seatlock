// client_test.go tests the HTTP payment client against the in-process mock
// gateway (internal/paymentmock via httptest). No Docker needed.
package payment

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shreyas0917/seatlock/internal/booking"
	"github.com/shreyas0917/seatlock/internal/paymentmock"
)

func newMock(t *testing.T) (*paymentmock.Server, *httptest.Server) {
	t.Helper()
	mock := paymentmock.New(paymentmock.Options{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	return mock, srv
}

// queue forces the behaviour of the next charge.
func queue(t *testing.T, srv *httptest.Server, behavior string) {
	t.Helper()
	resp, err := srv.Client().Post(srv.URL+"/debug/next-behavior", "application/json",
		strings.NewReader(`{"behavior":"`+behavior+`"}`))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestCharge_Succeeded(t *testing.T) {
	_, srv := newMock(t)
	c := NewClient(srv.URL, 2*time.Second)

	res, err := c.Charge(context.Background(), booking.ChargeRequest{IdempotencyKey: "pay-1", AmountPaise: 25000})

	require.NoError(t, err)
	assert.Equal(t, booking.ChargeSucceeded, res.Status)
	assert.NotEmpty(t, res.ChargeID)
}

func TestCharge_Declined(t *testing.T) {
	_, srv := newMock(t)
	queue(t, srv, paymentmock.BehaviorDecline)

	res, err := NewClient(srv.URL, 2*time.Second).Charge(context.Background(),
		booking.ChargeRequest{IdempotencyKey: "pay-1", AmountPaise: 25000})

	require.NoError(t, err, "a decline is a definite answer, not an error")
	assert.Equal(t, booking.ChargeDeclined, res.Status)
}

// TestCharge_Timeout_IsNoAnswer: the gateway took the money but never
// answered. The client must report "no answer", and a retry with the same
// key must reveal the real result without charging again.
func TestCharge_Timeout_IsNoAnswer(t *testing.T) {
	mock, srv := newMock(t)
	queue(t, srv, paymentmock.BehaviorTimeout)
	c := NewClient(srv.URL, 300*time.Millisecond)

	_, err := c.Charge(context.Background(), booking.ChargeRequest{IdempotencyKey: "pay-7", AmountPaise: 25000})
	require.ErrorIs(t, err, booking.ErrGatewayNoAnswer)

	res, err := c.Charge(context.Background(), booking.ChargeRequest{IdempotencyKey: "pay-7", AmountPaise: 25000})
	require.NoError(t, err)
	assert.Equal(t, booking.ChargeSucceeded, res.Status, "the money had been taken")
	assert.Equal(t, 1, mock.Stats().TotalCharges, "the retry did not charge again")
}

func TestCharge_GatewayDown_IsNoAnswer(t *testing.T) {
	_, err := NewClient("http://127.0.0.1:1", time.Second).Charge(context.Background(),
		booking.ChargeRequest{IdempotencyKey: "pay-1", AmountPaise: 25000})

	assert.ErrorIs(t, err, booking.ErrGatewayNoAnswer)
}

func TestCharge_Non200_IsNoAnswer(t *testing.T) {
	_, srv := newMock(t)
	c := NewClient(srv.URL, time.Second)
	_, err := c.Charge(context.Background(), booking.ChargeRequest{IdempotencyKey: "pay-1", AmountPaise: 25000})
	require.NoError(t, err)

	// Same key, different amount: the mock answers 422.
	_, err = c.Charge(context.Background(), booking.ChargeRequest{IdempotencyKey: "pay-1", AmountPaise: 1})

	assert.ErrorIs(t, err, booking.ErrGatewayNoAnswer)
	assert.Contains(t, err.Error(), "HTTP 422")
}

func TestRefund(t *testing.T) {
	mock, srv := newMock(t)
	c := NewClient(srv.URL, time.Second)
	charge, err := c.Charge(context.Background(), booking.ChargeRequest{IdempotencyKey: "pay-1", AmountPaise: 25000})
	require.NoError(t, err)

	r1, err := c.Refund(context.Background(), booking.RefundRequest{IdempotencyKey: "refund-1", ChargeID: charge.ChargeID})
	require.NoError(t, err)
	r2, err := c.Refund(context.Background(), booking.RefundRequest{IdempotencyKey: "refund-1", ChargeID: charge.ChargeID})
	require.NoError(t, err)

	assert.Equal(t, r1.RefundID, r2.RefundID, "a repeated refund key is not paid twice")
	assert.Equal(t, 1, mock.Stats().TotalRefunds)
}
