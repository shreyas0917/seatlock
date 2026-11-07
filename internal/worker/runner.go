// Package worker runs SeatLock's background jobs: the processes that repair
// what the request path leaves behind.
//
// Where it fits:
//
//	cmd/worker -> worker.Run(jobs) -> each Job.RunOnce on its own ticker
//	                                    -> booking.Service / repositories (the SAME
//	                                       code the API uses)
//
// This package only does SCHEDULING (tickers, jitter, shutdown), CLAIMING
// (which rows to work on) and LOGGING. What to do with a row is decided by
// booking.Service, so the API and the worker can never disagree about the
// rules.
//
// Many worker replicas may run at once, with no leader election: every job
// claims its rows with FOR UPDATE SKIP LOCKED (see
// internal/booking/repository_worker.go), so two replicas never get the same
// row.
package worker

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

// Result is what one RunOnce did. Counts are kept separate so Phase 5 can
// turn them into metrics directly.
type Result struct {
	Claimed   int // rows picked up
	Succeeded int // rows finished (or acted on) without error
	Failed    int // rows that failed and will be retried (or were dead-lettered)
}

// Job is one kind of background work. RunOnce processes ONE batch and
// returns. Tests call it directly, so they never wait on tickers.
type Job interface {
	Name() string
	RunOnce(ctx context.Context) (Result, error)
}

// Scheduled is a job and how often to run it.
type Scheduled struct {
	Job      Job
	Interval time.Duration
}

// jitterFraction spreads runs by up to +/-10% of the interval, so several
// worker replicas started together do not all hit the database at the same
// instant on every tick.
const jitterFraction = 0.1

// batchTimeout bounds one RunOnce. Each row inside it still has its own,
// shorter, query and gateway timeouts.
const batchTimeout = 5 * time.Minute

// Run runs every job on its own schedule until ctx is cancelled.
//
// GRACEFUL SHUTDOWN: when ctx is cancelled (SIGTERM), no NEW batch starts,
// but a batch already running is allowed to FINISH: it runs on a context
// that is not cancelled by shutdown (only by batchTimeout). Stopping halfway
// would be safe too (claims are leases that simply expire), but finishing
// avoids leaving rows waiting a full lease for no reason.
func Run(ctx context.Context, jobs []Scheduled, logger *slog.Logger) {
	var wg sync.WaitGroup
	for _, s := range jobs {
		wg.Add(1)
		go func(s Scheduled) {
			defer wg.Done()
			loop(ctx, s, logger)
		}(s)
	}
	wg.Wait()
}

func loop(ctx context.Context, s Scheduled, logger *slog.Logger) {
	logger.Info("job started", "job", s.Job.Name(), "interval", s.Interval.String())
	for {
		select {
		case <-ctx.Done():
			logger.Info("job stopped", "job", s.Job.Name())
			return
		case <-time.After(jittered(s.Interval)):
		}

		// The batch runs on a context detached from shutdown (see Run).
		batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchTimeout)
		start := time.Now()
		res, err := s.Job.RunOnce(batchCtx)
		cancel()

		if err != nil {
			logger.Error("job run failed", "job", s.Job.Name(), "error", err.Error())
		} else if res.Claimed > 0 {
			logger.Info("job run",
				"job", s.Job.Name(),
				"claimed", res.Claimed, "succeeded", res.Succeeded, "failed", res.Failed,
				"duration_ms", time.Since(start).Milliseconds())
		}
	}
}

// jittered returns d plus or minus up to jitterFraction of d.
func jittered(d time.Duration) time.Duration {
	delta := (rand.Float64()*2 - 1) * jitterFraction * float64(d)
	return d + time.Duration(delta)
}
