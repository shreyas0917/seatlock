//go:build integration

// worker_integration_test.go proves the background jobs against the REAL
// PostgreSQL and Redis and the mock gateway (in-process, httptest). Jobs are
// driven with RunOnce, so no test waits on a ticker. Time is "moved forward"
// by back-dating rows with SQL instead of sleeping.
//
// Every test ends by running the invariant checks from demo/verify.sql
// (every query must return 0 rows).
package worker_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
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
	"github.com/shreyas0917/seatlock/internal/idempotency"
	"github.com/shreyas0917/seatlock/internal/lock"
	"github.com/shreyas0917/seatlock/internal/payment"
	"github.com/shreyas0917/seatlock/internal/paymentmock"
	"github.com/shreyas0917/seatlock/internal/worker"
)

// Generous timeouts: these tests check correctness, not latency (Phase 6).
const (
	testDBTimeout      = 15 * time.Second
	testRedisTimeout   = 2 * time.Second
	testPaymentTimeout = 2 * time.Second
	holdTTL            = 300 * time.Second
)

type env struct {
	t       *testing.T
	pool    *pgxpool.Pool
	rdb     *redis.Client
	repo    *booking.PostgresRepository
	mock    *paymentmock.Server
	gateway *payment.Client // the working gateway
	locker  *lock.RedisLocker
	svc     *booking.Service // working gateway + working Redis
	keys    *idempotency.Store
	logger  *slog.Logger
	showID  int64
	seatIDs []int64
}

func setup(t *testing.T, seats int) *env {
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

	e := &env{t: t, pool: pool}
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO shows (title, venue, starts_at) VALUES ('Worker Show', 'Venue', now() + interval '7 days')
		RETURNING id`).Scan(&e.showID))
	for i := 0; i < seats; i++ {
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO seats (show_id, seat_label, price_paise) VALUES ($1, $2, 25000) RETURNING id`,
			e.showID, fmt.Sprintf("S%d", i+1)).Scan(&id))
		e.seatIDs = append(e.seatIDs, id)
	}

	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	e.rdb = lock.NewClient(lock.Options{Addr: redisAddr, DB: 1,
		DialTimeout: testRedisTimeout, ReadTimeout: testRedisTimeout, WriteTimeout: testRedisTimeout})
	t.Cleanup(func() { _ = e.rdb.Close() })
	require.NoError(t, e.rdb.FlushDB(ctx).Err(), "is redis running? try `make up`")
	e.locker = lock.NewRedisLocker(e.rdb, holdTTL, 10*time.Second)

	e.logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	e.mock = paymentmock.New(paymentmock.Options{SlowDelay: time.Second, Logger: e.logger})
	mockServer := httptest.NewServer(e.mock)
	t.Cleanup(mockServer.Close)
	e.gateway = payment.NewClient(mockServer.URL, testPaymentTimeout)

	e.repo = booking.NewPostgresRepository(pool, testDBTimeout)
	e.svc = booking.NewService(e.repo, e.locker, e.gateway, holdTTL)
	e.keys = idempotency.NewStore(pool, 30*time.Second, testDBTimeout)
	return e
}

// --- building blocks ---

// deadGateway is a payment client pointing where nothing listens.
func deadGateway() *payment.Client {
	return payment.NewClient("http://127.0.0.1:1", 500*time.Millisecond)
}

