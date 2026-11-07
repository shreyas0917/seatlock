//go:build integration

// service_integration_test.go runs the full hold -> confirm flow against the
// REAL PostgreSQL and the REAL Redis from docker compose. These are the
// Phase 2 proofs:
//
//   - hold race: 50 users, 1 seat -> exactly 1 hold
//   - confirm race: 10 confirms of 1 hold -> exactly 1 booking
//   - Redis loses its data -> PostgreSQL still rejects the second user
//   - Redis is down -> new holds fail closed, existing holds still confirm
//
// It is in package booking_test (an "external" test package) rather than
// package booking, because it imports internal/lock, and internal/lock
// imports booking: inside package booking that would be an import cycle.
// The trade-off: it can only use exported names, like a real caller.
//
// Postgres: database seatlock_test, default pool settings, no warm-up.
// Redis: database 1 (the app uses 0), emptied with FLUSHDB before each test.
package booking_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shreyas0917/seatlock/internal/booking"
	"github.com/shreyas0917/seatlock/internal/config"
	"github.com/shreyas0917/seatlock/internal/db"
	"github.com/shreyas0917/seatlock/internal/lock"
)

const (
	holdTTL   = 300 * time.Second
	lockExtra = 10 * time.Second
)

// Generous timeouts for integration tests. These tests check CORRECTNESS
// (one winner, no double booking), not speed. On a busy machine the
// production timeouts (3s DB, 200ms Redis) turned a slow-but-correct run into
// a failure. Latency is measured separately, by load tests (Phase 6).
const (
	testDBQueryTimeout = 15 * time.Second
	testRedisTimeout   = 2 * time.Second
)

// env is everything one test needs.
type env struct {
	pool    *pgxpool.Pool
	redis   *redis.Client
	repo    *booking.PostgresRepository
	svc     *booking.Service // wired to the real Redis
	showID  int64
	seatIDs []int64
}

