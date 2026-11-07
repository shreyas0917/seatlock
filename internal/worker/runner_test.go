// runner_test.go unit-tests the scheduler (runner.go): jobs run repeatedly,
// and on shutdown a running batch finishes but no new one starts.
package worker

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// countingJob counts its runs; each run takes `work` and records whether its
// context was cancelled before it finished.
type countingJob struct {
	runs        atomic.Int32
	interrupted atomic.Int32
	work        time.Duration
}

func (j *countingJob) Name() string { return "counting" }

func (j *countingJob) RunOnce(ctx context.Context) (Result, error) {
	j.runs.Add(1)
	select {
	case <-time.After(j.work):
	case <-ctx.Done():
		j.interrupted.Add(1)
	}
	return Result{Claimed: 1, Succeeded: 1}, nil
}

func TestRun_RepeatsJobs(t *testing.T) {
	job := &countingJob{}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	Run(ctx, []Scheduled{{Job: job, Interval: 20 * time.Millisecond}}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	assert.GreaterOrEqual(t, job.runs.Load(), int32(5))
}

// TestRun_ShutdownFinishesTheCurrentBatch: SIGTERM arrives in the middle of a
// batch. Run returns only after the batch finished, uninterrupted.
func TestRun_ShutdownFinishesTheCurrentBatch(t *testing.T) {
	job := &countingJob{work: 300 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, []Scheduled{{Job: job, Interval: 10 * time.Millisecond}}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
		close(done)
	}()

	time.Sleep(100 * time.Millisecond) // the first batch is now running
	cancel()                           // "SIGTERM"
	<-done

	assert.Equal(t, int32(1), job.runs.Load(), "no new batch after shutdown")
	assert.Zero(t, job.interrupted.Load(), "the running batch was not interrupted")
}

func TestJittered_StaysWithin10Percent(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := jittered(10 * time.Second)
		assert.GreaterOrEqual(t, d, 9*time.Second)
		assert.LessOrEqual(t, d, 11*time.Second)
	}
}
