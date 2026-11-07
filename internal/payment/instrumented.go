// instrumented.go wraps any booking.PaymentGateway and records every call in
// the metrics: seatlock_payment_gateway_requests_total{op,result} and
// seatlock_payment_gateway_duration_seconds{op}.
//
// Where it fits (wired in cmd/api and cmd/worker):
//
//	booking.Service -> Instrumented (metrics) -> Client (HTTP) -> gateway
//
// A decorator keeps the HTTP client free of metrics code, and the service
// unaware of them: neither changes, and tests can use either one alone.
package payment

import (
	"context"
	"errors"
	"time"

	"github.com/shreyas0917/seatlock/internal/booking"
	"github.com/shreyas0917/seatlock/internal/metrics"
)

// Instrumented is a PaymentGateway that records metrics around another one.
type Instrumented struct {
	next    booking.PaymentGateway
	metrics *metrics.Metrics
}

// Compile-time check.
var _ booking.PaymentGateway = (*Instrumented)(nil)

// NewInstrumented wraps next.
func NewInstrumented(next booking.PaymentGateway, m *metrics.Metrics) *Instrumented {
	return &Instrumented{next: next, metrics: m}
}

func (g *Instrumented) Charge(ctx context.Context, req booking.ChargeRequest) (booking.ChargeResult, error) {
	start := time.Now()
	res, err := g.next.Charge(ctx, req)
	g.record("charge", start, chargeResult(res, err))
	return res, err
}

func (g *Instrumented) Refund(ctx context.Context, req booking.RefundRequest) (booking.RefundResult, error) {
	start := time.Now()
	res, err := g.next.Refund(ctx, req)
	g.record("refund", start, chargeResult(booking.ChargeResult{Status: booking.ChargeSucceeded}, err))
	return res, err
}

func (g *Instrumented) Lookup(ctx context.Context, key string) (booking.ChargeResult, error) {
	start := time.Now()
	res, err := g.next.Lookup(ctx, key)
	g.record("lookup", start, chargeResult(res, err))
	return res, err
}

func (g *Instrumented) Void(ctx context.Context, key string) (booking.ChargeResult, error) {
	start := time.Now()
	res, err := g.next.Void(ctx, key)
	g.record("void", start, chargeResult(res, err))
	return res, err
}

func (g *Instrumented) record(op string, start time.Time, result string) {
	g.metrics.GatewayRequests.WithLabelValues(op, result).Inc()
	g.metrics.GatewayDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
}

// chargeResult maps a call's outcome to the fixed set: ok, declined, voided,
// not_found, no_answer, error.
func chargeResult(res booking.ChargeResult, err error) string {
	switch {
	case errors.Is(err, booking.ErrChargeNotFound):
		return "not_found"
	case errors.Is(err, booking.ErrGatewayNoAnswer):
		return "no_answer"
	case err != nil:
		return "error"
	case res.Status == booking.ChargeDeclined:
		return "declined"
	case res.Status == booking.ChargeVoided:
		return "voided"
	default:
		return "ok"
	}
}
