//go:build integration

// http_integration_test.go fires real HTTP requests at the real router, wired
// to the real Postgres repository, using the DEFAULT pool settings from
// internal/config (DB_MAX_CONNS / DB_MIN_CONNS) and NO warm-up.
//
// Why: a burst of requests against a small pool used to queue for
// connections until the 3s query timeout fired, and that showed up as 500s.
// This test proves a burst gets only "normal" answers. Any 503 or 500 is
// printed in full so it can be investigated, never hidden.
//
// Run with: make test-integration (needs `make up`).
package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shreyas0917/seatlock/internal/booking"
	"github.com/shreyas0917/seatlock/internal/config"
	"github.com/shreyas0917/seatlock/internal/db"
)

const defaultTestDatabaseURL = "postgres://seatlock:seatlock@localhost:5434/seatlock_test?sslmode=disable"

// httpResult is what one parallel request saw.
type httpResult struct {
	status int
	body   string
	err    error
}

func TestHTTP_ParallelBurstHasNo500s(t *testing.T) {
	ctx := context.Background()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = defaultTestDatabaseURL
	}
	require.NoError(t, db.MigrateUp(url), "is postgres running? try `make up`")

	// The same defaults the real server uses: no special test-only pool size.
	defaults, err := config.Load()
	require.NoError(t, err)
	pool, err := db.NewPool(ctx, url, defaults.DBMaxConns, defaults.DBMinConns)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	t.Logf("pool: max_conns=%d min_conns=%d", defaults.DBMaxConns, defaults.DBMinConns)

	// Fresh data: one show with one seat.
	_, err = pool.Exec(ctx, `TRUNCATE bookings, seats, shows RESTART IDENTITY CASCADE`)
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
	router := NewRouter(booking.NewService(booking.NewPostgresRepository(pool)), pool, logger)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	// Allow 50 open connections from the client so the requests really run in parallel.
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 50, MaxConnsPerHost: 50}}

	const requests = 50
	body := fmt.Sprintf(`{"show_id": %d, "seat_id": %d}`, showID, seatID)
	results := make([]httpResult, requests)
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start

			req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/bookings", strings.NewReader(body))
			if err != nil {
				results[i] = httpResult{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-ID", fmt.Sprintf("burst-user-%d", i))

			resp, err := client.Do(req)
			if err != nil {
				results[i] = httpResult{err: err}
				return
			}
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)
			results[i] = httpResult{status: resp.StatusCode, body: string(respBody)}
		}(i)
	}

	close(start)
	wg.Wait()

	counts := map[int]int{}
	for i, r := range results {
		if r.err != nil {
			t.Errorf("request %d failed at the transport level: %v", i, r.err)
			continue
		}
		counts[r.status]++
		switch r.status {
		case http.StatusCreated, http.StatusOK, http.StatusConflict:
			// expected
		default:
			// Print the full body: it contains the error code (e.g. SERVICE_BUSY).
			t.Errorf("request %d: unexpected status %d, body: %s", i, r.status, r.body)
		}
	}

	t.Logf("%d parallel requests -> status counts: %v (503s: %d, 500s: %d)",
		requests, counts, counts[http.StatusServiceUnavailable], counts[http.StatusInternalServerError])
	assert.Equal(t, 1, counts[http.StatusCreated], "exactly one booking must win")
	assert.Equal(t, requests-1, counts[http.StatusConflict], "everyone else gets 409")
	assert.Zero(t, counts[http.StatusInternalServerError], "a burst must never produce 500s")
}
