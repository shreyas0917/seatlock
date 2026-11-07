// Package metrics defines EVERY Prometheus metric SeatLock exports, in one
// place, with a comment per metric: what it means, its labels, and which
// dashboard panel or alert uses it.
//
// Where it fits:
//
//	httpapi middleware ----\
//	booking.Service --------+--> *metrics.Metrics --> private registry --> /metrics (internal port)
//	lock / payment hooks ---|                                               ^
//	worker jobs ------------/                               Prometheus scrapes it every 5s
//
// Rules followed by every metric here:
//
//  1. LOW CARDINALITY. Every distinct combination of label values is a
//     separate time series that Prometheus stores in memory. A label like
//     user_id (one value per user) or a raw URL path (/v1/holds/1,
//     /v1/holds/2, ...) creates a new series per user or per id, i.e.
//     unbounded growth: 100k users x 8 routes x 5 statuses = 4 million
//     series, enough to take Prometheus down. So labels only take values
//     from small, FIXED sets: route PATTERNS (/v1/holds/{holdID}), methods,
//     status codes, and outcome names defined in code.
//  2. No label is called "job" or "instance": Prometheus adds those itself
//     (the scrape target) and would rename ours to "exported_job", breaking
//     every query that joins on it. Worker jobs use "worker_job".
//  3. A private registry (not the global default one), so nothing is
//     exported by accident and tests get a clean, isolated set.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// latencyBuckets fit request latencies from 5ms to 5s: fine-grained where
// healthy requests land (5-250ms), coarser where only trouble lands.
var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// Metrics holds all SeatLock metrics. Use NewAPI or NewWorker; each registers
// only what that process produces, so neither exports series that would sit
// at a misleading 0.
type Metrics struct {
	Registry *prometheus.Registry

	// ---------------------------------------------------------------- HTTP (API)

	// HTTPRequests counts finished requests. Labels: route (chi pattern, or
	// "unmatched"), method, status (code as text). Panels: RPS by route and
	// status. Alert: HighErrorRate (5xx ratio).
	HTTPRequests *prometheus.CounterVec
	// HTTPDuration is request latency. Labels: route, method. Panels:
	// p50/p95/p99. Alert: SlowHolds (p95 of POST /v1/holds).
	HTTPDuration *prometheus.HistogramVec
	// HTTPInFlight is the number of requests being served right now.
	HTTPInFlight prometheus.Gauge

	// ------------------------------------------------------------ booking domain

	// Holds counts POST /v1/holds outcomes. Label: outcome (created, reused,
	// seat_held, already_booked, lock_unavailable, payment_in_progress,
	// error). Panel: holds by outcome. Alert: RedisLockUnavailable
	// (lock_unavailable is the FAIL-CLOSED signal).
	Holds *prometheus.CounterVec
	// Confirms counts POST /v1/bookings outcomes as decided by the service.
	// Label: outcome (booked, declined, pending, not_completed,
	// expired_refunded, expired_refund_pending, already_used, error).
	// Panel: confirms by outcome.
	Confirms *prometheus.CounterVec
	// Idempotency counts what the idempotency layer did with a confirm.
	// Label: result (new, replay, in_progress, reused_key, takeover).
	// Panel: idempotency results.
	Idempotency *prometheus.CounterVec
	// GatewayRequests counts payment gateway calls. Labels: op (charge,
	// refund, lookup, void), result (ok, declined, voided, not_found,
	// no_answer). Panel: gateway results.
	GatewayRequests *prometheus.CounterVec
	// GatewayDuration is gateway call latency. Label: op. Panel: gateway latency.
	GatewayDuration *prometheus.HistogramVec
	// RedisLockOps counts seat-lock operations. Labels: op (acquire,
	// refresh, release), result (ok, held, error). Panel: Redis lock ops;
	// result="error" means Redis could not be asked (fail closed).
	RedisLockOps *prometheus.CounterVec

	// -------------------------------------------------------------------- worker

	// WorkerRuns counts job runs. Labels: worker_job, result (ok, error).
	// Panel: job runs.
	WorkerRuns *prometheus.CounterVec
	// WorkerDuration is how long one job run took. Label: worker_job.
	WorkerDuration *prometheus.HistogramVec
	// WorkerRows counts rows a job finished, by outcome. Labels: worker_job,
	// outcome (e.g. reconcile: booked, refund_required, declined, abandoned,
	// still_pending; outbox: done, retry, failed). Panel: reconcile outcomes.
	WorkerRows *prometheus.CounterVec
	// WorkerLastSuccess is the Unix time of the last successful run. Label:
	// worker_job. Alert: WorkerJobStalled.
	WorkerLastSuccess *prometheus.GaugeVec
	// WorkerInterval is each job's configured interval in seconds. Label:
	// worker_job. It lets WorkerJobStalled compare "time since last success"
	// with "5 x this job's own interval" in ONE rule that cannot drift
	// from the config.
	WorkerInterval *prometheus.GaugeVec

	// ---------------------------------------------- invariants (verify.sql, live)

	// InvariantViolations is the number of rows each verify.sql check finds.
	// Label: check (fixed list in internal/invariants). Must ALWAYS be 0.
	// Panels: the Correctness row. Alert: DoubleBookingDetected (any > 0).
	InvariantViolations *prometheus.GaugeVec
	// InvariantCheckErrors counts check queries that failed or timed out.
	// Label: check. A failed check does NOT reset its gauge to 0: "could not
	// check" must never look like "healthy".
	InvariantCheckErrors *prometheus.CounterVec
	// InvariantLastSuccess is the Unix time each check last ran successfully.
	// Label: check. Alert: InvariantChecksNotRunning.
	InvariantLastSuccess *prometheus.GaugeVec
	// PaymentsPendingOldestAge is the age in seconds of the oldest PENDING
	// payment (0 if none). Panel: oldest PENDING. Alert: PaymentsStuckPending.
	PaymentsPendingOldestAge prometheus.Gauge
	// OutboxRows is the number of outbox rows by status (PENDING, FAILED).
	// Panels: outbox backlog. Alert: OutboxDeadLetters (FAILED > 0).
	OutboxRows *prometheus.GaugeVec
	// OutboxOldestPendingAge is the age in seconds of the oldest PENDING
	// outbox row (0 if none). Panel: outbox age.
	OutboxOldestPendingAge prometheus.Gauge
	// HoldsActive is the number of ACTIVE holds. Panel: active holds.
	HoldsActive prometheus.Gauge
}

