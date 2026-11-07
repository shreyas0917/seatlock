//go:build integration

// payments_integration_test.go is the Phase 3 proof, end to end over real
// HTTP: the production router, the real Postgres repository and idempotency
// store, real Redis, and the mock payment gateway (in-process, through
// httptest), whose behaviour each test controls.
//
// These are CORRECTNESS tests (exactly one charge, exactly one booking,
// refunds when needed), so they use generous timeouts; latency is a Phase 6
// load-test concern.
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shreyas0917/seatlock/internal/booking"
	"github.com/shreyas0917/seatlock/internal/config"
	"github.com/shreyas0917/seatlock/internal/db"
	"github.com/shreyas0917/seatlock/internal/idempotency"
	"github.com/shreyas0917/seatlock/internal/lock"
	"github.com/shreyas0917/seatlock/internal/payment"
	"github.com/shreyas0917/seatlock/internal/paymentmock"
)

// stackOptions tune one test stack.
type stackOptions struct {
	holdTTL        time.Duration // default 300s
	paymentTimeout time.Duration // default 2s
	slowDelay      time.Duration // default 1s (must be < paymentTimeout for "slow")
}

// stack is a running SeatLock API wired to real dependencies.
type stack struct {
	t       *testing.T
	pool    *pgxpool.Pool
	mock    *paymentmock.Server
	mockURL string
	url     string // API base URL
	client  *http.Client
	showID  int64
	seatIDs []int64
	// pieces reused to build a second router (e.g. with Redis down)
	repo    *booking.PostgresRepository
	gateway *payment.Client
	idem    *idempotency.Store
	opts    stackOptions
}

func newStack(t *testing.T, opts stackOptions) *stack {
	t.Helper()
	if opts.holdTTL == 0 {
		opts.holdTTL = 300 * time.Second
	}
	if opts.paymentTimeout == 0 {
		opts.paymentTimeout = 2 * time.Second
	}
	if opts.slowDelay == 0 {
		opts.slowDelay = time.Second
	}
	ctx := context.Background()

	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = defaultTestDatabaseURL
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
		VALUES ('Payment Show', 'Test Venue', now() + interval '7 days') RETURNING id`).Scan(&showID))
	var seatIDs []int64
	for _, label := range []string{"A1", "A2", "A3"} {
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO seats (show_id, seat_label, price_paise) VALUES ($1, $2, 25000) RETURNING id`,
			showID, label).Scan(&id))
		seatIDs = append(seatIDs, id)
	}

	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rdb := lock.NewClient(lock.Options{Addr: redisAddr, DB: 1,
		DialTimeout: testRedisTimeout, ReadTimeout: testRedisTimeout, WriteTimeout: testRedisTimeout})
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.FlushDB(ctx).Err(), "is redis running? try `make up`")

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	mock := paymentmock.New(paymentmock.Options{SlowDelay: opts.slowDelay, Logger: logger})
	mockServer := httptest.NewServer(mock)
	t.Cleanup(mockServer.Close)

	s := &stack{
		t: t, pool: pool, mock: mock, mockURL: mockServer.URL,
		client:  &http.Client{Timeout: 30 * time.Second},
		showID:  showID,
		seatIDs: seatIDs,
		repo:    booking.NewPostgresRepository(pool, testDBQueryTimeout),
		gateway: payment.NewClient(mockServer.URL, opts.paymentTimeout),
		idem:    idempotency.NewStore(pool, 30*time.Second, testDBQueryTimeout),
		opts:    opts,
	}
	locker := lock.NewRedisLocker(rdb, opts.holdTTL, 10*time.Second)
	s.url = s.serve(locker, pool)
	s.mockPost("/debug/reset", `{}`) // never inherit queued behaviours or stats
	return s
}

// serve starts an API server with the given locker and readiness pinger.
func (s *stack) serve(locker booking.SeatLocker, redisPinger Pinger) string {
	svc := booking.NewService(s.repo, locker, s.gateway, s.opts.holdTTL)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	server := httptest.NewServer(NewRouter(svc, s.idem, s.pool, redisPinger, logger))
	s.t.Cleanup(server.Close)
	return server.URL
}

