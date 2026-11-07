// Package payment is the HTTP client for the payment gateway. It implements
// booking.PaymentGateway.
//
// Where it fits:
//
//	Service.ConfirmBooking -> payment.Client.Charge -> POST {PAYMENT_URL}/charges
//
// Its main job is to turn the messy world of HTTP into two clear cases:
//   - a DEFINITE answer (succeeded / declined / refunded), or
//   - booking.ErrGatewayNoAnswer: we do not know what happened.
//
// The second case must never be reported as "declined": a timeout can mean
// the money was taken and only the answer was lost.
package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shreyas0917/seatlock/internal/booking"
)

// Client talks to the payment gateway over HTTP.
type Client struct {
	baseURL string
	http    *http.Client
}

// Compile-time check that Client satisfies the booking.PaymentGateway interface.
var _ booking.PaymentGateway = (*Client)(nil)

// NewClient returns a client for the gateway at baseURL (e.g.
// "http://payment:8090"). timeout (PAYMENT_TIMEOUT, default 5s) caps every
// call, so a hanging gateway can never hang a booking request forever.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: timeout}}
}

type chargeBody struct {
	IdempotencyKey string `json:"idempotency_key"`
	AmountPaise    int64  `json:"amount_paise"`
	Reference      string `json:"reference"`
}

type chargeReply struct {
	ChargeID string `json:"charge_id"`
	Status   string `json:"status"`
}

// Charge asks the gateway to take money.
func (c *Client) Charge(ctx context.Context, req booking.ChargeRequest) (booking.ChargeResult, error) {
	var reply chargeReply
	err := c.post(ctx, "/charges", chargeBody{
		IdempotencyKey: req.IdempotencyKey,
		AmountPaise:    req.AmountPaise,
		Reference:      req.Reference,
	}, &reply)
	if err != nil {
		return booking.ChargeResult{}, fmt.Errorf("charge %s: %w", req.IdempotencyKey, err)
	}

	switch booking.ChargeStatus(reply.Status) {
	case booking.ChargeSucceeded, booking.ChargeDeclined:
		return booking.ChargeResult{ChargeID: reply.ChargeID, Status: booking.ChargeStatus(reply.Status)}, nil
	default:
		// "refunded" (a replayed key whose charge was later refunded) or
		// anything unexpected: not a clean answer to "did this charge
		// succeed now?". Treat it as no answer; callers keep the payment
		// PENDING rather than guess.
		return booking.ChargeResult{}, fmt.Errorf("charge %s: %w: unexpected status %q",
			req.IdempotencyKey, booking.ErrGatewayNoAnswer, reply.Status)
	}
}

type refundBody struct {
	IdempotencyKey string `json:"idempotency_key"`
	ChargeID       string `json:"charge_id"`
}

type refundReply struct {
	RefundID string `json:"refund_id"`
	Status   string `json:"status"`
}

// Refund asks the gateway to return a charge's money.
func (c *Client) Refund(ctx context.Context, req booking.RefundRequest) (booking.RefundResult, error) {
	var reply refundReply
	if err := c.post(ctx, "/refunds", refundBody{IdempotencyKey: req.IdempotencyKey, ChargeID: req.ChargeID}, &reply); err != nil {
		return booking.RefundResult{}, fmt.Errorf("refund %s: %w", req.IdempotencyKey, err)
	}
	if reply.Status != "refunded" || reply.RefundID == "" {
		return booking.RefundResult{}, fmt.Errorf("refund %s: %w: unexpected status %q",
			req.IdempotencyKey, booking.ErrGatewayNoAnswer, reply.Status)
	}
	return booking.RefundResult{RefundID: reply.RefundID}, nil
}

// post sends a JSON body and decodes a 200 JSON reply into out. Every
// failure (network, timeout, non-200 status, bad JSON) wraps
// booking.ErrGatewayNoAnswer.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err) // a programming bug, not a gateway answer
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Timeout, connection refused, connection reset, ...
		return fmt.Errorf("%w: %w", booking.ErrGatewayNoAnswer, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: read response: %w", booking.ErrGatewayNoAnswer, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d: %s", booking.ErrGatewayNoAnswer, resp.StatusCode, bytes.TrimSpace(data))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: decode response: %w", booking.ErrGatewayNoAnswer, err)
	}
	return nil
}
