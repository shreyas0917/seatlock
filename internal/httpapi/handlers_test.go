// handlers_test.go tests the HTTP layer with net/http/httptest: no real
// server and no database.
//
// The handlers get the REAL booking.Service, backed by stubRepository below,
// so a request goes through the same handler -> service -> error-mapper path
// as in production. Only the SQL is replaced.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shreyas0917/seatlock/internal/booking"
)

// stubRepository knows show 1 with seat 7. createErr, when set, is returned
// by CreateBooking to simulate the database rejecting the insert.
type stubRepository struct {
	createErr error
}

func (s *stubRepository) ListShows(ctx context.Context) ([]booking.Show, error) {
	return []booking.Show{{ID: 1, Title: "Coldplay Live"}}, nil
}

func (s *stubRepository) GetShow(ctx context.Context, showID int64) (booking.Show, error) {
	if showID != 1 {
		return booking.Show{}, booking.ErrShowNotFound
	}
	return booking.Show{ID: 1, Title: "Coldplay Live"}, nil
}

func (s *stubRepository) GetSeat(ctx context.Context, seatID int64) (booking.Seat, error) {
	if seatID != 7 {
		return booking.Seat{}, booking.ErrSeatNotFound
	}
	return booking.Seat{ID: 7, ShowID: 1, Label: "A7", PricePaise: 25000}, nil
}

func (s *stubRepository) ListSeatsWithStatus(ctx context.Context, showID int64) ([]booking.SeatWithStatus, error) {
	return []booking.SeatWithStatus{
		{Seat: booking.Seat{ID: 7, ShowID: 1, Label: "A7", PricePaise: 25000}, Status: booking.SeatAvailable},
	}, nil
}

func (s *stubRepository) CreateBooking(ctx context.Context, in booking.BookSeatInput) (booking.Booking, error) {
	if s.createErr != nil {
		return booking.Booking{}, s.createErr
	}
	return booking.Booking{ID: 1, ShowID: in.ShowID, SeatID: in.SeatID, UserID: in.UserID, CreatedAt: time.Now()}, nil
}

func (s *stubRepository) GetBooking(ctx context.Context, bookingID int64) (booking.Booking, error) {
	return booking.Booking{}, booking.ErrBookingNotFound
}

// stubPinger simulates the database for /readyz.
type stubPinger struct{ err error }

func (p stubPinger) Ping(ctx context.Context) error { return p.err }

// newTestRouter builds the full router with the stubs. Logs are discarded.
func newTestRouter(repo *stubRepository, pinger Pinger) http.Handler {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return NewRouter(booking.NewService(repo), pinger, logger)
}

// postBooking sends POST /v1/bookings and returns the recorded response.
// httptest.NewRecorder is a fake ResponseWriter that stores what the handler wrote.
func postBooking(t *testing.T, router http.Handler, userID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("X-User-ID", userID)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// decodeErrorCode reads the "code" out of the standard error shape.
func decodeErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Error.Code
}

func TestCreateBooking_201Created(t *testing.T) {
	router := newTestRouter(&stubRepository{}, stubPinger{})

	rec := postBooking(t, router, "alice", `{"show_id": 1, "seat_id": 7}`)

	require.Equal(t, http.StatusCreated, rec.Code)
	var resp BookingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, int64(1), resp.ShowID)
	assert.Equal(t, int64(7), resp.SeatID)
	assert.Equal(t, "alice", resp.UserID)
	assert.NotEmpty(t, rec.Header().Get("X-Request-ID"), "every response carries its request ID")
}

func TestCreateBooking_400MissingUserID(t *testing.T) {
	router := newTestRouter(&stubRepository{}, stubPinger{})

	rec := postBooking(t, router, "", `{"show_id": 1, "seat_id": 7}`)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "MISSING_USER_ID", decodeErrorCode(t, rec))
}

func TestCreateBooking_400BadInput(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"not json", `hello`, "INVALID_INPUT"},
		{"unknown field", `{"show_id": 1, "seatid": 7}`, "INVALID_INPUT"},
		{"zero seat id", `{"show_id": 1, "seat_id": 0}`, "INVALID_INPUT"},
		{"negative show id", `{"show_id": -1, "seat_id": 7}`, "INVALID_INPUT"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := newTestRouter(&stubRepository{}, stubPinger{})

			rec := postBooking(t, router, "alice", tc.body)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, tc.wantCode, decodeErrorCode(t, rec))
		})
	}
}

func TestCreateBooking_404NotFound(t *testing.T) {
	router := newTestRouter(&stubRepository{}, stubPinger{})

	rec := postBooking(t, router, "alice", `{"show_id": 1, "seat_id": 999}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "SEAT_NOT_FOUND", decodeErrorCode(t, rec))

	rec = postBooking(t, router, "alice", `{"show_id": 999, "seat_id": 7}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "SHOW_NOT_FOUND", decodeErrorCode(t, rec))
}

func TestCreateBooking_409SeatAlreadyBooked(t *testing.T) {
	// Simulate Postgres rejecting the insert because the seat is taken.
	router := newTestRouter(&stubRepository{createErr: booking.ErrSeatAlreadyBooked}, stubPinger{})

	rec := postBooking(t, router, "bob", `{"show_id": 1, "seat_id": 7}`)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "SEAT_ALREADY_BOOKED", decodeErrorCode(t, rec))
}

func TestCreateBooking_500HidesInternalError(t *testing.T) {
	dbErr := errors.New(`pq: relation "bookings" does not exist on host db-prod-1`)
	router := newTestRouter(&stubRepository{createErr: dbErr}, stubPinger{})

	rec := postBooking(t, router, "alice", `{"show_id": 1, "seat_id": 7}`)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "INTERNAL_ERROR", decodeErrorCode(t, rec))
	assert.NotContains(t, rec.Body.String(), "db-prod-1", "raw database errors must never reach the client")
}

func TestCreateBooking_503ServiceBusy(t *testing.T) {
	// Simulate the repository timing out (slow query or no free pool connection).
	busy := fmt.Errorf("insert booking: %w: %w", booking.ErrBusy, context.DeadlineExceeded)
	router := newTestRouter(&stubRepository{createErr: busy}, stubPinger{})

	rec := postBooking(t, router, "alice", `{"show_id": 1, "seat_id": 7}`)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "SERVICE_BUSY", decodeErrorCode(t, rec))
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
}

func TestListSeats(t *testing.T) {
	router := newTestRouter(&stubRepository{}, stubPinger{})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/shows/1/seats", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp SeatMapResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Seats, 1)
	assert.Equal(t, "A7", resp.Seats[0].Label)
	assert.Equal(t, "AVAILABLE", resp.Seats[0].Status)

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/shows/42/seats", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/shows/abc/seats", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGetBooking_404(t *testing.T) {
	router := newTestRouter(&stubRepository{}, stubPinger{})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bookings/5", nil))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "BOOKING_NOT_FOUND", decodeErrorCode(t, rec))
}

func TestReadyz(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestRouter(&stubRepository{}, stubPinger{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	down := stubPinger{err: errors.New("connection refused")}
	newTestRouter(&stubRepository{}, down).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