// mockPost calls a /debug endpoint of the mock gateway.
func (s *stack) mockPost(path, body string) {
	s.t.Helper()
	resp, err := s.client.Post(s.mockURL+path, "application/json", strings.NewReader(body))
	require.NoError(s.t, err)
	resp.Body.Close()
	require.Equal(s.t, http.StatusOK, resp.StatusCode)
}

// queue forces the behaviour of the next NEW charge at the gateway.
func (s *stack) queue(behavior string) {
	s.mockPost("/debug/next-behavior", fmt.Sprintf(`{"behavior":%q,"count":1}`, behavior))
}

type apiResponse struct {
	status   int
	body     string
	replayed bool
}

func (s *stack) do(baseURL, method, path, userID, key, body string) apiResponse {
	s.t.Helper()
	req, err := http.NewRequest(method, baseURL+path, strings.NewReader(body))
	require.NoError(s.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", userID)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := s.client.Do(req)
	require.NoError(s.t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)
	return apiResponse{status: resp.StatusCode, body: string(data), replayed: resp.Header.Get("Idempotent-Replayed") == "true"}
}

// hold holds seat i (0-based) for the user and returns the hold id.
func (s *stack) hold(userID string, seat int) int64 {
	s.t.Helper()
	r := s.do(s.url, http.MethodPost, "/v1/holds", userID, "", fmt.Sprintf(`{"show_id":%d,"seat_id":%d}`, s.showID, s.seatIDs[seat]))
	require.Equal(s.t, http.StatusCreated, r.status, r.body)
	var h HoldResponse
	require.NoError(s.t, json.Unmarshal([]byte(r.body), &h))
	return h.HoldID
}

func (s *stack) confirm(baseURL, userID, key string, holdID int64) apiResponse {
	s.t.Helper()
	return s.do(baseURL, http.MethodPost, "/v1/bookings", userID, key, fmt.Sprintf(`{"hold_id":%d}`, holdID))
}

func (s *stack) count(query string, args ...any) int {
	s.t.Helper()
	var n int
	require.NoError(s.t, s.pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func (s *stack) value(query string, args ...any) string {
	s.t.Helper()
	var v string
	require.NoError(s.t, s.pool.QueryRow(context.Background(), query, args...).Scan(&v))
	return v
}

func errorCode(t *testing.T, body string) string {
	t.Helper()
	var e ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(body), &e), body)
	return e.Error.Code
}

// 1. Happy path.
func TestPay_HappyPath(t *testing.T) {
	s := newStack(t, stackOptions{})
	holdID := s.hold("alice", 0)

	r := s.confirm(s.url, "alice", "k-happy", holdID)

	require.Equal(t, http.StatusCreated, r.status, r.body)
	assert.Equal(t, "SUCCEEDED", s.value(`SELECT status FROM payments WHERE hold_id = $1`, holdID))
	assert.Equal(t, "CONSUMED", s.value(`SELECT status FROM holds WHERE id = $1`, holdID))
	assert.Equal(t, 1, s.mock.Stats().MoneyTaken)
}

// 2. The same key three times in a row.
func TestPay_SameKeyThreeTimes_ReplaysIdentically(t *testing.T) {
	s := newStack(t, stackOptions{})
	holdID := s.hold("alice", 0)

	r1 := s.confirm(s.url, "alice", "k-3x", holdID)
	r2 := s.confirm(s.url, "alice", "k-3x", holdID)
	r3 := s.confirm(s.url, "alice", "k-3x", holdID)

	require.Equal(t, http.StatusCreated, r1.status, r1.body)
	assert.False(t, r1.replayed)
	for _, r := range []apiResponse{r2, r3} {
		assert.Equal(t, http.StatusCreated, r.status)
		assert.True(t, r.replayed, "header Idempotent-Replayed: true")
		assert.Equal(t, r1.body, r.body, "byte-for-byte identical")
	}
	assert.Equal(t, 1, s.count(`SELECT COUNT(*) FROM bookings`))
	st := s.mock.Stats()
	assert.Equal(t, 1, st.TotalCharges, "1 charge")
	t.Logf("3 sends -> 1 booking; gateway: %d charge, %d charge request(s)", st.TotalCharges, st.ChargeRequests["pay-1"])
}

// 3. The same key, 10 requests at the same moment.
func TestPay_SameKeyConcurrent_OneChargeOneBooking(t *testing.T) {
	s := newStack(t, stackOptions{})
	holdID := s.hold("alice", 0)
	s.queue(paymentmock.BehaviorSlow) // keep the first request busy, so the others overlap it

	const n = 10
	results := make([]apiResponse, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = s.confirm(s.url, "alice", "k-burst", holdID)
		}(i)
	}
	close(start)
	wg.Wait()

	counts := map[string]int{}
	var created string
	for i, r := range results {
		switch {
		case r.status == http.StatusCreated:
			counts["201"]++
			if created == "" {
				created = r.body
			}
			assert.Equal(t, created, r.body, "every 201 carries the same booking")
		case r.status == http.StatusConflict && errorCode(t, r.body) == "REQUEST_IN_PROGRESS":
			counts["409 REQUEST_IN_PROGRESS"]++
		default:
			t.Errorf("request %d: unexpected %d: %s", i, r.status, r.body)
		}
	}
	t.Logf("10 concurrent requests, same key -> %v", counts)
	assert.GreaterOrEqual(t, counts["201"], 1)
	assert.Equal(t, 1, s.count(`SELECT COUNT(*) FROM bookings`), "exactly 1 booking")
	assert.Equal(t, 1, s.mock.Stats().TotalCharges, "exactly 1 charge")
}

