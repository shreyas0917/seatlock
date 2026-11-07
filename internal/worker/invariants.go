// invariants.go is the worker job that turns demo/verify.sql into LIVE
// metrics: every 15s it runs every invariant check (internal/invariants) and
// exports seatlock_invariant_violations{check}, plus a few health gauges
// (oldest PENDING payment, outbox backlog, active holds).
//
// Why invariants as metrics: a double booking that only shows up in a log
// line or a manual SQL query is noticed late, or never. As a gauge, it fires
// the DoubleBookingDetected alert within seconds, and the dashboard's
// Correctness row shows it in red. It is the strongest kind of monitoring:
// it watches the RESULT users care about ("did anyone pay without a seat?"),
// not just the machinery.
//
// NEVER "FALSE GREEN": if a check cannot run (query error, statement
// timeout), its gauge is NOT set to 0; it keeps its last value, and
// seatlock_invariant_check_errors_total{check} goes up. "Could not check"
// must never look like "healthy". InvariantChecksNotRunning fires when a
// check has not succeeded for 2 minutes.
package worker

import (
	"context"
	"log/slog"

	"github.com/shreyas0917/seatlock/internal/invariants"
	"github.com/shreyas0917/seatlock/internal/metrics"
)

// InvariantChecker runs the checks and records the results.
type InvariantChecker struct {
	Store   *invariants.Store
	Metrics *metrics.Metrics
	Logger  *slog.Logger
}

func (j *InvariantChecker) Name() string { return "invariants" }

// Init registers every check's series before the first run: the error
// counters at 0 (so rate() works), and last-success at 0 ("never"), so
// InvariantChecksNotRunning also fires if the checks NEVER succeed. The
// violation gauges are deliberately not created until a check really ran:
// "no data" must never be shown as a green 0.
func (j *InvariantChecker) Init() {
	for _, c := range j.Store.Checks {
		j.Metrics.InvariantCheckErrors.WithLabelValues(c.Name)
		j.Metrics.InvariantLastSuccess.WithLabelValues(c.Name).Set(0)
	}
}

func (j *InvariantChecker) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	for _, r := range j.Store.Run(ctx) {
		res.Claimed++
		if r.Err != nil {
			// Keep the last value; count the error.
			res.Failed++
			res.add("error")
			j.Metrics.InvariantCheckErrors.WithLabelValues(r.Check).Inc()
			j.Logger.Error("invariant check failed to run", "job", j.Name(), "check", r.Check, "error", r.Err.Error())
			continue
		}
		res.Succeeded++
		j.Metrics.InvariantViolations.WithLabelValues(r.Check).Set(float64(r.Violations))
		j.Metrics.InvariantLastSuccess.WithLabelValues(r.Check).SetToCurrentTime()
		if r.Violations > 0 {
			res.add("violation")
			j.Logger.Error("INVARIANT VIOLATED", "job", j.Name(), "check", r.Check, "rows", r.Violations)
		} else {
			res.add("ok")
		}
	}

	st, err := j.Store.Stats(ctx)
	if err != nil {
		// The checks above already ran; report the stats failure as a run
		// error (keeps last values, shows in WorkerRuns{result="error"}).
		return res, err
	}
	j.Metrics.PaymentsPendingOldestAge.Set(st.PendingPaymentOldestAge)
	j.Metrics.OutboxRows.WithLabelValues("PENDING").Set(st.OutboxPending)
	j.Metrics.OutboxRows.WithLabelValues("FAILED").Set(st.OutboxFailed)
	j.Metrics.OutboxOldestPendingAge.Set(st.OutboxOldestPendingAge)
	j.Metrics.HoldsActive.Set(st.HoldsActive)
	return res, nil
}
