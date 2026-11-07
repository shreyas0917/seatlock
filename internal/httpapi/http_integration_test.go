//go:build integration

// http_integration_test.go fires real HTTP requests at the real router, wired
// to the real Postgres repository and the real Redis locker, using the
// DEFAULT pool settings from internal/config (DB_MAX_CONNS / DB_MIN_CONNS)
// and NO warm-up.
//
// Why: a burst of requests against a small pool used to queue for
// connections until the 3s query timeout fired, and that showed up as 500s.
// This test proves a burst gets only "normal" answers (201/200/409). Any 503
// or 500 is printed in full so it can be investigated, never hidden.
//
// Run with: make test-integration (needs `make up`). Redis database 1 is used
// and emptied first, so the app's data in database 0 is untouched.
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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shreyas0917/seatlock/internal/booking"
	"github.com/shreyas0917/seatlock/internal/config"
	"github.com/shreyas0917/seatlock/internal/db"
	"github.com/shreyas0917/seatlock/internal/idempotency"
	"github.com/shreyas0917/seatlock/internal/lock"
	"github.com/shreyas0917/seatlock/internal/metrics"
	"github.com/shreyas0917/seatlock/internal/payment"
	"github.com/shreyas0917/seatlock/internal/paymentmock"
)

const defaultTestDatabaseURL = "postgres://seatlock:seatlock@localhost:5434/seatlock_test?sslmode=disable"

// Generous timeouts for integration tests. These tests check CORRECTNESS
// (one winner, no double booking), not speed. On a busy machine the
// production timeouts (3s DB, 200ms Redis) turned a slow-but-correct run into
// a failure. Latency is measured separately, by load tests (Phase 6).
const (
	testDBQueryTimeout = 15 * time.Second
	testRedisTimeout   = 2 * time.Second
)

// httpResult is what one parallel request saw.
type httpResult struct {
	status int
	body   string
	err    error
}