// 4. The same key with a different hold.
func TestPay_SameKeyDifferentHold_422(t *testing.T) {
	s := newStack(t, stackOptions{})
	first := s.hold("alice", 0)
	second := s.hold("alice", 1)
	require.Equal(t, http.StatusCreated, s.confirm(s.url, "alice", "k-reuse", first).status)

	r := s.confirm(s.url, "alice", "k-reuse", second)

	assert.Equal(t, http.StatusUnprocessableEntity, r.status)
	assert.Equal(t, "IDEMPOTENCY_KEY_REUSED", errorCode(t, r.body))
}

// 5. Declined, then a retry with a new key.
func TestPay_DeclinedThenRetryWithNewKey(t *testing.T) {
	s := newStack(t, stackOptions{})
	holdID := s.hold("alice", 0)
	s.queue(paymentmock.BehaviorDecline)

	r := s.confirm(s.url, "alice", "k-decl-1", holdID)
	require.Equal(t, http.StatusPaymentRequired, r.status, r.body)
	assert.Equal(t, "PAYMENT_DECLINED", errorCode(t, r.body))
	assert.Equal(t, "ACTIVE", s.value(`SELECT status FROM holds WHERE id = $1`, holdID), "the hold is kept")

	// The same key replays the decline; a NEW key tries again.
	assert.Equal(t, http.StatusPaymentRequired, s.confirm(s.url, "alice", "k-decl-1", holdID).status)
	r = s.confirm(s.url, "alice", "k-decl-2", holdID)
	require.Equal(t, http.StatusCreated, r.status, r.body)

	st := s.mock.Stats()
	assert.Equal(t, 2, st.TotalCharges, "2 attempts")
	assert.Equal(t, 1, st.MoneyTaken, "1 successful charge")
	assert.Equal(t, 1, s.count(`SELECT COUNT(*) FROM payments WHERE status = 'DECLINED'`))
}

