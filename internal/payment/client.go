// Package payment is the HTTP client for the payment gateway. It implements
// booking.PaymentGateway.
//
// Where it fits:
//
//	Service.ConfirmBooking -> payment.Client.Charge -> POST {PAYMENT_URL}/charges
//
// Its main job is to turn the messy world of HTTP into two clear cases:
//   - a DEFINITE answer (succeeded / declined / voided / refunded / not found), or
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
	"net/url"
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

	return toChargeResult("charge", req.IdempotencyKey, reply)
}

// Lookup asks the gateway what happened to a charge key. A 404 is a
// definite "no such charge" (booking.ErrChargeNotFound).
func (c *Client) Lookup(ctx context.Context, key string) (booking.ChargeResult, error) {
	var reply chargeReply
	status, err := c.do(ctx, http.MethodGet, "/charges/"+url.PathEscape(key), nil, &reply, http.StatusNotFound)
	if err != nil {
		return booking.ChargeResult{}, fmt.Errorf("lookup %s: %w", key, err)
	}
	if status == http.StatusNotFound {
		return booking.ChargeResult{}, fmt.Errorf("lookup %s: %w", key, booking.ErrChargeNotFound)
	}
	return toChargeResult("lookup", key, reply)
}

// Void closes a key at the gateway. The answer is "voided" if no charge
// existed, or the real result ("succeeded"/"declined") if one did.
func (c *Client) Void(ctx context.Context, key string) (booking.ChargeResult, error) {
	var reply chargeReply
	if _, err := c.do(ctx, http.MethodPost, "/charges/"+url.PathEscape(key)+"/void", struct{}{}, &reply); err != nil {
		return booking.ChargeResult{}, fmt.Errorf("void %s: %w", key, err)
	}
	return toChargeResult("void", key, reply)
}

// toChargeResult accepts only the three definite statuses.
func toChargeResult(op, key string, reply chargeReply) (booking.ChargeResult, error) {
	switch booking.ChargeStatus(reply.Status) {
	case booking.ChargeSucceeded, booking.ChargeDeclined, booking.ChargeVoided:
		return booking.ChargeResult{ChargeID: reply.ChargeID, Status: booking.ChargeStatus(reply.Status)}, nil
	default:
		// "refunded" (the charge was taken and later refunded) or anything
		// unexpected: not a clean answer to "did this charge succeed?".
		// Treat it as no answer; callers keep the payment PENDING rather
		// than guess.
		return booking.ChargeResult{}, fmt.Errorf("%s %s: %w: unexpected status %q",
			op, key, booking.ErrGatewayNoAnswer, reply.Status)
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

// post sends a JSON body and decodes a 200 JSON reply into out.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	_, err := c.do(ctx, http.MethodPost, path, body, out)
	return err
}

// do sends a request (with a JSON body unless body is nil) and decodes a 200
// JSON reply into out. Statuses listed in alsoOK are returned without error
// and without decoding (e.g. 404 for Lookup). Every other failure (network,
// timeout, other status, bad JSON) wraps booking.ErrGatewayNoAnswer.
func (c *Client) do(ctx context.Context, method, path string, body, out any, alsoOK ...int) (int, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode request: %w", err) // a programming bug, not a gateway answer
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Timeout, connection refused, connection reset, ...
		return 0, fmt.Errorf("%w: %w", booking.ErrGatewayNoAnswer, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("%w: read response: %w", booking.ErrGatewayNoAnswer, err)
	}
	for _, ok := range alsoOK {
		if resp.StatusCode == ok {
			return resp.StatusCode, nil
		}
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("%w: HTTP %d: %s", booking.ErrGatewayNoAnswer, resp.StatusCode, bytes.TrimSpace(data))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return resp.StatusCode, fmt.Errorf("%w: decode response: %w", booking.ErrGatewayNoAnswer, err)
	}
	return resp.StatusCode, nil
}