func TestHTTP_ParallelHoldBurstHasNo500s(t *testing.T) {
	ctx := context.Background()

	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = defaultTestDatabaseURL
	}
	require.NoError(t, db.MigrateUp(dbURL), "is postgres running? try `make up`")

	// The same defaults the real server uses: no special test-only pool size.
	defaults, err := config.Load()
	require.NoError(t, err)
	pool, err := db.NewPool(ctx, dbURL, defaults.DBMaxConns, defaults.DBMinConns)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	t.Logf("pool: max_conns=%d min_conns=%d", defaults.DBMaxConns, defaults.DBMinConns)

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
	// Like a long-running server: wait until the pool has its idle connections open.
	deadline := time.Now().Add(5 * time.Second)
	for rdb.PoolStats().IdleConns < uint32(defaults.RedisMinIdleConns) {
		require.True(t, time.Now().Before(deadline), "redis pool never warmed up")
		time.Sleep(10 * time.Millisecond)
	}
	locker := lock.NewRedisLocker(rdb, defaults.HoldTTL, defaults.RedisLockExtra)

	// Fresh data: one show with one seat.
	_, err = pool.Exec(ctx, `TRUNCATE bookings, payments, holds, seats, shows, outbox, idempotency_keys RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	var showID, seatID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO shows (title, venue, starts_at)
		VALUES ('Burst Show', 'Test Venue', now() + interval '7 days') RETURNING id`).Scan(&showID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO seats (show_id, seat_label, price_paise)
		VALUES ($1, 'A1', 25000) RETURNING id`, showID).Scan(&seatID))

	// A real HTTP server on a random local port, with the production router.
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	mockServer := httptest.NewServer(paymentmock.New(paymentmock.Options{Logger: logger}))
	t.Cleanup(mockServer.Close)
	gateway := payment.NewClient(mockServer.URL, 3*time.Second)
	svc := booking.NewService(booking.NewPostgresRepository(pool, testDBQueryTimeout), locker, gateway, defaults.HoldTTL)
	idem := idempotency.NewStore(pool, 30*time.Second, testDBQueryTimeout)
	server := httptest.NewServer(NewRouter(svc, idem, pool, locker, logger))
	t.Cleanup(server.Close)

	// Allow 50 open connections from the client so the requests really run in parallel.
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 50, MaxConnsPerHost: 50}}
	post := func(path, userID, body string) httpResult {
		req, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
		if err != nil {
			return httpResult{err: err}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-User-ID", userID)
		req.Header.Set("Idempotency-Key", "burst-"+userID) // only the confirm needs it; holds ignore it
		resp, err := client.Do(req)
		if err != nil {
			return httpResult{err: err}
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return httpResult{status: resp.StatusCode, body: string(respBody)}
	}

	const requests = 50
	holdBody := fmt.Sprintf(`{"show_id": %d, "seat_id": %d}`, showID, seatID)
	results := make([]httpResult, requests)
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = post("/v1/holds", fmt.Sprintf("burst-user-%d", i), holdBody)
		}(i)
	}
	close(start)
	wg.Wait()

	counts := map[int]int{}
	winner := -1
	for i, r := range results {
		if r.err != nil {
			t.Errorf("request %d failed at the transport level: %v", i, r.err)
			continue
		}
		counts[r.status]++
		switch r.status {
		case http.StatusCreated:
			winner = i
		case http.StatusOK, http.StatusConflict:
			// expected
		default:
			// Print the full body: it contains the error code (e.g. SERVICE_BUSY).
			t.Errorf("request %d: unexpected status %d, body: %s", i, r.status, r.body)
		}
	}

	t.Logf("%d parallel hold requests -> status counts: %v (503s: %d, 500s: %d)",
		requests, counts, counts[http.StatusServiceUnavailable], counts[http.StatusInternalServerError])
	assert.Equal(t, 1, counts[http.StatusCreated], "exactly one hold must win")
	assert.Equal(t, requests-1, counts[http.StatusConflict], "everyone else gets 409")
	assert.Zero(t, counts[http.StatusInternalServerError], "a burst must never produce 500s")
	require.GreaterOrEqual(t, winner, 0)

	// The winner confirms its hold over HTTP.
	var hold HoldResponse
	require.NoError(t, json.Unmarshal([]byte(results[winner].body), &hold))
	confirm := post("/v1/bookings", fmt.Sprintf("burst-user-%d", winner), fmt.Sprintf(`{"hold_id": %d}`, hold.HoldID))
	require.NoError(t, confirm.err)
	assert.Equal(t, http.StatusCreated, confirm.status, confirm.body)
}

// TestMetricsEndpoint_ExposesExpectedNames: the internal metrics handler
// (what cmd/api serves on METRICS_ADDR) exposes HTTP and pool metrics after
// real traffic, and the public router does not serve /metrics at all.
func TestMetricsEndpoint_ExposesExpectedNames(t *testing.T) {
	ctx := context.Background()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = defaultTestDatabaseURL
	}
	require.NoError(t, db.MigrateUp(dbURL))
	defaults, err := config.Load()
	require.NoError(t, err)
	pool, err := db.NewPool(ctx, dbURL, defaults.DBMaxConns, defaults.DBMinConns)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rdb := lock.NewClient(lock.Options{Addr: redisAddr, DB: 1, DialTimeout: testRedisTimeout, ReadTimeout: testRedisTimeout, WriteTimeout: testRedisTimeout})
	t.Cleanup(func() { _ = rdb.Close() })

	m := metrics.NewAPI()
	m.RegisterDBPool(pool)
	m.RegisterRedisPool(rdb)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	locker := lock.NewRedisLocker(rdb, defaults.HoldTTL, defaults.RedisLockExtra)
	svc := booking.NewService(booking.NewPostgresRepository(pool, testDBQueryTimeout), locker, nil, defaults.HoldTTL)
	public := httptest.NewServer(NewRouter(svc, nil, pool, locker, logger, WithMetrics(m)))
	t.Cleanup(public.Close)
	internal := httptest.NewServer(m.Handler())
	t.Cleanup(internal.Close)

	resp, err := http.Get(public.URL + "/v1/shows")
	require.NoError(t, err)
	resp.Body.Close()
	resp, err = http.Get(public.URL + "/metrics")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "the public port must not serve /metrics")

	resp, err = http.Get(internal.URL)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	for _, name := range []string{
		`seatlock_http_requests_total{method="GET",route="/v1/shows",status="200"}`,
		"seatlock_http_request_duration_seconds_bucket",
		"seatlock_http_in_flight_requests",
		`seatlock_db_pool_connections{state="total"}`,
		"seatlock_db_pool_acquire_wait_seconds_total",
		`seatlock_redis_pool_connections{state="idle"}`,
		"seatlock_redis_pool_hits_total",
		"go_goroutines",
		"process_resident_memory_bytes",
	} {
		assert.Contains(t, string(body), name)
	}
}