// 6. "Money taken, answer lost": timeout, then a retry with the same key.
func TestPay_TimeoutThenRetrySameKey_OneCharge(t *testing.T) {
	s := newStack(t, stackOptions{paymentTimeout: time.Second})
	holdID := s.hold("alice", 0)
	s.queue(paymentmock.BehaviorTimeout)

	r := s.confirm(s.url, "alice", "k-timeout", holdID)
	require.Equal(t, http.StatusGatewayTimeout, r.status, r.body)
	assert.Equal(t, "PAYMENT_PENDING", errorCode(t, r.body))
	assert.Equal(t, "PENDING", s.value(`SELECT status FROM payments WHERE hold_id = $1`, holdID), "unknown is not failed")
	assert.Zero(t, s.count(`SELECT COUNT(*) FROM bookings`))
	assert.Equal(t, 1, s.mock.Stats().MoneyTaken, "the gateway DID take the money")

	r = s.confirm(s.url, "alice", "k-timeout", holdID)

	require.Equal(t, http.StatusCreated, r.status, r.body)
	assert.False(t, r.replayed, "a 504 is never replayed; the retry ran the payment to the end")
	st := s.mock.Stats()
	assert.Equal(t, 1, st.TotalCharges, "still 1 charge")
	assert.Equal(t, 2, st.ChargeRequests["pay-1"], "the same charge key was sent twice")
	assert.Equal(t, 1, s.count(`SELECT COUNT(*) FROM bookings`))
}

// 7. The hold expires while the gateway is working.
func TestPay_HoldExpiresDuringPayment_Refunded(t *testing.T) {
	s := newStack(t, stackOptions{holdTTL: time.Second, slowDelay: 1500 * time.Millisecond, paymentTimeout: 3 * time.Second})
	holdID := s.hold("alice", 0)
	s.queue(paymentmock.BehaviorSlow) // 1.5s > 1s hold

	r := s.confirm(s.url, "alice", "k-expire", holdID)

	require.Equal(t, http.StatusGone, r.status, r.body)
	assert.Equal(t, "HOLD_EXPIRED", errorCode(t, r.body))
	assert.Contains(t, r.body, `"payment":"REFUNDED"`)
	assert.Equal(t, "REFUNDED", s.value(`SELECT status FROM payments WHERE hold_id = $1`, holdID))
	assert.Equal(t, "DONE", s.value(`SELECT status FROM outbox WHERE kind = 'REFUND'`))
	assert.Zero(t, s.count(`SELECT COUNT(*) FROM bookings`), "no booking")
	st := s.mock.Stats()
	assert.Equal(t, 1, st.MoneyTaken, "1 charge")
	assert.Equal(t, 1, st.TotalRefunds, "1 refund")

	// A retry with the same key replays the 410; nothing is charged or refunded again.
	again := s.confirm(s.url, "alice", "k-expire", holdID)
	assert.Equal(t, r.body, again.body)
	assert.Equal(t, 1, s.mock.Stats().TotalRefunds)
}

