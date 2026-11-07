// Package paymentmock is a tiny fake payment gateway, used by demos
// (cmd/payment, a separate binary and Compose service) and by integration
// tests (in-process, through httptest).
//
// Where it fits:
//
//	POST /v1/bookings -> booking service -> payment client (internal/payment) -> THIS mock
//
// It behaves like a real gateway in the one way that matters most:
// IDEMPOTENCY. A charge is identified by the idempotency key the caller
// sends. Sending the same key again returns the SAME stored result and never
// charges a second time. That is what makes it safe for SeatLock to retry a
// charge whose answer was lost.
//
// State is kept in memory only. The /debug/* endpoints exist only here,
// never in the SeatLock API.
package paymentmock

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Behaviours a charge can be forced into, via the X-Mock-Behavior header or
// the queue filled by POST /debug/next-behavior.
const (
	BehaviorOK      = "ok"      // succeed
	BehaviorDecline = "decline" // card declined
	BehaviorSlow    = "slow"    // succeed, but answer after SlowDelay
	// BehaviorTimeout records the charge as SUCCEEDED and then never answers.
	// The client times out. This simulates the most dangerous real-world
	// case: the money was taken, but the answer was lost.
	BehaviorTimeout = "timeout"
	// BehaviorRefundFail makes the next refund fail with HTTP 500 (queue it
	// with POST /debug/next-behavior), to demonstrate outbox retries.
	BehaviorRefundFail = "refund_fail"
)

// Charge statuses.
const (
	StatusSucceeded = "succeeded"
	StatusDeclined  = "declined"
	StatusRefunded  = "refunded"
	// StatusVoided: the key was closed with POST /charges/{key}/void before
	// any charge happened. Any later charge with that key is refused: no
	// money is ever taken for it.
	StatusVoided = "voided"
)

// Options controls random behaviour (for demos) and the slow delay.
type Options struct {
	FailRate  float64       // probability (0..1) that an unforced charge is declined
	MinDelay  time.Duration // random delay before answering an unforced charge
	MaxDelay  time.Duration
	SlowDelay time.Duration // how long a "slow" charge takes (default 3s)
	// TimeoutHold caps how long a "timeout" request waits before giving up
	// (the client normally disconnects long before). Default 2 minutes.
	TimeoutHold time.Duration
	Logger      *slog.Logger
}

// ChargeRequest is the body of POST /charges.
type ChargeRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	AmountPaise    int64  `json:"amount_paise"`
	Reference      string `json:"reference"`
}

// ChargeResponse is returned by POST /charges and GET /charges/{key}.
type ChargeResponse struct {
	ChargeID       string `json:"charge_id"`
	Status         string `json:"status"`
	IdempotencyKey string `json:"idempotency_key"`
	AmountPaise    int64  `json:"amount_paise"`
}

// RefundRequest is the body of POST /refunds.
type RefundRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	ChargeID       string `json:"charge_id"`
}

// RefundResponse is returned by POST /refunds.
type RefundResponse struct {
	RefundID string `json:"refund_id"`
	ChargeID string `json:"charge_id"`
	Status   string `json:"status"`
}

// Stats is returned by GET /debug/stats. ChargesCreated counts how many
// charges were REALLY created per idempotency key (must never exceed 1);
// ChargeRequests counts every request, including replays.
type Stats struct {
	ChargesCreated map[string]int `json:"charges_created"`
	ChargeRequests map[string]int `json:"charge_requests"`
	RefundsCreated map[string]int `json:"refunds_created"` // per charge idempotency key
	VoidedKeys     map[string]int `json:"voided_keys"`
	TotalCharges   int            `json:"total_charges"`       // every charge created, declined included (voided keys excluded)
	MoneyTaken     int            `json:"money_taken_charges"` // charges that took money (succeeded or later refunded)
	TotalRefunds   int            `json:"total_refunds"`
	TotalVoids     int            `json:"total_voids"`
}

type charge struct {
	ChargeResponse
	refundID string
}

