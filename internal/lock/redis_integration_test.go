//go:build integration

// redis_integration_test.go tests RedisLocker against the real Redis from
// docker compose. It uses Redis database 1 (the app uses 0) and empties it
// before every test with FLUSHDB, so tests never see each other's keys.
package lock

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testHoldTTL = 300 * time.Second
	testExtra   = 10 * time.Second
)

// setupRedis returns a locker and the raw client (to inspect keys).
func setupRedis(t *testing.T) (*RedisLocker, *redis.Client) {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	client := NewClient(Options{
		Addr: addr, DB: 1,
		// Generous timeouts: these tests check lock correctness, not latency.
		DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
	})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.FlushDB(context.Background()).Err(), "is redis running? try `make up`")
	return NewRedisLocker(client, testHoldTTL, testExtra), client
}

func TestAcquire_FreeSeat_SetsOwnerAndTTL(t *testing.T) {
	l, client := setupRedis(t)
	ctx := context.Background()

	acquired, err := l.Acquire(ctx, 1, 7, "alice")
	require.NoError(t, err)
	assert.True(t, acquired)

	assert.Equal(t, "alice", client.Get(ctx, "seat:1:7").Val(), "the value is the owner")
	ttl := client.TTL(ctx, "seat:1:7").Val()
	assert.Greater(t, ttl, testHoldTTL, "the Redis lock must outlive the 300s database hold")
	assert.LessOrEqual(t, ttl, testHoldTTL+testExtra)
}

func TestAcquire_HeldByOther_NotAcquired(t *testing.T) {
	l, client := setupRedis(t)
	ctx := context.Background()
	_, err := l.Acquire(ctx, 1, 7, "alice")
	require.NoError(t, err)

	acquired, err := l.Acquire(ctx, 1, 7, "bob")

	require.NoError(t, err, "'someone else holds it' is a normal answer, not an error")
	assert.False(t, acquired)
	assert.Equal(t, "alice", client.Get(ctx, "seat:1:7").Val(), "bob must not overwrite alice's lock")
}

func TestAcquire_SameOwner_ReacquiresAndRefreshesTTL(t *testing.T) {
	l, client := setupRedis(t)
	ctx := context.Background()
	_, err := l.Acquire(ctx, 1, 7, "alice")
	require.NoError(t, err)
	// Pretend most of the lock's life has passed: only 5s left.
	require.NoError(t, client.Expire(ctx, "seat:1:7", 5*time.Second).Err())

	acquired, err := l.Acquire(ctx, 1, 7, "alice")

	require.NoError(t, err)
	assert.True(t, acquired, "a retry by the same user must succeed")
	assert.Greater(t, client.TTL(ctx, "seat:1:7").Val(), testHoldTTL, "re-acquire must refresh the TTL")
}

func TestRelease_OnlyOwnerCanRelease(t *testing.T) {
	l, client := setupRedis(t)
	ctx := context.Background()
	_, err := l.Acquire(ctx, 1, 7, "alice")
	require.NoError(t, err)

	// Bob's release must NOT delete alice's lock (a plain DEL would).
	require.NoError(t, l.Release(ctx, 1, 7, "bob"))
	assert.Equal(t, "alice", client.Get(ctx, "seat:1:7").Val())

	require.NoError(t, l.Release(ctx, 1, 7, "alice"))
	assert.Equal(t, int64(0), client.Exists(ctx, "seat:1:7").Val())

	// Releasing a lock that no longer exists is a harmless no-op.
	assert.NoError(t, l.Release(ctx, 1, 7, "alice"))
}

func TestPing(t *testing.T) {
	l, _ := setupRedis(t)
	assert.NoError(t, l.Ping(context.Background()))
}