// setup gives each test a clean Postgres (one show, three seats) and an
// empty Redis database 1.
func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()

	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://seatlock:seatlock@localhost:5434/seatlock_test?sslmode=disable"
	}
	require.NoError(t, db.MigrateUp(dbURL), "is postgres running? try `make up`")

	defaults, err := config.Load()
	require.NoError(t, err)
	pool, err := db.NewPool(ctx, dbURL, defaults.DBMaxConns, defaults.DBMinConns)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `TRUNCATE bookings, payments, holds, seats, shows, outbox, idempotency_keys RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	var showID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO shows (title, venue, starts_at)
		VALUES ('Test Show', 'Test Venue', now() + interval '7 days') RETURNING id`).Scan(&showID))
	var seatIDs []int64
	for _, label := range []string{"A1", "A2", "A3"} {
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO seats (show_id, seat_label, price_paise)
			VALUES ($1, $2, 25000) RETURNING id`, showID, label).Scan(&id))
		seatIDs = append(seatIDs, id)
	}

	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rdb := lock.NewClient(lock.Options{
		Addr: redisAddr, DB: 1,
		DialTimeout: testRedisTimeout, ReadTimeout: testRedisTimeout, WriteTimeout: testRedisTimeout,
		PoolSize: int(defaults.RedisPoolSize), MinIdleConns: int(defaults.RedisMinIdleConns),
	})
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.FlushDB(ctx).Err(), "is redis running? try `make up`")
	waitForIdleConns(t, rdb, int(defaults.RedisMinIdleConns))

	repo := booking.NewPostgresRepository(pool, testDBQueryTimeout)
	svc := booking.NewService(repo, lock.NewRedisLocker(rdb, holdTTL, lockExtra), holdTTL)
	return env{pool: pool, redis: rdb, repo: repo, svc: svc, showID: showID, seatIDs: seatIDs}
}

// waitForIdleConns waits until the Redis client has opened its MinIdleConns
// connections. A long-running API server is always in this state; a client
// created a millisecond ago is not. Waiting puts the test in the same
// steady state as production instead of measuring a cold start.
func waitForIdleConns(t *testing.T, rdb *redis.Client, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for int(rdb.PoolStats().IdleConns) < n {
		require.True(t, time.Now().Before(deadline), "redis pool never reached %d idle connections", n)
		time.Sleep(10 * time.Millisecond)
	}
}

// deadLockerService returns a Service whose Redis is unreachable
// (127.0.0.1:1, where nothing listens), but whose Postgres is the real one.
func deadLockerService(e env) *booking.Service {
	dead := lock.NewClient(lock.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond,
	})
	return booking.NewService(e.repo, lock.NewRedisLocker(dead, holdTTL, lockExtra), holdTTL)
}

// countRows runs a COUNT(*) query.
func countRows(t *testing.T, e env, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

// TestHoldRace: 50 different users try to hold the SAME seat at the same
// instant. Exactly one wins; the other 49 get ErrSeatHeld; the holds table
// has exactly one ACTIVE row for the seat.
//
// Same "starting gun" technique as the Phase 1 test: every goroutine blocks
// on <-start, and close(start) releases them all at once.
func TestHoldRace(t *testing.T) {
	e := setup(t)
	const users = 50
	seat := e.seatIDs[0]

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, users)
	for i := 0; i < users; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, errs[i] = e.svc.HoldSeat(context.Background(), booking.HoldSeatInput{
				UserID: fmt.Sprintf("user-%d", i), ShowID: e.showID, SeatID: seat,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	wins, held := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, booking.ErrSeatHeld):
			held++
		default:
			t.Errorf("user %d: unexpected error: %v", i, err) // full text, never hidden
		}
	}
	t.Logf("%d users -> %d hold, %d ErrSeatHeld", users, wins, held)
	assert.Equal(t, 1, wins)
	assert.Equal(t, users-1, held)
	assert.Equal(t, 1, countRows(t, e, `SELECT COUNT(*) FROM holds WHERE seat_id = $1 AND status = 'ACTIVE'`, seat))
}

// TestConfirmRace: the same user sends confirm 10 times at once for one hold
// (double-click, retries). FOR UPDATE on the hold row serialises them: the
// first creates the booking, the other 9 find the hold CONSUMED.
func TestConfirmRace(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	hold, _, err := e.svc.HoldSeat(ctx, booking.HoldSeatInput{UserID: "alice", ShowID: e.showID, SeatID: e.seatIDs[0]})
	require.NoError(t, err)

	const confirms = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, confirms)
	for i := 0; i < confirms; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = e.svc.ConfirmBooking(ctx, booking.ConfirmInput{UserID: "alice", HoldID: hold.ID})
		}(i)
	}
	close(start)
	wg.Wait()

	booked, used := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			booked++
		case errors.Is(err, booking.ErrHoldAlreadyUsed):
			used++
		default:
			t.Errorf("confirm %d: unexpected error: %v", i, err)
		}
	}
	t.Logf("%d confirms -> %d booking, %d ErrHoldAlreadyUsed", confirms, booked, used)
	assert.Equal(t, 1, booked)
	assert.Equal(t, confirms-1, used)
	assert.Equal(t, 1, countRows(t, e, `SELECT COUNT(*) FROM bookings WHERE hold_id = $1`, hold.ID))
}

// TestConfirmRejections: every way a confirm must be refused.
func TestConfirmRejections(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, e env, holdID int64)
		user    string
		useFake bool
		wantErr error
	}{
		{name: "another user's hold", user: "mallory", wantErr: booking.ErrHoldNotFound},
		{name: "fake hold id", user: "alice", useFake: true, wantErr: booking.ErrHoldNotFound},
		{name: "expired hold", user: "alice", wantErr: booking.ErrHoldExpired,
			prepare: func(t *testing.T, e env, id int64) {
				_, err := e.pool.Exec(context.Background(),
					`UPDATE holds SET expires_at = NOW() - interval '1 second' WHERE id = $1`, id)
				require.NoError(t, err)
			}},
		{name: "released hold", user: "alice", wantErr: booking.ErrHoldExpired,
			prepare: func(t *testing.T, e env, id int64) {
				_, err := e.svc.CancelHold(context.Background(), "alice", id)
				require.NoError(t, err)
			}},
		{name: "already consumed hold", user: "alice", wantErr: booking.ErrHoldAlreadyUsed,
			prepare: func(t *testing.T, e env, id int64) {
				_, err := e.svc.ConfirmBooking(context.Background(), booking.ConfirmInput{UserID: "alice", HoldID: id})
				require.NoError(t, err)
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			ctx := context.Background()
			hold, _, err := e.svc.HoldSeat(ctx, booking.HoldSeatInput{UserID: "alice", ShowID: e.showID, SeatID: e.seatIDs[0]})
			require.NoError(t, err)
			if tc.prepare != nil {
				tc.prepare(t, e, hold.ID)
			}
			holdID := hold.ID
			if tc.useFake {
				holdID = 987654
			}

			_, err = e.svc.ConfirmBooking(ctx, booking.ConfirmInput{UserID: tc.user, HoldID: holdID})

			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// TestRedisLostData_PostgresWins: Redis forgets every lock (restart without
// persistence, FLUSHALL, failover to an empty replica) while Postgres still
// has user A's ACTIVE hold. User B now gets the REDIS lock, but the Postgres
// check rejects B. This is the proof that PostgreSQL is the authority and
// Redis can never override it.
func TestRedisLostData_PostgresWins(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	seat := e.seatIDs[0]
	holdA, _, err := e.svc.HoldSeat(ctx, booking.HoldSeatInput{UserID: "alice", ShowID: e.showID, SeatID: seat})
	require.NoError(t, err)

	require.NoError(t, e.redis.FlushDB(ctx).Err()) // Redis loses all its data

	_, _, err = e.svc.HoldSeat(ctx, booking.HoldSeatInput{UserID: "bob", ShowID: e.showID, SeatID: seat})

	assert.ErrorIs(t, err, booking.ErrSeatHeld, "Postgres must reject bob even though Redis let him through")
	key := fmt.Sprintf("seat:%d:%d", e.showID, seat)
	assert.Zero(t, e.redis.Exists(ctx, key).Val(), "bob's Redis lock is released after Postgres rejects him")
	assert.Equal(t, 1, countRows(t, e, `SELECT COUNT(*) FROM holds WHERE seat_id = $1 AND status = 'ACTIVE'`, seat))

	// And alice can still complete her booking.
	_, err = e.svc.ConfirmBooking(ctx, booking.ConfirmInput{UserID: "alice", HoldID: holdA.ID})
	assert.NoError(t, err)
}

// TestRedisDown_FailClosed_ExistingHoldStillConfirms is "Option B":
// with Redis unreachable, NEW holds are refused (and no hold row is
// written), but a hold that already exists in Postgres confirms.
func TestRedisDown_FailClosed_ExistingHoldStillConfirms(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	holdA, _, err := e.svc.HoldSeat(ctx, booking.HoldSeatInput{UserID: "alice", ShowID: e.showID, SeatID: e.seatIDs[0]})
	require.NoError(t, err)

	down := deadLockerService(e) // same Postgres, Redis unreachable

	_, _, err = down.HoldSeat(ctx, booking.HoldSeatInput{UserID: "bob", ShowID: e.showID, SeatID: e.seatIDs[1]})
	assert.ErrorIs(t, err, booking.ErrLockUnavailable, "a new hold must fail closed")
	assert.Zero(t, countRows(t, e, `SELECT COUNT(*) FROM holds WHERE seat_id = $1`, e.seatIDs[1]),
		"no hold row may be written while the lock is unavailable")

	b, err := down.ConfirmBooking(ctx, booking.ConfirmInput{UserID: "alice", HoldID: holdA.ID})
	require.NoError(t, err, "an existing valid hold must still confirm with Redis down")
	assert.Equal(t, holdA.ID, b.HoldID)
}

// TestSameUserReHold: holding the same seat twice returns the same hold.
func TestSameUserReHold(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	in := booking.HoldSeatInput{UserID: "alice", ShowID: e.showID, SeatID: e.seatIDs[0]}

	first, created1, err := e.svc.HoldSeat(ctx, in)
	require.NoError(t, err)
	second, created2, err := e.svc.HoldSeat(ctx, in)
	require.NoError(t, err)

	assert.True(t, created1)
	assert.False(t, created2, "the second call returns the existing hold (HTTP 200, not 201)")
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, 1, countRows(t, e, `SELECT COUNT(*) FROM holds WHERE seat_id = $1`, e.seatIDs[0]))
}

// TestSeatMapThroughService: AVAILABLE / HELD / BOOKED, and an expired hold
// shows as AVAILABLE (even though its row still says ACTIVE).
func TestSeatMapThroughService(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	booked, _, err := e.svc.HoldSeat(ctx, booking.HoldSeatInput{UserID: "alice", ShowID: e.showID, SeatID: e.seatIDs[0]})
	require.NoError(t, err)
	_, err = e.svc.ConfirmBooking(ctx, booking.ConfirmInput{UserID: "alice", HoldID: booked.ID})
	require.NoError(t, err)
	_, _, err = e.svc.HoldSeat(ctx, booking.HoldSeatInput{UserID: "bob", ShowID: e.showID, SeatID: e.seatIDs[1]})
	require.NoError(t, err)
	expired, _, err := e.svc.HoldSeat(ctx, booking.HoldSeatInput{UserID: "carol", ShowID: e.showID, SeatID: e.seatIDs[2]})
	require.NoError(t, err)
	_, err = e.pool.Exec(ctx, `UPDATE holds SET expires_at = NOW() - interval '1 second' WHERE id = $1`, expired.ID)
	require.NoError(t, err)

	seats, err := e.svc.ListSeats(ctx, e.showID)

	require.NoError(t, err)
	require.Len(t, seats, 3)
	assert.Equal(t, booking.SeatBooked, seats[0].Status)
	assert.Equal(t, booking.SeatHeld, seats[1].Status)
	assert.Equal(t, booking.SeatAvailable, seats[2].Status)
}