// Server is the mock gateway. Create it with New and use it as an http.Handler.
type Server struct {
	opts Options
	mux  *http.ServeMux

	// mu protects everything below. Handlers run on many goroutines.
	mu             sync.Mutex
	chargesByKey   map[string]*charge
	chargesByID    map[string]*charge
	refundsByKey   map[string]RefundResponse
	chargeRequests map[string]int
	queue          []string // behaviours for the next charges, oldest first
	refundFailures int      // how many upcoming refunds must fail (refund_fail)
	// slowUntil/slowDelay: a TIMED slowdown (POST /debug/slow), used by the
	// chaos tests: until slowUntil, every new unforced charge waits slowDelay.
	// Unlike restarting the mock with a new PAYMENT_MIN_DELAY, it keeps the
	// in-memory charges and stats (the evidence for "no double charges").
	slowUntil time.Time
	slowDelay time.Duration
	nextID    int
	rng       *rand.Rand
}

// New creates a mock gateway.
func New(opts Options) *Server {
	if opts.SlowDelay == 0 {
		opts.SlowDelay = 3 * time.Second
	}
	if opts.TimeoutHold == 0 {
		opts.TimeoutHold = 2 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{opts: opts, rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
	s.resetLocked()

	s.mux = http.NewServeMux()
	s.mux.HandleFunc("POST /charges", s.createCharge)
	s.mux.HandleFunc("GET /charges/{key}", s.getCharge)
	s.mux.HandleFunc("POST /charges/{key}/void", s.voidCharge)
	s.mux.HandleFunc("POST /refunds", s.createRefund)
	s.mux.HandleFunc("POST /debug/next-behavior", s.queueBehavior)
	s.mux.HandleFunc("POST /debug/slow", s.slowFor)
	s.mux.HandleFunc("POST /debug/reset", s.reset)
	s.mux.HandleFunc("GET /debug/stats", s.stats)
	s.mux.Handle("GET /metrics", s.metricsHandler())
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return s
}

// ServeHTTP makes Server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// resetLocked clears all state. Callers must hold s.mu (or be in New).
func (s *Server) resetLocked() {
	s.chargesByKey = map[string]*charge{}
	s.chargesByID = map[string]*charge{}
	s.refundsByKey = map[string]RefundResponse{}
	s.chargeRequests = map[string]int{}
	s.queue = nil
	s.refundFailures = 0
	s.slowUntil = time.Time{}
	s.slowDelay = 0
}

func (s *Server) createCharge(w http.ResponseWriter, r *http.Request) {
	var req ChargeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IdempotencyKey == "" || req.AmountPaise <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "idempotency_key and a positive amount_paise are required"})
		return
	}

	s.mu.Lock()
	s.chargeRequests[req.IdempotencyKey]++

	// IDEMPOTENCY: a key we have seen before gets the stored result back,
	// immediately, with no second charge, whatever behaviour is queued.
	if existing, ok := s.chargesByKey[req.IdempotencyKey]; ok {
		s.mu.Unlock()
		if existing.Status == StatusVoided {
			// The key was closed before any charge happened (a late request
			// arriving after reconciliation). Refuse it: no money is taken.
			writeJSON(w, http.StatusOK, existing.ChargeResponse)
			return
		}
		if existing.AmountPaise != req.AmountPaise {
			// Same key, different request: a client bug. Real gateways refuse it.
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "idempotency key reused with a different amount"})
			return
		}
		writeJSON(w, http.StatusOK, existing.ChargeResponse)
		return
	}

	behavior := s.pickBehaviorLocked(r.Header.Get("X-Mock-Behavior"))
	s.nextID++
	c := &charge{ChargeResponse: ChargeResponse{
		ChargeID:       fmt.Sprintf("ch_%d", s.nextID),
		Status:         StatusSucceeded,
		IdempotencyKey: req.IdempotencyKey,
		AmountPaise:    req.AmountPaise,
	}}
	if behavior == BehaviorDecline {
		c.Status = StatusDeclined
	}
	// The charge is recorded BEFORE any delay, like a real gateway that
	// takes the money and only then sends the (possibly lost) answer.
	s.chargesByKey[req.IdempotencyKey] = c
	s.chargesByID[c.ChargeID] = c
	delay := s.randomDelayLocked()
	s.mu.Unlock()

	s.opts.Logger.Info("charge", "key", req.IdempotencyKey, "charge_id", c.ChargeID, "status", c.Status, "behavior", behavior)

	switch behavior {
	case BehaviorTimeout:
		// Never answer. Wait until the client gives up (its context is
		// cancelled when it disconnects) or TimeoutHold passes.
		select {
		case <-r.Context().Done():
		case <-time.After(s.opts.TimeoutHold):
		}
		return
	case BehaviorSlow:
		delay = s.opts.SlowDelay
	case "":
		// random behaviour: keep the random delay
	default:
		delay = 0
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	writeJSON(w, http.StatusOK, c.ChargeResponse)
}

