//go:build integration

// repository_postgres_test.go tests the real SQL against the real PostgreSQL
// started by docker compose, including THE PHASE 1 CONCURRENCY PROOF:
// 50 bookings race for one seat and the UNIQUE constraint lets exactly one win.
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

	"github.com/shreyas0917/seatlock/internal/config"
	"github.com/shreyas0917/seatlock/internal/db"
)

const defaultTestDatabaseURL = "postgres://seatlock:seatlock@localhost:5434/seatlock_test?sslmode=disable"

// Generous timeouts for integration tests. These tests check CORRECTNESS
// (one winner, no double booking), not speed. On a busy machine the
// production timeouts (3s DB, 200ms Redis) turned a slow-but-correct run into
// a failure. Latency is measured separately, by load tests (Phase 6).
const testDBQueryTimeout = 15 * time.Second

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
//
// The pool uses the SAME default size as the real server (DB_MAX_CONNS /
// DB_MIN_CONNS from internal/config), with no warm-up, so the tests see the
// same connection pressure production would.
func setupTestDB(t *testing.T) testFixture {
	// t.Helper marks this as a helper, so failures point at the calling test line.
	t.Helper()
	ctx := context.Background()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = defaultTestDatabaseURL
	}
	require.NoError(t, db.MigrateUp(url), "is postgres running? try `make up`")

	defaults, err := config.Load()
	require.NoError(t, err)
	pool, err := db.NewPool(ctx, url, defaults.DBMaxConns, defaults.DBMinConns)
	require.NoError(t, err)
	// t.Cleanup runs after the test finishes (pass or fail), like defer for tests.
	t.Cleanup(pool.Close)

	// Clean setup: empty all tables and restart ID counters.
	_, err = pool.Exec(ctx, `TRUNCATE bookings, payments, holds, seats, shows, outbox, idempotency_keys RESTART IDENTITY CASCADE`)
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

	return testFixture{pool: pool, repo: NewPostgresRepository(pool, testDBQueryTimeout), showID: showID, seatIDs: seatIDs}
}

// payAndBook simulates a confirm whose charge succeeded: Tx A (BeginPayment)
// then Tx B (CompletePayment), with no gateway in between.
func payAndBook(ctx context.Context, f testFixture, in ConfirmInput) (Booking, error) {
	p, _, err := f.repo.BeginPayment(ctx, in)
	if err != nil {
		return Booking{}, err
	}
	res, err := f.repo.CompletePayment(ctx, p.ID, "ch_test")
	if err != nil {
		return Booking{}, err
	}
	if res.Kind != Booked {
		return Booking{}, &HoldExpiredError{PaymentStatus: res.Payment.Status}
	}
	return res.Booking, nil
}

// bookSeat books a seat the only way that exists now: hold, then pay and book.
func bookSeat(t *testing.T, f testFixture, userID string, seatID int64) Booking {
	t.Helper()
	ctx := context.Background()
	h, _, err := f.repo.CreateHold(ctx, HoldSeatInput{UserID: userID, ShowID: f.showID, SeatID: seatID}, testHoldTTL)
	require.NoError(t, err)
	b, err := payAndBook(ctx, f, ConfirmInput{UserID: userID, HoldID: h.ID})
	require.NoError(t, err)
	return b
}

// insertConsumedHold writes a CONSUMED hold row directly with SQL. Several
// CONSUMED holds may exist for one seat (the partial unique index only covers
// ACTIVE ones). That lets the tests below aim many bookings at one seat and
// prove the bookings table's own UNIQUE constraint still guards it.
func insertConsumedHold(t *testing.T, f testFixture, userID string, seatID int64) int64 {
	t.Helper()
	var id int64
	require.NoError(t, f.pool.QueryRow(context.Background(), `
		INSERT INTO holds (show_id, seat_id, user_id, status, expires_at)
		VALUES ($1, $2, $3, 'CONSUMED', NOW()) RETURNING id`, f.showID, seatID, userID).Scan(&id))
	return id
}

