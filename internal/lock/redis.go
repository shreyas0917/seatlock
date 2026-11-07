// Package lock implements booking.SeatLocker with Redis.
//
// Where it fits:
//
//	Service.HoldSeat -> RedisLocker.Acquire  (this file: fast filter, ~1ms)
//	                 -> Repository.CreateHold (PostgreSQL: the AUTHORITY)
//
// Redis here is an OPTIMISATION, not the source of truth. It quickly turns
// away the 49 losers of a 50-user race without touching Postgres. But Redis
// can lose data (restart, FLUSHALL, failover), so a "yes" from Redis is never
// enough on its own: PostgreSQL still checks, and Postgres wins any
// disagreement (see CreateHold in internal/booking/repository_postgres.go).
//
// FAIL CLOSED: if Redis cannot be asked, the answer is "no hold"
// (booking.ErrLockUnavailable -> 503), NEVER "the seat is free". There is no
// flag, fallback or code path that skips the lock when Redis is down.
package lock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/shreyas0917/seatlock/internal/booking"
)

// Options configures the Redis client.
type Options struct {
	Addr         string
	DB           int // Redis database number: 0 for the app, 1 for tests
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// PoolSize caps open connections; MinIdleConns keeps that many open while
	// idle (see config.RedisPoolSize for why both matter). Zero means the
	// go-redis default.
	PoolSize     int
	MinIdleConns int
}

// NewClient creates a Redis client. It does not connect yet: go-redis opens
// connections lazily on the first command, so the API can start even while
// Redis is down (confirms and reads must keep working without Redis).
func NewClient(opts Options) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:         opts.Addr,
		DB:           opts.DB,
		DialTimeout:  opts.DialTimeout,
		ReadTimeout:  opts.ReadTimeout,
		WriteTimeout: opts.WriteTimeout,
		// A small, warm pool. Waiting for a free pooled connection is
		// bounded by go-redis's PoolTimeout (default: ReadTimeout + 1s), and
		// a pool-wait timeout is an error like any other: fail closed.
		PoolSize:     opts.PoolSize,
		MinIdleConns: opts.MinIdleConns,
		// go-redis retries failed commands 3 times by default. With Redis
		// down that multiplies the wait before we can answer 503. We fail
		// fast instead (-1 = no retries) and let the client retry the HTTP
		// request.
		MaxRetries: -1,
	})
}

// RedisLocker is a per-seat lock stored in Redis.
type RedisLocker struct {
	client *redis.Client
	ttl    time.Duration
}

// Compile-time check that RedisLocker satisfies the booking.SeatLocker interface.
var _ booking.SeatLocker = (*RedisLocker)(nil)

// NewRedisLocker returns a locker whose locks live for holdTTL + extra.
//
// WHY THE REDIS LOCK MUST OUTLIVE THE DATABASE HOLD (310s vs 300s):
// Postgres decides when a hold expires (expires_at = NOW() + 300s). Imagine
// both used exactly 300s, but the Redis key was created a few milliseconds
// BEFORE the Postgres row (it is: we lock first, then insert). Then Redis
// frees the seat slightly before Postgres does:
//
//	t=299.99s  Redis key gone     -> user B's SET NX succeeds
//	t=299.99s  Postgres: A's hold is still ACTIVE -> B is rejected (409)
//
// That is still SAFE (Postgres is the authority), but the lock has stopped
// doing its one job. Clock differences and slow requests widen that gap. The
// extra 10s means: whenever Postgres still considers a seat held, Redis
// does too. The cost is a short window after a DB hold expires where Redis
// still says "held" (a new hold gets 409 for up to 10s). That errs on the
// safe side.
func NewRedisLocker(client *redis.Client, holdTTL, extra time.Duration) *RedisLocker {
	return &RedisLocker{client: client, ttl: holdTTL + extra}
}

// key is the Redis key for one seat, e.g. "seat:1:7".
func key(showID, seatID int64) string {
	return fmt.Sprintf("seat:%d:%d", showID, seatID)
}

// refreshIfOwnerScript runs when SET NX failed (the key exists). If the key
// belongs to the same owner, it resets the TTL and reports "acquired".
//
// About Lua scripts: Redis runs a script ATOMICALLY: no other command can
// run between its lines. That matters here. With two separate commands
// (GET, then EXPIRE), the key could expire after our GET and be taken by
// another user, and our EXPIRE would then extend THEIR lock. Inside one
// script, "check the owner" and "extend" happen as one step.
//
// KEYS[1] = seat key, ARGV[1] = owner, ARGV[2] = TTL in milliseconds.
var refreshIfOwnerScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	redis.call("PEXPIRE", KEYS[1], ARGV[2])
	return 1
end
return 0
`)

// releaseIfOwnerScript deletes the key only if it still belongs to owner.
//
// Why a plain DEL is dangerous: suppose user A's lock expired (A was slow),
// and user B then acquired the same seat. If A now runs DEL seat:1:7, it
// deletes B's lock, and a third user C can take the seat while B is still
// paying. Comparing the value first, atomically in one script, means a
// client can only ever delete its OWN lock.
var releaseIfOwnerScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

// Acquire tries to take the lock for (showID, seatID) on behalf of owner.
func (l *RedisLocker) Acquire(ctx context.Context, showID, seatID int64, owner string) (bool, error) {
	k := key(showID, seatID)

	// SET key owner NX EX 310: create the key only if it does Not eXist,
	// with an expiry. This is one atomic command: of 50 concurrent callers,
	// exactly one creates the key.
	err := l.client.SetArgs(ctx, k, owner, redis.SetArgs{Mode: "NX", TTL: l.ttl}).Err()

	switch {
	case err == nil:
		return true, nil // we created the key: lock acquired

	case errors.Is(err, redis.Nil):
		// redis.Nil vs a real error, the most important distinction here:
		//   - redis.Nil means Redis ANSWERED, and the answer was "nothing
		//     set" because the key already exists. A normal, healthy answer.
		//   - any other error (connection refused, timeout, ...) means we
		//     got NO answer. We know nothing about the seat.
		// Fall through below to check whether the existing key is ours.

	default:
		// FAIL CLOSED: no answer is treated as "no lock", never as "free".
		return false, fmt.Errorf("%w: acquire %s: %w", booking.ErrLockUnavailable, k, err)
	}

	// The key exists. If it is OURS (for example the user retried after a
	// timeout, or re-holds a seat they already hold), treat it as acquired
	// and refresh its TTL so Redis again outlives the new database hold.
	refreshed, err := refreshIfOwnerScript.Run(ctx, l.client, []string{k}, owner, l.ttl.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("%w: refresh %s: %w", booking.ErrLockUnavailable, k, err)
	}
	// If the key expired between SET NX and the script, refreshed is 0 and
	// we answer "someone holds it" (409). That is the safe mistake, and the
	// user's retry succeeds.
	return refreshed == 1, nil
}

// Release deletes the lock if owner holds it. Releasing someone else's lock,
// or a lock that has already expired, is a harmless no-op.
func (l *RedisLocker) Release(ctx context.Context, showID, seatID int64, owner string) error {
	k := key(showID, seatID)
	if err := releaseIfOwnerScript.Run(ctx, l.client, []string{k}, owner).Err(); err != nil {
		return fmt.Errorf("%w: release %s: %w", booking.ErrLockUnavailable, k, err)
	}
	return nil
}

// Ping checks Redis is reachable (used by /readyz to report its status).
func (l *RedisLocker) Ping(ctx context.Context) error {
	return l.client.Ping(ctx).Err()
}
