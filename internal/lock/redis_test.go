// redis_test.go proves the locker FAILS CLOSED when Redis is unreachable.
// It needs no Redis at all: it points the client at a port where nothing
// listens, so it runs in plain `make test`.
package lock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/shreyas0917/seatlock/internal/booking"
)

// newDeadLocker returns a locker pointing at 127.0.0.1:1 (nothing listens there).
func newDeadLocker() *RedisLocker {
	client := NewClient(Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  200 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
	})
	return NewRedisLocker(client, 300*time.Second, 10*time.Second)
}

func TestAcquire_RedisDown_FailsClosed(t *testing.T) {
	l := newDeadLocker()

	acquired, err := l.Acquire(context.Background(), 1, 7, "alice")

	// The two things that must be true when Redis is down:
	assert.False(t, acquired, "an unreachable lock must NEVER be reported as acquired")
	assert.ErrorIs(t, err, booking.ErrLockUnavailable)
}

func TestRelease_RedisDown_ReturnsErrLockUnavailable(t *testing.T) {
	err := newDeadLocker().Release(context.Background(), 1, 7, "alice")

	assert.ErrorIs(t, err, booking.ErrLockUnavailable)
}

func TestPing_RedisDown(t *testing.T) {
	assert.Error(t, newDeadLocker().Ping(context.Background()))
}

func TestKeyFormat(t *testing.T) {
	assert.Equal(t, "seat:1:7", key(1, 7))
}
