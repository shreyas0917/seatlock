// repository_postgres.go is the ONLY place in SeatLock that talks SQL.
//
// Where it fits: booking.Service calls these methods through the Repository
// interface (service.go). Each method runs one hand-written query through the
// shared pgx pool and converts database results and errors into domain types
// and domain errors. Raw database errors never leave this file unwrapped.
package booking

import (
	"context"
	"errors"
	"fmt"
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

// Constraint names from db/migrations/0001_init.up.sql.
const (
	constraintBookingsShowSeatUnique = "bookings_show_seat_unique"
	constraintBookingsSeatFK         = "bookings_seat_fk"
)

// defaultQueryTimeout caps how long any single query may take. If Postgres is
// stuck (locks, overload), we give up and return an error instead of letting
// requests pile up forever.
const defaultQueryTimeout = 3 * time.Second

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
// Those become ErrBusy. fmt.Errorf with two %w verbs wraps BOTH errors, so
// errors.Is(err, ErrBusy) is true for the HTTP layer AND the original pgx
// error is still there for the logs.
func dbError(err error, format string, args ...any) error {
	op := fmt.Sprintf(format, args...)
	if errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return fmt.Errorf("%s: %w: %w", op, ErrBusy, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// PostgresRepository implements Repository using PostgreSQL.
type PostgresRepository struct {
	pool         *pgxpool.Pool
	queryTimeout time.Duration
}

// Compile-time check that *PostgresRepository satisfies Repository. If a
// method is missing or has the wrong signature, the build fails here with a
// clear message, instead of somewhere far away in main.go.
var _ Repository = (*PostgresRepository)(nil)

// NewPostgresRepository returns a repository that runs queries on pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool, queryTimeout: defaultQueryTimeout}
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

// ListSeatsWithStatus returns every seat of a show and whether it is booked.
//
// LEFT JOIN keeps every seat, even those with no matching booking; for those,
// b.id is NULL and the seat is AVAILABLE. The join uses the
// bookings_show_seat_unique index, so it stays fast.
func (r *PostgresRepository) ListSeatsWithStatus(ctx context.Context, showID int64) ([]SeatWithStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	rows, err := r.pool.Query(ctx, `
		SELECT s.id, s.show_id, s.seat_label, s.price_paise,
		       CASE WHEN b.id IS NULL THEN 'AVAILABLE' ELSE 'BOOKED' END AS status
		FROM seats s
		LEFT JOIN bookings b ON b.show_id = s.show_id AND b.seat_id = s.id
		WHERE s.show_id = $1
		ORDER BY s.id`, showID)
	if err != nil {
		return nil, dbError(err, "list seats for show %d", showID)
	}
	defer rows.Close()

	seats := []SeatWithStatus{}
	for rows.Next() {
		var s SeatWithStatus
		var status string
		if err := rows.Scan(&s.ID, &s.ShowID, &s.Label, &s.PricePaise, &status); err != nil {
			return nil, dbError(err, "scan seat")
		}
		s.Status = SeatStatus(status)
		seats = append(seats, s)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError(err, "list seats for show %d", showID)
	}
	return seats, nil
}

// CreateBooking inserts a booking and lets PostgreSQL decide if the seat is free.
//
// There is deliberately NO "SELECT ... is it booked?" before the INSERT (see
// the timeline in service.go). The INSERT is a single atomic operation: if two
// requests insert the same (show_id, seat_id) at the same moment, Postgres
// lets one commit and makes the other fail with error 23505 on the
// bookings_show_seat_unique constraint. We translate that into
// ErrSeatAlreadyBooked, which the HTTP layer returns as 409 Conflict.
func (r *PostgresRepository) CreateBooking(ctx context.Context, in BookSeatInput) (Booking, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	var b Booking
	err := r.pool.QueryRow(ctx, `
		INSERT INTO bookings (show_id, seat_id, user_id)
		VALUES ($1, $2, $3)
		RETURNING id, show_id, seat_id, user_id, created_at`,
		in.ShowID, in.SeatID, in.UserID).
		Scan(&b.ID, &b.ShowID, &b.SeatID, &b.UserID, &b.CreatedAt)
	if err != nil {
		return Booking{}, mapInsertBookingError(err)
	}
	return b, nil
}

// mapInsertBookingError turns known constraint violations into domain errors.
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
		case pgErr.Code == pgForeignKeyViolation && pgErr.ConstraintName == constraintBookingsSeatFK:
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
		SELECT id, show_id, seat_id, user_id, created_at
		FROM bookings
		WHERE id = $1`, bookingID).
		Scan(&b.ID, &b.ShowID, &b.SeatID, &b.UserID, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, ErrBookingNotFound
	}
	if err != nil {
		return Booking{}, dbError(err, "get booking %d", bookingID)
	}
	return b, nil
}
