// jobs.go defines the four background jobs. Each one claims a batch of rows
// (FOR UPDATE SKIP LOCKED, committed at once), then works on each row with
// NO transaction open, then records the result. The rules for what to do
// with a row live in booking.Service, not here.
//
//	payment reconciler : abandoned PENDING payments -> booked / refunded / declined / abandoned
//	outbox drainer     : PENDING REFUND and RELEASE_LOCK rows -> DONE (or FAILED dead letter)
//	hold expirer       : dead ACTIVE holds -> EXPIRED + RELEASE_LOCK outbox row
//	key cleanup        : old COMPLETED idempotency keys -> deleted
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/shreyas0917/seatlock/internal/booking"
)

// --- what the jobs need (small interfaces, satisfied by the real types) ---

// Store is the claiming side of booking.PostgresRepository.
type Store interface {
	ClaimPaymentsToReconcile(ctx context.Context, olderThan, lease time.Duration, limit int) ([]booking.Payment, error)
	ClaimOutbox(ctx context.Context, lease time.Duration, limit int) ([]booking.OutboxRow, error)
	FailClaimedOutbox(ctx context.Context, outboxID int64, failure string) (booking.OutboxStatus, error)
	ExpireHolds(ctx context.Context, limit int) ([]booking.ExpiredHold, error)
}

// Actions is the business side, satisfied by *booking.Service.
type Actions interface {
	ReconcilePayment(ctx context.Context, payment booking.Payment) (booking.ReconcileOutcome, error)
	ProcessOutboxRow(ctx context.Context, row booking.OutboxRow) error
}

// KeyStore deletes old idempotency keys, satisfied by *idempotency.Store.
type KeyStore interface {
	DeleteCompleted(ctx context.Context, retention time.Duration, limit int) (int64, error)
}

// ---------------------------------------------------------------- reconciler

// Reconciler finishes payments whose request was abandoned: the gateway call
// timed out (504) and the user never retried, so nobody knows whether money
// was taken. Without it, such a payment stays PENDING forever: the user may
// have paid with no seat and no refund, and the seat stays blocked.
type Reconciler struct {
	Store     Store
	Actions   Actions
	Logger    *slog.Logger
	OlderThan time.Duration // RECONCILE_AFTER
	Lease     time.Duration // RECONCILE_LEASE
	BatchSize int
}

func (j *Reconciler) Name() string { return "payment_reconciler" }

func (j *Reconciler) RunOnce(ctx context.Context) (Result, error) {
	payments, err := j.Store.ClaimPaymentsToReconcile(ctx, j.OlderThan, j.Lease, j.BatchSize)
	if err != nil {
		return Result{}, err
	}
	res := Result{Claimed: len(payments)}
	for _, p := range payments {
		start := time.Now()
		outcome, err := j.Actions.ReconcilePayment(ctx, p)
		attrs := []any{"job", j.Name(), "payment_id", p.ID, "charge_key", p.ChargeKey,
			"outcome", string(outcome), "duration_ms", time.Since(start).Milliseconds()}
		res.add(string(outcome))
		if err != nil {
			res.Failed++
			j.Logger.Warn("reconcile failed; will retry with backoff", append(attrs, "error", err.Error())...)
			continue
		}
		res.Succeeded++
		j.Logger.Info("payment reconciled", attrs...)
	}
	return res, nil
}

// ------------------------------------------------------------------- drainer

// OutboxDrainer performs the side effects recorded in the outbox: refunds
// owed to users, and Redis locks to release. Without it, a refund whose
// inline attempt failed is never paid, and a lock released while Redis was
// down lingers until its TTL.
type OutboxDrainer struct {
	Store     Store
	Actions   Actions
	Logger    *slog.Logger
	Lease     time.Duration // OUTBOX_LEASE
	BatchSize int
}

func (j *OutboxDrainer) Name() string { return "outbox_drainer" }