// pickBehaviorLocked chooses the behaviour for a NEW charge: the header wins,
// then the queue, then random (decline with probability FailRate).
// It returns "" for a random success, so the random delay applies.
func (s *Server) pickBehaviorLocked(header string) string {
	switch {
	case header != "":
		return header
	case len(s.queue) > 0:
		b := s.queue[0]
		s.queue = s.queue[1:]
		return b
	case s.opts.FailRate > 0 && s.rng.Float64() < s.opts.FailRate:
		return BehaviorDecline
	default:
		return ""
	}
}

func (s *Server) randomDelayLocked() time.Duration {
	if s.slowDelay > 0 && time.Now().Before(s.slowUntil) {
		return s.slowDelay
	}
	if s.opts.MaxDelay <= s.opts.MinDelay {
		return s.opts.MinDelay
	}
	return s.opts.MinDelay + time.Duration(s.rng.Int63n(int64(s.opts.MaxDelay-s.opts.MinDelay)))
}

func (s *Server) getCharge(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	c, ok := s.chargesByKey[r.PathValue("key")]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "charge not found"})
		return
	}
	writeJSON(w, http.StatusOK, c.ChargeResponse)
}

// voidCharge handles POST /charges/{key}/void: "close this key".
//
// If no charge exists for the key, the key is recorded as VOIDED: any charge
// that arrives later with it is refused (no money taken). If a charge already
// exists, nothing changes and its real result is returned, e.g. "succeeded"
// when a slow request reached the gateway just before the void.
func (s *Server) voidCharge(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	s.mu.Lock()
	c, ok := s.chargesByKey[key]
	if !ok {
		c = &charge{ChargeResponse: ChargeResponse{Status: StatusVoided, IdempotencyKey: key}}
		s.chargesByKey[key] = c
	}
	resp := c.ChargeResponse
	s.mu.Unlock()
	s.opts.Logger.Info("void", "key", key, "status", resp.Status, "was_new", !ok)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) createRefund(w http.ResponseWriter, r *http.Request) {
	var req RefundRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IdempotencyKey == "" || req.ChargeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "idempotency_key and charge_id are required"})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.refundFailures > 0 {
		s.refundFailures--
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "simulated refund failure"})
		return
	}

	// Same refund key -> same result.
	if existing, ok := s.refundsByKey[req.IdempotencyKey]; ok {
		writeJSON(w, http.StatusOK, existing)
		return
	}
	c, ok := s.chargesByID[req.ChargeID]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "charge not found"})
		return
	}
	if c.Status == StatusDeclined || c.Status == StatusVoided {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no money was taken for this charge"})
		return
	}
	// A charge is refunded at most once, even with a different refund key.
	if c.refundID == "" {
		s.nextID++
		c.refundID = fmt.Sprintf("re_%d", s.nextID)
		c.Status = StatusRefunded
	}
	resp := RefundResponse{RefundID: c.refundID, ChargeID: c.ChargeID, Status: StatusRefunded}
	s.refundsByKey[req.IdempotencyKey] = resp
	s.opts.Logger.Info("refund", "key", req.IdempotencyKey, "charge_id", c.ChargeID, "refund_id", c.refundID)
	writeJSON(w, http.StatusOK, resp)
}

