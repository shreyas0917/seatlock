//go:build integration

// repository_postgres_test.go tests the real SQL against the real PostgreSQL
// started by docker compose, including THE CONCURRENCY PROOF:
// 50 goroutines race for one seat and exactly one wins.
//
// The "//go:build integration" line at the top means this file is only
// compiled when tests run with `-tags=integration` (make test-integration).
// Plain `make test` skips it, so unit tests never need a database.
//
// It uses a separate database (seatlock_test, created by db/init/), so the
// TRUNCATE in setup never touches your seeded demo data.
package booking

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shreyas0917/seatlock/internal/db"
)

const defaultTestDatabaseURL = "postgres://seatlock:seatlock@localhost:5434/seatlock_test?sslmode=disable"

// testFixture is what every integration test starts with: a clean database
// containing one show with three seats.
type testFixture struct {
	pool    *pgxpool.Pool
	repo    *PostgresRepository
	showID  int64
	seatIDs []int64
}

// setupTestDB migrates the test database, wipes all rows, inserts fresh data,
// and registers cleanup to run when the test ends.
func setupTestDB(t *testing.T) testFixture {
	// t.Helper marks this as a helper, so failures point at the calling test line.
	t.Helper()
	ctx := context.Background()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = defaultTestDatabaseURL
	}
	require.NoError(t, db.MigrateUp(url), "is postgres running? try `make up`")

	// A larger pool than default so 50 goroutines can really hit Postgres at once.
	cfg, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)
	cfg.MaxConns = 60
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	// t.Cleanup runs after the test finishes (pass or fail), like defer for tests.
	t.Cleanup(pool.Close)

	// Clean setup: empty all tables and restart ID counters.
	_, err = pool.Exec(ctx, `TRUNCATE bookings, holds, seats, shows RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	var showID int64
	err = pool.QueryRow(ctx, `
		INSERT INTO shows (title, venue, starts_at)
		VALUES ('Test Show', 'Test Venue', now() + interval '7 days')
		RETURNING id`).Scan(&showID)
	require.NoError(t, err)

	var seatIDs []int64
	for _, label := range []string{"A1", "A2", "A3"} {
		var id int64
		err = pool.QueryRow(ctx, `
			INSERT INTO seats (show_id, seat_label, price_paise)
			VALUES ($1, $2, 25000) RETURNING id`, showID, label).Scan(&id)
		require.NoError(t, err)
		seatIDs = append(seatIDs, id)
	}

	return testFixture{pool: pool, repo: NewPostgresRepository(pool), showID: showID, seatIDs: seatIDs}
}

// warmUpPool makes the pool open n connections now: it borrows n at once,
// then gives them all back. They stay open in the pool, ready to use.
func warmUpPool(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conns := make([]*pgxpool.Conn, 0, n)
	for i := 0; i < n; i++ {
		conn, err := pool.Acquire(ctx)
		require.NoError(t, err)
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		conn.Release()
	}
}

func TestPostgres_CreateAndGetBooking(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()

	created, err := f.repo.CreateBooking(ctx, BookSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[0]})
	require.NoError(t, err)
	assert.NotZero(t, created.ID)
	assert.WithinDuration(t, time.Now(), created.CreatedAt, time.Minute)

	got, err := f.repo.GetBooking(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, "alice", got.UserID)
}

func TestPostgres_DuplicateBookingReturnsErrSeatAlreadyBooked(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()

	_, err := f.repo.CreateBooking(ctx, BookSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[0]})
	require.NoError(t, err)

	_, err = f.repo.CreateBooking(ctx, BookSeatInput{UserID: "bob", ShowID: f.showID, SeatID: f.seatIDs[0]})
	assert.ErrorIs(t, err, ErrSeatAlreadyBooked)
}

func TestPostgres_ForeignKeyRejectsSeatFromAnotherShow(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()

	// Bypass the service and insert a mismatched pair directly: the composite
	// foreign key in the database must still refuse it.
	_, err := f.repo.CreateBooking(ctx, BookSeatInput{UserID: "alice", ShowID: f.showID + 1000, SeatID: f.seatIDs[0]})
	assert.ErrorIs(t, err, ErrSeatNotInShow)
}

func TestPostgres_NotFoundErrors(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()

	_, err := f.repo.GetShow(ctx, 999)
	assert.ErrorIs(t, err, ErrShowNotFound)
	_, err = f.repo.GetSeat(ctx, 999)
	assert.ErrorIs(t, err, ErrSeatNotFound)
	_, err = f.repo.GetBooking(ctx, 999)
	assert.ErrorIs(t, err, ErrBookingNotFound)
}

func TestPostgres_ListShowsAndSeatMap(t *testing.T) {
	f := setupTestDB(t)
	ctx := context.Background()

	shows, err := f.repo.ListShows(ctx)
	require.NoError(t, err)
	require.Len(t, shows, 1)
	assert.Equal(t, "Test Show", shows[0].Title)

	_, err = f.repo.CreateBooking(ctx, BookSeatInput{UserID: "alice", ShowID: f.showID, SeatID: f.seatIDs[1]})
	require.NoError(t, err)

	seats, err := f.repo.ListSeatsWithStatus(ctx, f.showID)
	require.NoError(t, err)
	require.Len(t, seats, 3)
	assert.Equal(t, SeatAvailable, seats[0].Status)
	assert.Equal(t, SeatBooked, seats[1].Status)
	assert.Equal(t, SeatAvailable, seats[2].Status)
	assert.Equal(t, "A2", seats[1].Label)
	assert.Equal(t, int64(25000), seats[1].PricePaise)
}

// TestConcurrentBookingSameSeat is THE CONCURRENCY PROOF.
//
// Scenario: 50 users click "Book" on the same seat at the same instant.
// Expected: exactly 1 succeeds, 49 get ErrSeatAlreadyBooked, and the bookings
// table contains exactly 1 row for that seat. No double booking, ever.
//
// It goes through the full Service (validation + repository + Postgres), the
// same path a real HTTP request takes.
//
// Go concepts used:
//   - goroutine: `go func() {...}()` runs the function concurrently with the
//     rest of the program. Goroutines are very cheap (a few KB each).
//   - channel: `start` is a channel. Receiving from it (<-start) blocks until
//     it is closed. Closing it wakes up ALL waiting goroutines at once, which
//     is how we make all 50 really start together (a "starting gun").
//   - sync.WaitGroup: a counter. wg.Add(1) before starting each goroutine,
//     wg.Done() when it finishes, wg.Wait() blocks until the counter is 0,
//     i.e. until every goroutine has finished.
func TestConcurrentBookingSameSeat(t *testing.T) {
	f := setupTestDB(t)
	svc := NewService(f.repo)
	ctx := context.Background()

	const attempts = 50
	targetSeat := f.seatIDs[0]

	// Open all 50 database connections BEFORE the race. Otherwise every
	// goroutine has to open a brand-new connection at the same instant, and
	// on a slow machine (e.g. Docker Desktop) that handshake burst can eat the
	// repository's 3s query timeout. We want to test the race inside
	// Postgres, not how fast TCP connections open.
	warmUpPool(t, f.pool, attempts)

	start := make(chan struct{}) // the starting gun
	var wg sync.WaitGroup

	// Each goroutine writes its result into its own slot of this slice.
	// Different goroutines never write the same index, so no lock is needed.
	results := make([]error, attempts)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // wait here until the gun fires

			_, err := svc.BookSeat(ctx, BookSeatInput{
				UserID: fmt.Sprintf("user-%d", i), // 50 different users
				ShowID: f.showID,
				SeatID: targetSeat,
			})
			results[i] = err
		}(i)
	}

	close(start) // fire! all 50 goroutines start booking at once
	wg.Wait()    // wait for all of them to finish

	successes, conflicts := 0, 0
	for i, err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrSeatAlreadyBooked) {
			conflicts++
		} else {
			t.Errorf("attempt %d failed with an unexpected error: %v", i, err)
		}
	}

	assert.Equal(t, 1, successes, "exactly one booking must win")
	assert.Equal(t, attempts-1, conflicts, "every other attempt must get ErrSeatAlreadyBooked")

	// Do not trust only the return values: check the database itself.
	var rowsForSeat int
	err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM bookings WHERE show_id = $1 AND seat_id = $2`,
		f.showID, targetSeat).Scan(&rowsForSeat)
	require.NoError(t, err)
	assert.Equal(t, 1, rowsForSeat, "the bookings table must contain exactly one row for the seat")

	t.Logf("%d concurrent attempts -> %d success, %d ErrSeatAlreadyBooked, %d row in bookings",
		attempts, successes, conflicts, rowsForSeat)
}