func (j *OutboxDrainer) RunOnce(ctx context.Context) (Result, error) {
	rows, err := j.Store.ClaimOutbox(ctx, j.Lease, j.BatchSize)
	if err != nil {
		return Result{}, err
	}
	res := Result{Claimed: len(rows)}
	for _, row := range rows {
		start := time.Now()
		err := j.Actions.ProcessOutboxRow(ctx, row)
		attrs := []any{"job", j.Name(), "outbox_id", row.ID, "kind", string(row.Kind),
			"attempt", row.Attempts, "duration_ms", time.Since(start).Milliseconds()}
		if err == nil {
			res.Succeeded++
			res.add("done")
			j.Logger.Info("outbox row done", append(attrs, "outcome", "done")...)
			continue
		}

		res.Failed++
		status, recErr := j.Store.FailClaimedOutbox(ctx, row.ID, err.Error())
		switch {
		case recErr != nil:
			res.add("retry")
			// Could not even record the failure: the claim lease expires and
			// the row is retried then.
			j.Logger.Error("could not record outbox failure",
				append(attrs, "error", err.Error(), "record_error", recErr.Error())...)
		case status == booking.OutboxFailed:
			res.add("failed")
			j.Logger.Error("outbox row dead-lettered after max attempts; requeue by hand with `make outbox-retry ID=<id>`",
				append(attrs, "outcome", "failed", "error", err.Error())...)
		default:
			res.add("retry")
			j.Logger.Warn("outbox row failed; will retry with backoff",
				append(attrs, "outcome", "retry", "error", err.Error())...)
		}
	}
	return res, nil
}

// ------------------------------------------------------------------- expirer

// HoldExpirer marks dead ACTIVE holds EXPIRED and queues their Redis lock
// release. Without it, an expired hold stays ACTIVE until someone else
// happens to try that seat, and its Redis lock lingers until its TTL.
type HoldExpirer struct {
	Store     Store
	Logger    *slog.Logger
	BatchSize int
}

func (j *HoldExpirer) Name() string { return "hold_expirer" }

func (j *HoldExpirer) RunOnce(ctx context.Context) (Result, error) {
	expired, err := j.Store.ExpireHolds(ctx, j.BatchSize)
	if err != nil {
		return Result{}, err
	}
	for _, h := range expired {
		j.Logger.Info("hold expired",
			"job", j.Name(), "hold_id", h.HoldID, "seat_id", h.SeatID, "outbox_id", h.OutboxID, "outcome", "expired")
	}
	res := Result{Claimed: len(expired), Succeeded: len(expired)}
	for range expired {
		res.add("expired")
	}
	return res, nil
}

// --------------------------------------------------------------- key cleanup

// KeyCleanup deletes COMPLETED idempotency keys older than the retention
// window. Without it, the table grows by one row per confirm forever.
type KeyCleanup struct {
	Keys      KeyStore
	Logger    *slog.Logger
	Retention time.Duration // IDEMPOTENCY_RETENTION
	BatchSize int
}

func (j *KeyCleanup) Name() string { return "idempotency_cleanup" }

// maxCleanupBatches bounds one run. The job runs rarely (hourly), so a single
// batch per run could fall behind; it deletes batch after batch until fewer
// than a full batch are left, up to this many.
const maxCleanupBatches = 100

func (j *KeyCleanup) RunOnce(ctx context.Context) (Result, error) {
	var total int64
	for i := 0; i < maxCleanupBatches; i++ {
		n, err := j.Keys.DeleteCompleted(ctx, j.Retention, j.BatchSize)
		if err != nil {
			return Result{Claimed: int(total), Succeeded: int(total)}, err
		}
		total += n
		if n < int64(j.BatchSize) {
			break
		}
	}
	res := Result{Claimed: int(total), Succeeded: int(total)}
	if total > 0 {
		j.Logger.Info("idempotency keys deleted", "job", j.Name(), "deleted", total, "outcome", "deleted")
		res.Outcomes = map[string]int{"deleted": int(total)}
	}
	return res, nil
}
