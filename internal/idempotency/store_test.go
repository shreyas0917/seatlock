//go:build integration

// store_test.go tests the Postgres idempotency store against the real
// database (seatlock_test): new keys, in-progress, replay, 422 on reuse,
// lease takeover with token-checked completion, and release.
package idempotency

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shreyas0917/seatlock/internal/config"
	"github.com/shreyas0917/seatlock/internal/db"
)

func newTestStore(t *testing.T, lease time.Duration) *Store {
	t.Helper()
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://seatlock:seatlock@localhost:5434/seatlock_test?sslmode=disable"
	}
	require.NoError(t, db.MigrateUp(url), "is postgres running? try `make up`")
	defaults, err := config.Load()
	require.NoError(t, err)
	pool, err := db.NewPool(ctx, url, defaults.DBMaxConns, defaults.DBMinConns)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE idempotency_keys`)
	require.NoError(t, err)
	return NewStore(pool, lease, 15*time.Second) // generous: correctness, not latency
}

func TestBegin_NewKeyThenInProgress(t *testing.T) {
	s := newTestStore(t, 30*time.Second)
	ctx := context.Background()

	first, err := s.Begin(ctx, "alice", "k1", "h1")
	require.NoError(t, err)
	assert.Equal(t, Proceed, first.Kind)
	assert.Len(t, first.Token, 32)

	_, err = s.Begin(ctx, "alice", "k1", "h1")
	assert.ErrorIs(t, err, ErrInProgress, "the lease is still valid")

	other, err := s.Begin(ctx, "bob", "k1", "h1")
	require.NoError(t, err)
	assert.Equal(t, Proceed, other.Kind, "keys are per user")
}

// TestReplayIsByteForByte: the stored body must come back EXACTLY, with the
// original key order and spacing (migration 0004: JSON, not JSONB).
func TestReplayIsByteForByte(t *testing.T) {
	s := newTestStore(t, 30*time.Second)
	ctx := context.Background()
	claim, err := s.Begin(ctx, "alice", "k1", "h1")
	require.NoError(t, err)
	original := `{"id":68,"show_id":1,"seat_id":6,"user_id":"smoke","hold_id":35}`
	_, err = s.Complete(ctx, "alice", "k1", claim.Token, Response{Status: 201, Body: []byte(original)})
	require.NoError(t, err)

	replay, err := s.Begin(ctx, "alice", "k1", "h1")

	require.NoError(t, err)
	assert.Equal(t, original, string(replay.Stored.Body))
}

func TestCompleteThenReplay(t *testing.T) {
	s := newTestStore(t, 30*time.Second)
	ctx := context.Background()
	claim, err := s.Begin(ctx, "alice", "k1", "h1")
	require.NoError(t, err)

	stored, err := s.Complete(ctx, "alice", "k1", claim.Token, Response{Status: 201, Body: []byte(`{"id":1}`)})
	require.NoError(t, err)
	assert.True(t, stored)

	replay, err := s.Begin(ctx, "alice", "k1", "h1")
	require.NoError(t, err)
	assert.Equal(t, Replay, replay.Kind)
	assert.Equal(t, 201, replay.Stored.Status)
	assert.Equal(t, `{"id":1}`, string(replay.Stored.Body))

	_, err = s.Begin(ctx, "alice", "k1", "h-different")
	assert.ErrorIs(t, err, ErrKeyReused)
}

// TestLeaseTakeover_OnlyTheCurrentOwnerCompletes: request A's lease expires,
// request B takes the key over; A finishing late must NOT overwrite B's result.
func TestLeaseTakeover_OnlyTheCurrentOwnerCompletes(t *testing.T) {
	s := newTestStore(t, 300*time.Millisecond)
	ctx := context.Background()
	a, err := s.Begin(ctx, "alice", "k1", "h1")
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond) // A's lease runs out (A "crashed" or is slow)

	b, err := s.Begin(ctx, "alice", "k1", "h1")
	require.NoError(t, err)
	assert.Equal(t, Proceed, b.Kind, "an expired lease can be taken over")
	assert.NotEqual(t, a.Token, b.Token)

	storedA, err := s.Complete(ctx, "alice", "k1", a.Token, Response{Status: 201, Body: []byte(`{"from":"A"}`)})
	require.NoError(t, err)
	assert.False(t, storedA, "the old owner must not write")

	storedB, err := s.Complete(ctx, "alice", "k1", b.Token, Response{Status: 201, Body: []byte(`{"from":"B"}`)})
	require.NoError(t, err)
	assert.True(t, storedB)

	replay, err := s.Begin(ctx, "alice", "k1", "h1")
	require.NoError(t, err)
	assert.JSONEq(t, `{"from":"B"}`, string(replay.Stored.Body))
}

func TestRelease_MakesKeyRetryable(t *testing.T) {
	s := newTestStore(t, 30*time.Second)
	ctx := context.Background()
	first, err := s.Begin(ctx, "alice", "k1", "h1")
	require.NoError(t, err)

	require.NoError(t, s.Release(ctx, "alice", "k1", first.Token))

	again, err := s.Begin(ctx, "alice", "k1", "h1")
	require.NoError(t, err)
	assert.Equal(t, Proceed, again.Kind, "a released key runs again (not replayed)")

	_, err = s.Begin(ctx, "alice", "k1", "h-other")
	assert.ErrorIs(t, err, ErrKeyReused, "a released key stays bound to its request")
}
