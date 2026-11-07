// gateway.go defines the PaymentGateway interface: how the booking service
// charges and refunds money.
//
// Where it fits:
//
//	POST /v1/bookings -> Service.ConfirmBooking -> PaymentGateway.Charge -> payment provider
//
// The real implementation is internal/payment.Client (HTTP). Like
// SeatLocker, the interface lives HERE, in the package that uses it, so unit
// tests can plug in a fake gateway that declines, times out or succeeds on
// demand.
package booking

import (
	"context"
	"errors"
)

// ChargeStatus is the gateway's final answer about a charge.
type ChargeStatus string

const (
	ChargeSucceeded ChargeStatus = "succeeded" // the money was taken
	ChargeDeclined  ChargeStatus = "declined"  // no money was taken
	// ChargeVoided: the key was closed at the gateway (by the reconciler)
	// before any charge happened. No money was taken, and none ever will be
	// for this key.
	ChargeVoided ChargeStatus = "voided"
)

// ChargeRequest asks the gateway to take money.
type ChargeRequest struct {
	// IdempotencyKey identifies THIS payment at the gateway (payments.charge_key).
	// Sending the same key again must return the stored result, never a
	// second charge. That property is what makes every retry safe.
	IdempotencyKey string
	AmountPaise    int64
	Reference      string // free text for the gateway, e.g. "hold-12"
}

// ChargeResult is a definite answer from the gateway.
type ChargeResult struct {
	ChargeID string
	Status   ChargeStatus
}

// RefundRequest asks the gateway to give a charge's money back.
type RefundRequest struct {
	IdempotencyKey string // e.g. "refund-<payment_id>": the same refund is never paid twice
	ChargeID       string
}

// RefundResult is a definite refund answer.
type RefundResult struct {
	RefundID string
}

// ErrGatewayNoAnswer means the gateway gave no usable answer: a timeout, a
// dropped connection, a 5xx, or an unreadable response.
//
// THIS IS NOT "FAILED". For a charge it means the outcome is UNKNOWN: the
// gateway may have taken the money and then the answer got lost. The only
// safe reaction is to keep the payment PENDING and ask again later with the
// SAME idempotency key (the gateway then returns what really happened).
var ErrGatewayNoAnswer = errors.New("payment gateway gave no answer")

// ErrChargeNotFound is a DEFINITE answer from Lookup: the gateway has no
// charge for this key (the request probably never reached it).
var ErrChargeNotFound = errors.New("no charge for this key at the gateway")

// PaymentGateway charges and refunds money.
//
// Contract: Charge, Lookup and Void return a ChargeResult (succeeded,
// declined or voided) ONLY when the gateway gave a definite answer; anything
// else is an error wrapping ErrGatewayNoAnswer. Lookup returns
// ErrChargeNotFound when the gateway definitely has no charge for the key.
type PaymentGateway interface {
	Charge(ctx context.Context, req ChargeRequest) (ChargeResult, error)
	Refund(ctx context.Context, req RefundRequest) (RefundResult, error)
	// Lookup asks what happened to a charge key (GET /charges/{key}).
	Lookup(ctx context.Context, key string) (ChargeResult, error)
	// Void closes an unused key so no later charge with it can take money.
	// If a charge already exists, its real result is returned instead.
	Void(ctx context.Context, key string) (ChargeResult, error)
}
