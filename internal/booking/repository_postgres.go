// repository_postgres.go (with repository_holds.go) is the ONLY place in
// SeatLock that talks SQL.
//
// Where it fits: booking.Service calls these methods through the Repository
// interface (service.go). Each method runs hand-written queries through the
// shared pgx pool and converts database results and errors into domain types
// and domain errors. Raw database errors never leave these files unwrapped.
//
// This file: shows, seats, bookings and the shared error helpers.
// repository_holds.go: the hold / confirm / release transactions.
package booking

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL error codes we react to. Full list:
// https://www.postgresql.org/docs/16/errcodes-appendix.html
const (
	pgUniqueViolation     = "23505" // a UNIQUE constraint rejected the row
	pgForeignKeyViolation = "23503" // a FOREIGN KEY constraint rejected the row
)

// Constraint and index names from db/migrations/.
const (
	constraintBookingsShowSeatUnique = "bookings_show_seat_unique"
	constraintBookingsSeatFK         = "bookings_seat_fk"
	constraintBookingsHoldUnique     = "bookings_hold_unique"
	constraintHoldsSeatFK            = "holds_seat_fk"
	constraintHoldsOneActivePerSeat  = "holds_one_active_per_seat"
)

// dbError is the ONE place repository errors get wrapped. Every method sends
// unexpected errors through it, so timeout handling cannot be forgotten in one
// of them.
//
// A timeout (context.DeadlineExceeded) can happen in two places, and both mean
// "Postgres could not serve us in time":
//   - the query itself ran longer than queryTimeout;
//   - the query never started, because all pool connections were busy and we
//     ran out of time WAITING for one (pool acquire uses the same ctx).
//
// The same goes for Postgres being UNAVAILABLE for a moment (a restart, a
// failover, a dropped connection): see IsDBUnavailable.
//
// Those become ErrBusy. fmt.Errorf with two %w verbs wraps BOTH errors, so
// errors.Is(err, ErrBusy) is true for the HTTP layer AND the original pgx
// error is still there for the logs.
func dbError(err error, format string, args ...any) error {
	op := fmt.Sprintf(format, args...)
	if IsDBUnavailable(err) {
		return fmt.Errorf("%s: %w: %w", op, ErrBusy, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// IsDBUnavailable reports whether err means "Postgres could not serve this
// request RIGHT NOW", a temporary condition where the same request will
// probably succeed in a moment (503 SERVICE_BUSY), as opposed to a bug (500):
//
//   - a timeout: the query was too slow, or no pool connection was free in time;
//   - no connection could be made: Postgres is restarting ("the database
//     system is shutting down"), refusing connections, or its name does not
//     resolve yet (*pgconn.ConnectError);
//   - Postgres ended the session or cannot take it: SQLSTATE class 57
//     (57P01 admin_shutdown, 57P02 crash_shutdown, 57P03 cannot_connect_now),
//     class 08 (connection exception), class 53 (insufficient resources, e.g.
//     too many connections);
//   - the connection broke mid-query (connection reset: a network error).
//
// Found by chaos/postgres-restart.sh: before this, a Postgres restart under
// load gave clients 132 x 500 INTERNAL_ERROR in 3 seconds instead of 503.
// Data was never wrong; the status was. Exported so the idempotency store
// applies the same rule.
func IsDBUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return true
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		class := pgErr.Code[:min(2, len(pgErr.Code))]
		return class == "57" || class == "08" || class == "53"
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// PostgresRepository implements Repository using PostgreSQL.
type PostgresRepository struct {
	pool         *pgxpool.Pool
	queryTimeout time.Duration
	limits       Limits
}

// Limits are timing/retry rules shared by the API and the worker. Both read
// them from the same config, so both apply the SAME rules.
type Limits struct {
	// PendingHoldMax: an expired hold that still has a PENDING payment keeps
	// blocking its seat (its payment may yet succeed) for at most this long
	// after expires_at. Then it may be expired anyway; see deadHoldCondition.
	PendingHoldMax time.Duration
	// OutboxMaxAttempts: after this many attempts an outbox row becomes a
	// FAILED dead letter.
	OutboxMaxAttempts int
}

// DefaultLimits are used until SetLimits is called.
var DefaultLimits = Limits{PendingHoldMax: 10 * time.Minute, OutboxMaxAttempts: 20}

// SetLimits replaces the timing/retry rules (from config, or short ones in tests).
func (r *PostgresRepository) SetLimits(l Limits) { r.limits = l }

// Compile-time check that *PostgresRepository satisfies Repository. If a
// method is missing or has the wrong signature, the build fails here with a
// clear message, instead of somewhere far away in main.go.
var _ Repository = (*PostgresRepository)(nil)

// NewPostgresRepository returns a repository that runs queries on pool.
//
// queryTimeout (DB_QUERY_TIMEOUT, default 3s) caps how long one query or
// transaction may take. If Postgres is stuck (locks, overload), we give up
// with ErrBusy instead of letting requests pile up forever.
func NewPostgresRepository(pool *pgxpool.Pool, queryTimeout time.Duration) *PostgresRepository {
	return &PostgresRepository{pool: pool, queryTimeout: queryTimeout, limits: DefaultLimits}
}

// deadHoldCondition is THE rule for "this ACTIVE hold no longer blocks its
// seat", used by CreateHold (lazy expiry), the seat map and the worker's hold
// expirer, so all three always agree. A hold is dead when its time is up AND
// either it has no PENDING payment, or that payment has been unresolved for
// longer than PendingHoldMax past the hold's expiry.
//
// Why a PENDING payment keeps the seat blocked: the user is paying. Their
// charge may still succeed, so we do not hand the seat to someone else while
// the outcome is unknown. Why only for PendingHoldMax: if the gateway is
// unreachable for a long time, the seat must not be blocked forever. Freeing
// it then is SAFE, because the payment stays PENDING for the reconciler, and
// if the charge turns out to have succeeded, CompletePayment sees the expired
// hold and refunds (it never books an expired hold).
//
// It expects the hold's table alias to be "h" and $N to be PendingHoldMax in
// seconds, where N is the given placeholder number.
func deadHoldCondition(param int) string {
	return fmt.Sprintf(`(h.expires_at <= NOW()
	AND (NOT EXISTS (SELECT 1 FROM payments p WHERE p.hold_id = h.id AND p.status = 'PENDING')
	     OR h.expires_at + make_interval(secs => $%d) <= NOW()))`, param)
}

// ListShows returns all shows, earliest first.
func (r *PostgresRepository) ListShows(ctx context.Context) ([]Show, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	rows, err := r.pool.Query(ctx, `
		SELECT id, title, venue, starts_at, created_at
		FROM shows
		ORDER BY starts_at, id`)
	if err != nil {
		return nil, dbError(err, "list shows")
	}
	// rows holds a pooled connection until closed; defer guarantees we
	// give it back even if scanning fails halfway.
	defer rows.Close()

	shows := []Show{} // non-nil so an empty result becomes [] in JSON, not null
	for rows.Next() {
		var s Show
		if err := rows.Scan(&s.ID, &s.Title, &s.Venue, &s.StartsAt, &s.CreatedAt); err != nil {
			return nil, dbError(err, "scan show")
		}
		shows = append(shows, s)
	}
	// rows.Err reports any error that stopped the loop early (e.g. connection lost).
	if err := rows.Err(); err != nil {
		return nil, dbError(err, "list shows")
	}
	return shows, nil
}

// GetShow returns one show, or ErrShowNotFound.
func (r *PostgresRepository) GetShow(ctx context.Context, showID int64) (Show, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	var s Show
	err := r.pool.QueryRow(ctx, `
		SELECT id, title, venue, starts_at, created_at
		FROM shows
		WHERE id = $1`, showID).
		Scan(&s.ID, &s.Title, &s.Venue, &s.StartsAt, &s.CreatedAt)
	// $1 is a query parameter. The value is sent separately from the SQL text,
	// so it can never be interpreted as SQL (this prevents SQL injection).
	if errors.Is(err, pgx.ErrNoRows) {
		return Show{}, ErrShowNotFound
	}
	if err != nil {
		return Show{}, dbError(err, "get show %d", showID)
	}
	return s, nil
}

// GetSeat returns one seat, or ErrSeatNotFound.
func (r *PostgresRepository) GetSeat(ctx context.Context, seatID int64) (Seat, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	var s Seat
	err := r.pool.QueryRow(ctx, `
		SELECT id, show_id, seat_label, price_paise
		FROM seats
		WHERE id = $1`, seatID).
		Scan(&s.ID, &s.ShowID, &s.Label, &s.PricePaise)
	if errors.Is(err, pgx.ErrNoRows) {
		return Seat{}, ErrSeatNotFound
	}
	if err != nil {
		return Seat{}, dbError(err, "get seat %d", seatID)
	}
	return s, nil
}

// GetSeatInShow returns the seat seatID of show showID in ONE round trip:
// ErrShowNotFound if the show does not exist, ErrSeatNotFound if the seat
// does not, ErrSeatNotInShow if it belongs to another show (checked in that
// order, the same answers as GetShow followed by GetSeat).
//
// Phase 6: a hold used to call GetShow and then GetSeat, two pool acquires
// and two round trips before any real work. Under load the pool was the
// bottleneck (all connections busy, requests queuing for one), so the two
// lookups became one query. See docs/benchmarks/RESULTS.md.
func (r *PostgresRepository) GetSeatInShow(ctx context.Context, showID, seatID int64) (Seat, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	// Always exactly ONE row: "one" is a single-row table, the seat is
	// LEFT JOINed (NULLs when it does not exist), the show is an EXISTS.
	var (
		showExists          bool
		id, seatShow, price *int64
		label               *string
	)
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM shows WHERE id = $1),
		       s.id, s.show_id, s.seat_label, s.price_paise
		FROM (SELECT 1) AS one
		LEFT JOIN seats s ON s.id = $2`, showID, seatID).
		Scan(&showExists, &id, &seatShow, &label, &price)
	if err != nil {
		return Seat{}, dbError(err, "get seat %d of show %d", seatID, showID)
	}
	switch {
	case !showExists:
		return Seat{}, ErrShowNotFound
	case id == nil:
		return Seat{}, ErrSeatNotFound
	case *seatShow != showID:
		return Seat{}, ErrSeatNotInShow
	}
	return Seat{ID: *id, ShowID: *seatShow, Label: *label, PricePaise: *price}, nil
}

// ListSeatsWithStatus returns every seat of a show as AVAILABLE, HELD or
// BOOKED, or ErrShowNotFound if the show does not exist.
//
// One round trip: the query starts FROM shows, so "no rows" means "no such
// show", and a show without seats is one row of NULL seat columns. Phase 6:
// this used to be GetShow + this query, i.e. two pool acquires for the most
// frequent request (seat maps), when the pool was the bottleneck.
//
// The status comes from PostgreSQL ONLY, never from Redis: Postgres is the
// authority, and Redis may have lost keys or hold stale ones.
//
// LEFT JOIN keeps every seat, even those with no matching row; for those the
// joined id is NULL. A seat is HELD if its hold is ACTIVE and not dead by
// deadHoldCondition: not past expires_at, or past it but with a PENDING
// payment younger than PendingHoldMax. A dead hold that was not yet marked
// EXPIRED (the worker or the next hold attempt does that) shows as AVAILABLE.
// Both joins use indexes: bookings_show_seat_unique and the partial index
// holds_one_active_per_seat (which also guarantees at most one hold row per
// seat, so the join cannot duplicate seats).
func (r *PostgresRepository) ListSeatsWithStatus(ctx context.Context, showID int64) ([]SeatWithStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	rows, err := r.pool.Query(ctx, `
		SELECT s.id, s.show_id, s.seat_label, s.price_paise,
		       CASE
		           WHEN b.id IS NOT NULL THEN 'BOOKED'
		           WHEN h.id IS NOT NULL THEN 'HELD'
		           ELSE 'AVAILABLE'
		       END AS status
		FROM shows sh
		LEFT JOIN seats s    ON s.show_id = sh.id
		LEFT JOIN bookings b ON b.show_id = s.show_id AND b.seat_id = s.id
		LEFT JOIN holds h    ON h.show_id = s.show_id AND h.seat_id = s.id
		                    AND h.status = 'ACTIVE' AND NOT `+deadHoldCondition(2)+`
		WHERE sh.id = $1
		ORDER BY s.id`, showID, r.limits.PendingHoldMax.Seconds())
	if err != nil {
		return nil, dbError(err, "list seats for show %d", showID)
	}
	defer rows.Close()

	seats := []SeatWithStatus{}
	showFound := false
	for rows.Next() {
		showFound = true
		var (
			id, seatShow, price *int64
			label               *string
			status              string
		)
		if err := rows.Scan(&id, &seatShow, &label, &price, &status); err != nil {
			return nil, dbError(err, "scan seat")
		}
		if id == nil { // the show exists but has no seats
			continue
		}
		seats = append(seats, SeatWithStatus{
			Seat:   Seat{ID: *id, ShowID: *seatShow, Label: *label, PricePaise: *price},
			Status: SeatStatus(status),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, dbError(err, "list seats for show %d", showID)
	}
	if !showFound {
		return nil, ErrShowNotFound
	}
	return seats, nil
}

// mapInsertBookingError turns known constraint violations on the bookings
// insert (in CompletePayment) into domain errors.
//
// errors.As looks through the chain of wrapped errors for one of type
// *pgconn.PgError (the error pgx returns when Postgres itself rejects a
// statement) and, if found, stores it in pgErr so we can read its code.
func mapInsertBookingError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == constraintBookingsShowSeatUnique:
			return ErrSeatAlreadyBooked
		case pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == constraintBookingsHoldUnique:
			// A second booking from the same hold. FOR UPDATE in CompletePayment
			// prevents this; the constraint is the backstop.
			return ErrHoldAlreadyUsed
		case pgErr.Code == pgForeignKeyViolation &&
			(pgErr.ConstraintName == constraintBookingsSeatFK || pgErr.ConstraintName == constraintHoldsSeatFK):
			// The service already checks this; the database check is the backstop.
			return ErrSeatNotInShow
		}
	}
	return dbError(err, "insert booking")
}

// GetBooking returns one booking, or ErrBookingNotFound.
func (r *PostgresRepository) GetBooking(ctx context.Context, bookingID int64) (Booking, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	var b Booking
	err := r.pool.QueryRow(ctx, `
		SELECT id, show_id, seat_id, user_id, hold_id, created_at
		FROM bookings
		WHERE id = $1`, bookingID).
		Scan(&b.ID, &b.ShowID, &b.SeatID, &b.UserID, &b.HoldID, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, ErrBookingNotFound
	}
	if err != nil {
		return Booking{}, dbError(err, "get booking %d", bookingID)
	}
	return b, nil
}