// queueBehavior handles POST /debug/next-behavior {"behavior":"timeout","count":1}.
func (s *Server) queueBehavior(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Behavior string `json:"behavior"`
		Count    int    `json:"count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	switch req.Behavior {
	case BehaviorOK, BehaviorDecline, BehaviorSlow, BehaviorTimeout, BehaviorRefundFail:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "behavior must be ok, decline, slow, timeout or refund_fail"})
		return
	}
	if req.Count <= 0 {
		req.Count = 1
	}
	s.mu.Lock()
	if req.Behavior == BehaviorRefundFail {
		s.refundFailures += req.Count // refunds have their own counter
	} else {
		for i := 0; i < req.Count; i++ {
			s.queue = append(s.queue, req.Behavior)
		}
	}
	queued := len(s.queue) + s.refundFailures
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]int{"queued": queued})
}

// slowFor handles POST /debug/slow {"delay":"3s","for":"60s"}: for the next
// "for", every new unforced charge answers after "delay" (the charge itself
// is still recorded first, like a slow real gateway). {"delay":"0s"} ends it.
func (s *Server) slowFor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Delay string `json:"delay"`
		For   string `json:"for"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	delay, err1 := time.ParseDuration(req.Delay)
	length, err2 := time.ParseDuration(req.For)
	if err1 != nil || delay < 0 || (delay > 0 && (err2 != nil || length <= 0)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `want {"delay":"3s","for":"60s"} (delay "0s" ends a slowdown)`})
		return
	}
	s.mu.Lock()
	s.slowDelay = delay
	s.slowUntil = time.Now().Add(length)
	until := s.slowUntil
	s.mu.Unlock()
	if delay == 0 {
		writeJSON(w, http.StatusOK, map[string]string{"slow": "off"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"delay": delay.String(), "until": until.UTC().Format(time.RFC3339)})
}

// reset handles POST /debug/reset: forget all charges, refunds, stats and
// queued behaviours, so nothing leaks from one test or demo into the next.
func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.resetLocked()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Stats())
}

// Stats returns a snapshot of the counters (also used directly by tests).
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{
		ChargesCreated: map[string]int{},
		ChargeRequests: map[string]int{},
		RefundsCreated: map[string]int{},
		VoidedKeys:     map[string]int{},
	}
	for key, c := range s.chargesByKey {
		if c.Status == StatusVoided {
			st.VoidedKeys[key] = 1
			st.TotalVoids++
			continue // a voided key never took money
		}
		st.ChargesCreated[key] = 1 // one map entry = one real charge
		st.TotalCharges++
		if c.Status != StatusDeclined {
			st.MoneyTaken++
		}
		if c.refundID != "" {
			st.RefundsCreated[key] = 1
			st.TotalRefunds++
		}
	}
	for key, n := range s.chargeRequests {
		st.ChargeRequests[key] = n
	}
	return st
}

// metricsHandler exposes the mock's counters to Prometheus (GaugeFuncs read
// the in-memory stats at scrape time; gauges, not counters, because
// /debug/reset sets them back to 0). The dashboard shows them next to
// SeatLock's own gateway metrics, so "SeatLock thinks" and "the gateway saw"
// can be compared.
func (s *Server) metricsHandler() http.Handler {
	reg := prometheus.NewRegistry()
	for name, help := range map[string]string{
		"seatlock_mock_gateway_charges":     "Charges recorded by the mock gateway (declined included).",
		"seatlock_mock_gateway_money_taken": "Charges that took money (succeeded or later refunded).",
		"seatlock_mock_gateway_refunds":     "Refunds paid by the mock gateway.",
		"seatlock_mock_gateway_voids":       "Keys voided at the mock gateway.",
	} {
		name := name
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, func() float64 {
			st := s.Stats()
			switch name {
			case "seatlock_mock_gateway_charges":
				return float64(st.TotalCharges)
			case "seatlock_mock_gateway_money_taken":
				return float64(st.MoneyTaken)
			case "seatlock_mock_gateway_refunds":
				return float64(st.TotalRefunds)
			default:
				return float64(st.TotalVoids)
			}
		}))
	}
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