// newMetrics creates every metric (unregistered).
func newMetrics() *Metrics {
	return &Metrics{
		Registry: prometheus.NewRegistry(),

		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "seatlock_http_requests_total", Help: "HTTP requests by route pattern, method and status.",
		}, []string{"route", "method", "status"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "seatlock_http_request_duration_seconds", Help: "HTTP request latency by route pattern and method.",
			Buckets: latencyBuckets,
		}, []string{"route", "method"}),
		HTTPInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "seatlock_http_in_flight_requests", Help: "HTTP requests being served right now.",
		}),

		Holds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "seatlock_holds_total", Help: "Hold attempts by outcome.",
		}, []string{"outcome"}),
		Confirms: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "seatlock_confirms_total", Help: "Confirm (pay + book) attempts by outcome.",
		}, []string{"outcome"}),
		Idempotency: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "seatlock_idempotency_total", Help: "Idempotency-Key handling results for confirms.",
		}, []string{"result"}),
		GatewayRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "seatlock_payment_gateway_requests_total", Help: "Payment gateway calls by operation and result.",
		}, []string{"op", "result"}),
		GatewayDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "seatlock_payment_gateway_duration_seconds", Help: "Payment gateway call latency by operation.",
			Buckets: latencyBuckets,
		}, []string{"op"}),
		RedisLockOps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "seatlock_redis_lock_ops_total", Help: "Redis seat-lock operations by op and result (error = fail closed).",
		}, []string{"op", "result"}),

		WorkerRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "seatlock_worker_job_runs_total", Help: "Worker job runs by job and result.",
		}, []string{"worker_job", "result"}),
		WorkerDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "seatlock_worker_job_duration_seconds", Help: "Duration of one worker job run.",
			Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60},
		}, []string{"worker_job"}),
		WorkerRows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "seatlock_worker_rows_total", Help: "Rows processed by worker jobs, by outcome.",
		}, []string{"worker_job", "outcome"}),
		WorkerLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "seatlock_worker_last_success_timestamp_seconds", Help: "Unix time of each job's last successful run.",
		}, []string{"worker_job"}),
		WorkerInterval: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "seatlock_worker_job_interval_seconds", Help: "Configured run interval of each worker job.",
		}, []string{"worker_job"}),

		InvariantViolations: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "seatlock_invariant_violations", Help: "Rows violating each verify.sql invariant. Must be 0.",
		}, []string{"check"}),
		InvariantCheckErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "seatlock_invariant_check_errors_total", Help: "Invariant check queries that failed or timed out.",
		}, []string{"check"}),
		InvariantLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "seatlock_invariant_last_success_timestamp_seconds", Help: "Unix time each invariant check last succeeded.",
		}, []string{"check"}),
		PaymentsPendingOldestAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "seatlock_payments_pending_oldest_age_seconds", Help: "Age of the oldest PENDING payment (0 if none).",
		}),
		OutboxRows: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "seatlock_outbox_rows", Help: "Outbox rows by status (PENDING, FAILED).",
		}, []string{"status"}),
		OutboxOldestPendingAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "seatlock_outbox_oldest_pending_age_seconds", Help: "Age of the oldest PENDING outbox row (0 if none).",
		}),
		HoldsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "seatlock_holds_active", Help: "Number of ACTIVE holds.",
		}),
	}
}