// insertBooking runs the same INSERT as CompletePayment and maps its errors the
// same way, but without the hold checks in front of it.
func insertBooking(f testFixture, userID string, showID, seatID, holdID int64) error {
	_, err := f.pool.Exec(context.Background(), `
		INSERT INTO bookings (show_id, seat_id, user_id, hold_id) VALUES ($1, $2, $3, $4)`,
		showID, seatID, userID, holdID)
	if err != nil {
		return mapInsertBookingError(err)
	}
	return nil
}

func TestPostgres_BookAndGetBooking(t *testing.T) {
	f := setupTestDB(t)

	created := bookSeat(t, f, "alice", f.seatIDs[0])
	assert.NotZero(t, created.ID)
	assert.WithinDuration(t, time.Now(), created.CreatedAt, time.Minute)

	got, err := f.repo.GetBooking(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, created.HoldID, got.HoldID)
	assert.Equal(t, "alice", got.UserID)
}

// The Phase 1 safety net still holds: even if a second booking for a booked
// seat got past every hold check, bookings_show_seat_unique (23505) stops it.
func TestPostgres_SecondBookingForSeatReturnsErrSeatAlreadyBooked(t *testing.T) {
	f := setupTestDB(t)
	bookSeat(t, f, "alice", f.seatIDs[0])
	bobHold := insertConsumedHold(t, f, "bob", f.seatIDs[0])

	err := insertBooking(f, "bob", f.showID, f.seatIDs[0], bobHold)

	assert.ErrorIs(t, err, ErrSeatAlreadyBooked)
}

// A hold can produce only one booking (bookings_hold_unique).
func TestPostgres_SecondBookingFromSameHoldReturnsErrHoldAlreadyUsed(t *testing.T) {
	f := setupTestDB(t)
	b := bookSeat(t, f, "alice", f.seatIDs[0])

	// Same hold, different seat: only the hold_id constraint can catch this.
	err := insertBooking(f, "alice", f.showID, f.seatIDs[1], b.HoldID)

	assert.ErrorIs(t, err, ErrHoldAlreadyUsed)
}

func TestPostgres_ForeignKeyRejectsSeatFromAnotherShow(t *testing.T) {
	f := setupTestDB(t)
	holdID := insertConsumedHold(t, f, "alice", f.seatIDs[0])

	// Bypass the service and insert a mismatched (show, seat) pair directly:
	// the composite foreign key in the database must still refuse it.
	err := insertBooking(f, "alice", f.showID+1000, f.seatIDs[0], holdID)

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

	bookSeat(t, f, "alice", f.seatIDs[1])

	seats, err := f.repo.ListSeatsWithStatus(ctx, f.showID)
	require.NoError(t, err)
	require.Len(t, seats, 3)
	assert.Equal(t, SeatAvailable, seats[0].Status)
	assert.Equal(t, SeatBooked, seats[1].Status)
	assert.Equal(t, SeatAvailable, seats[2].Status)
	assert.Equal(t, "A2", seats[1].Label)
	assert.Equal(t, int64(25000), seats[1].PricePaise)
}

// TestConcurrentBookingSameSeat is THE PHASE 1 CONCURRENCY PROOF, kept in
// Phase 2. It tests the LAST line of defence: the bookings table itself.
//
// Scenario: 50 bookings for the same seat are inserted at the same instant,
// each from its own (consumed) hold, so none of the hold checks can stop
// them. Expected: exactly 1 succeeds, 49 get ErrSeatAlreadyBooked, and the
// bookings table contains exactly 1 row for that seat.
//
// (The Phase 2 races through the full hold -> confirm flow with real Redis
// are in service_integration_test.go: TestHoldRace and TestConfirmRace.)
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
	ctx := context.Background()

	const attempts = 50
	targetSeat := f.seatIDs[0]

	holdIDs := make([]int64, attempts)
	for i := range holdIDs {
		holdIDs[i] = insertConsumedHold(t, f, fmt.Sprintf("user-%d", i), targetSeat)
	}

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

			results[i] = insertBooking(f, fmt.Sprintf("user-%d", i), f.showID, targetSeat, holdIDs[i])
		}(i)
	}

	close(start) // fire! all 50 goroutines insert at once
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