// deadLocker is a Redis locker pointing where nothing listens.
func deadLocker() *lock.RedisLocker {
	c := lock.NewClient(lock.Options{Addr: "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond})
	return lock.NewRedisLocker(c, holdTTL, 10*time.Second)
}

func (e *env) hold(user string, seat int) booking.Hold {
	e.t.Helper()
	h, _, err := e.svc.HoldSeat(context.Background(), booking.HoldSeatInput{UserID: user, ShowID: e.showID, SeatID: e.seatIDs[seat]})
	require.NoError(e.t, err)
	return h
}

// abandonAfterTimeout: the user confirms, the gateway TAKES the money but the
// answer is lost (504), and the user never retries.
func (e *env) abandonAfterTimeout(user string, seat int) booking.Hold {
	e.t.Helper()
	h := e.hold(user, seat)
	e.queue(paymentmock.BehaviorTimeout)
	_, err := e.svc.ConfirmBooking(context.Background(), booking.ConfirmInput{UserID: user, HoldID: h.ID})
	require.ErrorIs(e.t, err, booking.ErrPaymentPending)
	return h
}

// abandonBeforeGateway: the confirm never reached the gateway (it was down),
// so there is NO charge for the key. The user never retries.
func (e *env) abandonBeforeGateway(user string, seat int) booking.Hold {
	e.t.Helper()
	h := e.hold(user, seat)
	down := booking.NewService(e.repo, e.locker, deadGateway(), holdTTL)
	_, err := down.ConfirmBooking(context.Background(), booking.ConfirmInput{UserID: user, HoldID: h.ID})
	require.ErrorIs(e.t, err, booking.ErrPaymentPending)
	return h
}

func (e *env) queue(behavior string) {
	e.t.Helper()
	e.queueN(behavior, 1)
}

func (e *env) queueN(behavior string, n int) {
	e.t.Helper()
	srv := httptest.NewServer(e.mock) // a second listener on the same mock instance
	defer srv.Close()
	resp, err := srv.Client().Post(srv.URL+"/debug/next-behavior", "application/json",
		strings.NewReader(fmt.Sprintf(`{"behavior":%q,"count":%d}`, behavior, n)))
	require.NoError(e.t, err)
	resp.Body.Close()
}

// ageAllPendingPayments makes every PENDING payment look 5 minutes old,
// i.e. past RECONCILE_AFTER, instead of sleeping.
func (e *env) ageAllPendingPayments() {
	e.exec(`UPDATE payments SET created_at = NOW() - interval '5 minutes' WHERE status = 'PENDING'`)
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	_, err := e.pool.Exec(context.Background(), sql, args...)
	require.NoError(e.t, err)
}

func (e *env) str(sql string, args ...any) string {
	e.t.Helper()
	var v string
	require.NoError(e.t, e.pool.QueryRow(context.Background(), sql, args...).Scan(&v))
	return v
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	require.NoError(e.t, e.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func (e *env) reconciler(svc *booking.Service, batch int) *worker.Reconciler {
	return &worker.Reconciler{Store: e.repo, Actions: svc, Logger: e.logger,
		OlderThan: 2 * time.Minute, Lease: time.Minute, BatchSize: batch}
}

func (e *env) drainer(svc *booking.Service, batch int) *worker.OutboxDrainer {
	return &worker.OutboxDrainer{Store: e.repo, Actions: svc, Logger: e.logger, Lease: time.Minute, BatchSize: batch}
}

func (e *env) runOnce(job worker.Job) worker.Result {
	e.t.Helper()
	res, err := job.RunOnce(context.Background())
	require.NoError(e.t, err)
	return res
}

// assertInvariants runs every check in demo/verify.sql; each must return 0 rows.
func (e *env) assertInvariants() {
	e.t.Helper()
	data, err := os.ReadFile("../../demo/verify.sql")
	require.NoError(e.t, err)
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") || strings.HasPrefix(trimmed, `\echo`) {
			continue
		}
		lines = append(lines, line)
	}
	checks := 0
	for _, query := range strings.Split(strings.Join(lines, "\n"), ";") {
		if strings.TrimSpace(query) == "" {
			continue
		}
		checks++
		rows, err := e.pool.Query(context.Background(), query)
		require.NoError(e.t, err, query)
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		assert.Zero(e.t, n, "verify.sql check must return 0 rows:\n%s", query)
	}
	require.Greater(e.t, checks, 5)
}

// ======================================================= payment reconciler

// 1. Timeout at confirm, user never retries, the charge DID succeed and the
// hold is still valid: the worker books the seat.
func TestReconciler_ChargeSucceeded_HoldValid_Books(t *testing.T) {
	e := setup(t, 1)
	h := e.abandonAfterTimeout("alice", 0)
	e.ageAllPendingPayments()

	res := e.runOnce(e.reconciler(e.svc, 50))

	assert.Equal(t, worker.Result{Claimed: 1, Succeeded: 1}, res)
	assert.Equal(t, "SUCCEEDED", e.str(`SELECT status FROM payments WHERE hold_id = $1`, h.ID))
	assert.Equal(t, 1, e.count(`SELECT COUNT(*) FROM bookings WHERE hold_id = $1`, h.ID))
	st := e.mock.Stats()
	assert.Equal(t, 1, st.MoneyTaken, "1 charge")
	e.assertInvariants()
}

// 2. Same, but the hold expired meanwhile: REFUND_PENDING + outbox, then the
// drainer refunds.
func TestReconciler_ChargeSucceeded_HoldExpired_DrainerRefunds(t *testing.T) {
	e := setup(t, 1)
	h := e.abandonAfterTimeout("alice", 0)
	e.ageAllPendingPayments()
	e.exec(`UPDATE holds SET expires_at = NOW() - interval '1 second' WHERE id = $1`, h.ID)

	e.runOnce(e.reconciler(e.svc, 50))
	assert.Equal(t, "REFUND_PENDING", e.str(`SELECT status FROM payments WHERE hold_id = $1`, h.ID))
	assert.Equal(t, 1, e.count(`SELECT COUNT(*) FROM outbox WHERE kind = 'REFUND' AND status = 'PENDING'`))

	e.runOnce(e.drainer(e.svc, 50))

	assert.Equal(t, "REFUNDED", e.str(`SELECT status FROM payments WHERE hold_id = $1`, h.ID))
	assert.Zero(t, e.count(`SELECT COUNT(*) FROM bookings`))
	st := e.mock.Stats()
	assert.Equal(t, 1, st.MoneyTaken, "1 charge")
	assert.Equal(t, 1, st.TotalRefunds, "1 refund")
	e.assertInvariants()
}

// 3. The charge never reached the gateway: the worker voids the key and
// abandons the payment; a late charge with that key is refused; the user pays
// again (new payment, new key). Exactly ONE successful charge in total.
func TestReconciler_NotFound_VoidsAbandons_ThenNewPaymentWorks(t *testing.T) {
	e := setup(t, 1)
	h := e.abandonBeforeGateway("alice", 0)
	e.ageAllPendingPayments()
	oldKey := e.str(`SELECT charge_key FROM payments WHERE hold_id = $1`, h.ID)

	e.runOnce(e.reconciler(e.svc, 50))

	assert.Equal(t, "ABANDONED", e.str(`SELECT status FROM payments WHERE charge_key = $1`, oldKey))
	late, err := e.gateway.Charge(context.Background(), booking.ChargeRequest{IdempotencyKey: oldKey, AmountPaise: 25000})
	require.NoError(t, err)
	assert.Equal(t, booking.ChargeVoided, late.Status, "a late charge with the voided key takes no money")

	// The user comes back and pays (a retry with the old Idempotency-Key ends
	// up here too: the 504 was never stored, and BeginPayment only reuses
	// PENDING payments).
	b, err := e.svc.ConfirmBooking(context.Background(), booking.ConfirmInput{UserID: "alice", HoldID: h.ID})
	require.NoError(t, err)
	newKey := e.str(`SELECT charge_key FROM payments WHERE status = 'SUCCEEDED'`)
	assert.NotEqual(t, oldKey, newKey)
	assert.Equal(t, h.ID, b.HoldID)
	st := e.mock.Stats()
	assert.Equal(t, 1, st.MoneyTaken, "exactly 1 successful charge")
	assert.Equal(t, 1, st.TotalVoids)
	e.assertInvariants()
}

// raceGateway reproduces the void race deterministically: Lookup says "not
// found", and the late charge lands at the gateway just BEFORE the void.
type raceGateway struct {
	*payment.Client
}

func (g raceGateway) Lookup(ctx context.Context, key string) (booking.ChargeResult, error) {
	return booking.ChargeResult{}, booking.ErrChargeNotFound
}

func (g raceGateway) Void(ctx context.Context, key string) (booking.ChargeResult, error) {
	// The slow request arrives now, between the lookup and the void.
	if _, err := g.Client.Charge(ctx, booking.ChargeRequest{IdempotencyKey: key, AmountPaise: 25000}); err != nil {
		return booking.ChargeResult{}, err
	}
	return g.Client.Void(ctx, key)
}

// 4. The void race: the late charge wins, so the void reports "succeeded" and
// the reconciler books (no ABANDONED, no lost money).
func TestReconciler_VoidRace_LateChargeWins_Books(t *testing.T) {
	e := setup(t, 1)
	h := e.abandonBeforeGateway("alice", 0)
	e.ageAllPendingPayments()
	racy := booking.NewService(e.repo, e.locker, raceGateway{e.gateway}, holdTTL)

	e.runOnce(e.reconciler(racy, 50))

	assert.Equal(t, "SUCCEEDED", e.str(`SELECT status FROM payments WHERE hold_id = $1`, h.ID))
	assert.Equal(t, 1, e.count(`SELECT COUNT(*) FROM bookings`))
	assert.Equal(t, 0, e.mock.Stats().TotalVoids, "the void found the charge, nothing was voided")
	e.assertInvariants()
}

// 5. Gateway unreachable: the payment stays PENDING, the error is recorded,
// nothing else changes.
func TestReconciler_GatewayDown_StaysPending(t *testing.T) {
	e := setup(t, 1)
	h := e.abandonAfterTimeout("alice", 0)
	e.ageAllPendingPayments()
	down := booking.NewService(e.repo, e.locker, deadGateway(), holdTTL)

	res := e.runOnce(e.reconciler(down, 50))

	assert.Equal(t, worker.Result{Claimed: 1, Failed: 1}, res)
	var status, lastError string
	var attempts int
	var later bool
	require.NoError(t, e.pool.QueryRow(context.Background(), `
		SELECT status, last_error, reconcile_attempts, next_check_at > NOW() FROM payments WHERE hold_id = $1`, h.ID).
		Scan(&status, &lastError, &attempts, &later))
	assert.Equal(t, "PENDING", status)
	assert.Contains(t, lastError, "payment gateway gave no answer")
	assert.Equal(t, 1, attempts)
	assert.True(t, later, "retried later, with backoff")
	assert.Equal(t, "ACTIVE", e.str(`SELECT status FROM holds WHERE id = $1`, h.ID))
	assert.Zero(t, e.count(`SELECT COUNT(*) FROM bookings`))
	assert.Equal(t, 0, e.mock.Stats().TotalVoids, "never void when the gateway did not answer")
}

// ========================================================== outbox drainer

// queueRefund produces one REFUND outbox row the way production does
// (abandoned payment, charge succeeded, hold expired, reconciler).
func (e *env) queueRefund(seat int) int64 {
	e.t.Helper()
	h := e.abandonAfterTimeout(fmt.Sprintf("user-%d", seat), seat)
	e.exec(`UPDATE holds SET expires_at = NOW() - interval '1 second' WHERE id = $1`, h.ID)
	e.ageAllPendingPayments()
	e.runOnce(e.reconciler(e.svc, 50))
	var id int64
	require.NoError(e.t, e.pool.QueryRow(context.Background(),
		`SELECT o.id FROM outbox o JOIN payments p ON (o.payload->>'payment_id')::bigint = p.id WHERE p.hold_id = $1`, h.ID).Scan(&id))
	return id
}

// 6. Refunds fail 3 times: attempts 1, 2, 3 with growing backoff; the 4th succeeds.
func TestDrainer_BackoffThenSuccess(t *testing.T) {
	e := setup(t, 1)
	outboxID := e.queueRefund(0)
	e.queueN(paymentmock.BehaviorRefundFail, 3)

	var delays []float64
	for run := 1; run <= 3; run++ {
		res := e.runOnce(e.drainer(e.svc, 50))
		require.Equal(t, 1, res.Failed, "run %d", run)
		var attempts int
		var delay float64
		require.NoError(t, e.pool.QueryRow(context.Background(),
			`SELECT attempts, EXTRACT(EPOCH FROM next_attempt_at - NOW()) FROM outbox WHERE id = $1`, outboxID).Scan(&attempts, &delay))
		assert.Equal(t, run, attempts)
		delays = append(delays, delay)
		e.exec(`UPDATE outbox SET next_attempt_at = NOW() WHERE id = $1`, outboxID) // "time passes"
	}
	t.Logf("backoff delays (s): %.1f, %.1f, %.1f", delays[0], delays[1], delays[2])
	assert.Greater(t, delays[1], delays[0])
	assert.Greater(t, delays[2], delays[1])

	res := e.runOnce(e.drainer(e.svc, 50))

	assert.Equal(t, 1, res.Succeeded)
	assert.Equal(t, "DONE", e.str(`SELECT status FROM outbox WHERE id = $1`, outboxID))
	assert.Equal(t, 1, e.mock.Stats().TotalRefunds)
	e.assertInvariants()
}

// 7. A refund that always fails becomes a FAILED dead letter and is not retried.
func TestDrainer_DeadLetter(t *testing.T) {
	e := setup(t, 1)
	e.repo.SetLimits(booking.Limits{PendingHoldMax: booking.DefaultLimits.PendingHoldMax, OutboxMaxAttempts: 3})
	outboxID := e.queueRefund(0)
	e.queueN(paymentmock.BehaviorRefundFail, 100)

	for run := 0; run < 3; run++ {
		e.runOnce(e.drainer(e.svc, 50))
		e.exec(`UPDATE outbox SET next_attempt_at = NOW() WHERE id = $1`, outboxID)
	}
	assert.Equal(t, "FAILED", e.str(`SELECT status FROM outbox WHERE id = $1`, outboxID))

	res := e.runOnce(e.drainer(e.svc, 50))
	assert.Zero(t, res.Claimed, "a dead letter is never retried automatically")
	assert.Equal(t, 0, e.mock.Stats().TotalRefunds)
}

// 8. RELEASE_LOCK while Redis is down: retried, and done once Redis is back.
func TestDrainer_ReleaseLock_RedisDownThenBack(t *testing.T) {
	e := setup(t, 1)
	h := e.hold("alice", 0)
	key := fmt.Sprintf("seat:%d:%d", e.showID, e.seatIDs[0])
	redisDown := booking.NewService(e.repo, deadLocker(), e.gateway, holdTTL)
	_, err := redisDown.CancelHold(context.Background(), "alice", h.ID) // inline release fails
	require.NoError(t, err)
	require.Equal(t, int64(1), e.rdb.Exists(context.Background(), key).Val(), "the lock is still in Redis")

	res := e.runOnce(e.drainer(redisDown, 50))
	assert.Equal(t, 1, res.Failed)
	assert.Equal(t, "PENDING", e.str(`SELECT status FROM outbox WHERE kind = 'RELEASE_LOCK'`))

	e.exec(`UPDATE outbox SET next_attempt_at = NOW()`) // the backoff has passed
	res = e.runOnce(e.drainer(e.svc, 50))               // Redis is back

	assert.Equal(t, 1, res.Succeeded)
	assert.Equal(t, "DONE", e.str(`SELECT status FROM outbox WHERE kind = 'RELEASE_LOCK'`))
	assert.Zero(t, e.rdb.Exists(context.Background(), key).Val(), "the lock is gone")
}

// ============================================================= hold expirer

// 9. The expirer expires a dead hold (with a RELEASE_LOCK row in the same
// transaction) and skips a hold whose payment is PENDING; after the drainer,
// another user can hold the freed seat.
func TestExpirer_ExpiresIdleHold_SkipsPaying_LockReleased(t *testing.T) {
	e := setup(t, 2)
	idle := e.hold("alice", 0)
	paying := e.abandonAfterTimeout("bob", 1)
	e.exec(`UPDATE holds SET expires_at = NOW() - interval '1 second'`)

	res := e.runOnce(&worker.HoldExpirer{Store: e.repo, Logger: e.logger, BatchSize: 50})

	assert.Equal(t, 1, res.Succeeded)
	assert.Equal(t, "EXPIRED", e.str(`SELECT status FROM holds WHERE id = $1`, idle.ID))
	assert.Equal(t, "ACTIVE", e.str(`SELECT status FROM holds WHERE id = $1`, paying.ID), "a paying hold is skipped")
	assert.Equal(t, 1, e.count(`SELECT COUNT(*) FROM outbox WHERE kind = 'RELEASE_LOCK' AND (payload->>'hold_id')::bigint = $1`, idle.ID))

	e.runOnce(e.drainer(e.svc, 50))
	_, created, err := e.svc.HoldSeat(context.Background(), booking.HoldSeatInput{UserID: "carol", ShowID: e.showID, SeatID: e.seatIDs[0]})
	require.NoError(t, err, "with the Redis lock released, another user can hold the seat")
	assert.True(t, created)
	e.assertInvariants()
}

// 9b. PENDING_HOLD_MAX: the gateway is unreachable for a long time. The seat
// frees after PENDING_HOLD_MAX; when the gateway comes back and reports the
// charge succeeded, the money is refunded and nothing is booked for the
// first user.
func TestPendingHoldMax_SeatFreesThenLateSuccessIsRefunded(t *testing.T) {
	e := setup(t, 1)
	e.repo.SetLimits(booking.Limits{PendingHoldMax: 2 * time.Second, OutboxMaxAttempts: 20})
	h := e.abandonAfterTimeout("alice", 0) // money taken, answer lost
	e.ageAllPendingPayments()
	gatewayDown := booking.NewService(e.repo, e.locker, deadGateway(), holdTTL)
	e.runOnce(e.reconciler(gatewayDown, 50)) // still unreachable
	assert.Equal(t, "PENDING", e.str(`SELECT status FROM payments WHERE hold_id = $1`, h.ID))

	// Time passes beyond expires_at + PENDING_HOLD_MAX.
	e.exec(`UPDATE holds SET expires_at = NOW() - interval '10 seconds' WHERE id = $1`, h.ID)
	e.runOnce(&worker.HoldExpirer{Store: e.repo, Logger: e.logger, BatchSize: 50})
	e.runOnce(e.drainer(e.svc, 50)) // releases alice's Redis lock
	assert.Equal(t, "EXPIRED", e.str(`SELECT status FROM holds WHERE id = $1`, h.ID))
	_, _, err := e.svc.HoldSeat(context.Background(), booking.HoldSeatInput{UserID: "bob", ShowID: e.showID, SeatID: e.seatIDs[0]})
	require.NoError(t, err, "the seat is free again")

	// The gateway is back and says: alice's charge succeeded.
	e.exec(`UPDATE payments SET next_check_at = NOW() WHERE hold_id = $1`, h.ID)
	e.runOnce(e.reconciler(e.svc, 50))
	e.runOnce(e.drainer(e.svc, 50))

	assert.Equal(t, "REFUNDED", e.str(`SELECT status FROM payments WHERE hold_id = $1`, h.ID))
	assert.Zero(t, e.count(`SELECT COUNT(*) FROM bookings`), "no booking for alice")
	assert.Equal(t, 1, e.mock.Stats().TotalRefunds)
	e.assertInvariants()
}

// ============================================================ two workers

// 10. Two reconcilers and then two drainers run AT THE SAME TIME on one
// backlog of 30 payments. Every row must be processed exactly once.
func TestTwoWorkers_ProcessEveryRowExactlyOnce(t *testing.T) {
	const n = 30
	e := setup(t, n)
	ctx := context.Background()

	// Backlog: 30 abandoned PENDING payments whose charges DID succeed at the
	// gateway, and whose holds have all expired, so each needs a refund.
	for i := 0; i < n; i++ {
		h := e.hold(fmt.Sprintf("user-%d", i), i)
		p, _, err := e.repo.BeginPayment(ctx, booking.ConfirmInput{UserID: fmt.Sprintf("user-%d", i), HoldID: h.ID})
		require.NoError(t, err)
		_, err = e.gateway.Charge(ctx, booking.ChargeRequest{IdempotencyKey: p.ChargeKey, AmountPaise: p.AmountPaise})
		require.NoError(t, err)
	}
	e.ageAllPendingPayments()
	e.exec(`UPDATE holds SET expires_at = NOW() - interval '1 second'`)

	runConcurrently := func(newJob func() worker.Job) {
		var wg sync.WaitGroup
		for w := 0; w < 2; w++ {
			wg.Add(1)
			go func(job worker.Job) {
				defer wg.Done()
				for {
					res, err := job.RunOnce(ctx)
					if !assert.NoError(t, err) || res.Claimed == 0 {
						return
					}
				}
			}(newJob())
		}
		wg.Wait()
	}

	runConcurrently(func() worker.Job { return e.reconciler(e.svc, 4) })
	assert.Equal(t, n, e.count(`SELECT COUNT(*) FROM payments WHERE status = 'REFUND_PENDING'`))
	assert.Equal(t, n, e.count(`SELECT COUNT(*) FROM payments WHERE reconcile_attempts = 1`), "each payment reconciled exactly once")
	assert.Equal(t, n, e.count(`SELECT COUNT(*) FROM outbox WHERE kind = 'REFUND'`), "one refund row per payment")

	runConcurrently(func() worker.Job { return e.drainer(e.svc, 4) })
	assert.Equal(t, n, e.count(`SELECT COUNT(*) FROM payments WHERE status = 'REFUNDED'`))
	assert.Equal(t, n, e.count(`SELECT COUNT(*) FROM outbox WHERE kind = 'REFUND' AND status = 'DONE' AND attempts = 1`),
		"each refund row processed exactly once")
	st := e.mock.Stats()
	assert.Equal(t, n, st.TotalRefunds, "one refund per payment at the gateway")
	assert.Zero(t, e.count(`SELECT COUNT(*) FROM bookings`), "no double (or any) booking")
	e.assertInvariants()
}

// ========================================================== key cleanup

// 11. The cleanup job deletes only old COMPLETED keys.
func TestKeyCleanup_DeletesOnlyOldCompletedKeys(t *testing.T) {
	e := setup(t, 1)
	ctx := context.Background()
	for _, k := range []string{"old-done", "new-done", "old-running"} {
		claim, err := e.keys.Begin(ctx, "alice", k, "h")
		require.NoError(t, err)
		if k != "old-running" {
			_, err := e.keys.Complete(ctx, "alice", k, claim.Token, idempotency.Response{Status: 201, Body: []byte(`{}`)})
			require.NoError(t, err)
		}
	}
	e.exec(`UPDATE idempotency_keys SET updated_at = NOW() - interval '2 days' WHERE key LIKE 'old-%'`)

	res := e.runOnce(&worker.KeyCleanup{Keys: e.keys, Logger: e.logger, Retention: 24 * time.Hour, BatchSize: 1})

	assert.Equal(t, 1, res.Succeeded)
	assert.Equal(t, 2, e.count(`SELECT COUNT(*) FROM idempotency_keys`))
	assert.Zero(t, e.count(`SELECT COUNT(*) FROM idempotency_keys WHERE key = 'old-done'`))
}