// 8. Redis down: confirm with payment still works (Option B).
func TestPay_RedisDown_ConfirmStillWorks(t *testing.T) {
	s := newStack(t, stackOptions{})
	holdID := s.hold("alice", 0)
	dead := lock.NewClient(lock.Options{Addr: "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond})
	downLocker := lock.NewRedisLocker(dead, s.opts.holdTTL, 10*time.Second)
	downURL := s.serve(downLocker, downLocker) // same Postgres + gateway, Redis unreachable

	newHold := s.do(downURL, http.MethodPost, "/v1/holds", "bob", "", fmt.Sprintf(`{"show_id":%d,"seat_id":%d}`, s.showID, s.seatIDs[1]))
	assert.Equal(t, http.StatusServiceUnavailable, newHold.status, "new holds fail closed")

	r := s.confirm(downURL, "alice", "k-redis-down", holdID)

	require.Equal(t, http.StatusCreated, r.status, r.body)
	assert.Equal(t, 1, s.mock.Stats().MoneyTaken)
}

// 9. No transaction or row lock is held while the gateway is called, and a
// cancel during payment is refused with 409 PAYMENT_IN_PROGRESS.
func TestPay_NoTransactionOpenDuringGatewayCall(t *testing.T) {
	s := newStack(t, stackOptions{slowDelay: 1500 * time.Millisecond, paymentTimeout: 5 * time.Second})
	holdID := s.hold("alice", 0)
	s.queue(paymentmock.BehaviorSlow)

	done := make(chan apiResponse, 1)
	go func() { done <- s.confirm(s.url, "alice", "k-nolock", holdID) }()

	// Wait until the request is INSIDE the gateway call.
	deadline := time.Now().Add(5 * time.Second)
	for s.mock.Stats().ChargeRequests["pay-1"] == 0 {
		require.True(t, time.Now().Before(deadline), "the charge never reached the gateway")
		time.Sleep(10 * time.Millisecond)
	}

	ctx := context.Background()
	// (a) The hold and seat rows can be locked right now. NOWAIT fails
	//     immediately if any other transaction holds a lock on them.
	tx, err := s.pool.Begin(ctx)
	require.NoError(t, err)
	var id int64
	require.NoError(t, tx.QueryRow(ctx, `SELECT id FROM holds WHERE id = $1 FOR UPDATE NOWAIT`, holdID).Scan(&id),
		"the hold row must not be locked during the gateway call")
	require.NoError(t, tx.QueryRow(ctx, `SELECT id FROM seats WHERE id = $1 FOR UPDATE NOWAIT`, s.seatIDs[0]).Scan(&id),
		"the seat row must not be locked during the gateway call")
	require.NoError(t, tx.Rollback(ctx))

	// (b) No session of the test database sits "idle in transaction"
	//     (excluding this query's own connection).
	idleInTx := s.count(`
		SELECT COUNT(*) FROM pg_stat_activity
		WHERE datname = current_database() AND state LIKE 'idle in transaction%'
		  AND pid <> pg_backend_pid()`)
	assert.Zero(t, idleInTx, "no transaction may be open while waiting for the gateway")

	// (c) Cancelling the hold mid-payment is refused.
	cancelResp := s.do(s.url, http.MethodDelete, fmt.Sprintf("/v1/holds/%d", holdID), "alice", "", "")
	assert.Equal(t, http.StatusConflict, cancelResp.status)
	assert.Equal(t, "PAYMENT_IN_PROGRESS", errorCode(t, cancelResp.body))

	r := <-done
	assert.Equal(t, http.StatusCreated, r.status, r.body)
}

// 11. Two confirms for one hold with DIFFERENT keys, at the same time: they
// share the hold's single PENDING payment (same charge_key), so there is one
// charge; Tx B never refunds a payment that backs a booking, so both get the
// same booking and there are no refunds.
func TestPay_TwoKeysSameHoldConcurrent_OneBookingNoRefund(t *testing.T) {
	s := newStack(t, stackOptions{})
	holdID := s.hold("alice", 0)
	s.queue(paymentmock.BehaviorSlow) // the first charge is slow, so the two overlap

	results := make([]apiResponse, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, key := range []string{"k-tab-1", "k-tab-2"} {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			<-start
			results[i] = s.confirm(s.url, "alice", key, holdID)
		}(i, key)
	}
	close(start)
	wg.Wait()

	for i, r := range results {
		assert.Equal(t, http.StatusCreated, r.status, "confirm %d: %s", i, r.body)
	}
	var b1, b2 BookingResponse
	require.NoError(t, json.Unmarshal([]byte(results[0].body), &b1))
	require.NoError(t, json.Unmarshal([]byte(results[1].body), &b2))
	assert.Equal(t, b1.ID, b2.ID, "both get the SAME booking")
	assert.Equal(t, 1, s.count(`SELECT COUNT(*) FROM bookings`))
	st := s.mock.Stats()
	assert.Equal(t, 1, st.TotalCharges, "1 charge")
	assert.Zero(t, st.TotalRefunds, "0 refunds")
	assert.Zero(t, s.count(`SELECT COUNT(*) FROM payments WHERE status IN ('REFUND_PENDING', 'REFUNDED')`))
}