// registerRuntime adds the standard Go runtime (goroutines, GC, memory) and
// process (CPU, open files, RSS) collectors.
func (m *Metrics) registerRuntime() {
	m.Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// NewAPI returns the metrics the API process exports: HTTP, booking domain,
// gateway, Redis lock, plus runtime. (Pool collectors are added with
// RegisterDBPool / RegisterRedisPool once the pools exist.)
func NewAPI() *Metrics {
	m := newMetrics()
	m.registerRuntime()
	m.Registry.MustRegister(
		m.HTTPRequests, m.HTTPDuration, m.HTTPInFlight,
		m.Holds, m.Confirms, m.Idempotency,
		m.GatewayRequests, m.GatewayDuration, m.RedisLockOps,
	)
	return m
}

// NewWorker returns the metrics the worker process exports: worker jobs,
// invariants, and the gateway / Redis calls the jobs make, plus runtime.
func NewWorker() *Metrics {
	m := newMetrics()
	m.registerRuntime()
	m.Registry.MustRegister(
		m.WorkerRuns, m.WorkerDuration, m.WorkerRows, m.WorkerLastSuccess, m.WorkerInterval,
		m.InvariantViolations, m.InvariantCheckErrors, m.InvariantLastSuccess,
		m.PaymentsPendingOldestAge, m.OutboxRows, m.OutboxOldestPendingAge, m.HoldsActive,
		m.GatewayRequests, m.GatewayDuration, m.RedisLockOps,
	)
	return m
}

// Handler serves this registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// ------------------------------------------------- adapters for other packages
// These small methods let other packages report into the metrics without
// importing Prometheus types.

// HoldOutcome implements booking.Observer.
func (m *Metrics) HoldOutcome(outcome string) { m.Holds.WithLabelValues(outcome).Inc() }

// ConfirmOutcome implements booking.Observer.
func (m *Metrics) ConfirmOutcome(outcome string) { m.Confirms.WithLabelValues(outcome).Inc() }

// ObserveLockOp is the lock.Options.Observe callback (op: acquire, refresh,
// release; result: ok, held, error).
func (m *Metrics) ObserveLockOp(op, result string) { m.RedisLockOps.WithLabelValues(op, result).Inc() }

// IdempotencyResult records what the idempotency layer did with a confirm.
func (m *Metrics) IdempotencyResult(result string) { m.Idempotency.WithLabelValues(result).Inc() }
