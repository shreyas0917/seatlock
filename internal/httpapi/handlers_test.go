// handlers_test.go tests the HTTP layer with net/http/httptest: no real
// server, no database, no Redis.
//
// The handlers get the REAL booking.Service, backed by stubRepository and
// stubLocker below, so a request goes through the same handler -> service ->
// error-mapper path as in production. Only the SQL and Redis are replaced.
// There is at least one test per status code the API can return.
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

// stubRepository knows show 1 with seat 7. The *Err fields, when set, are
// returned by the matching method to simulate the database rejecting.
type stubRepository struct {
	createHoldErr   error
	existingHold    bool // CreateHold returns created=false (a same-user retry)
	confirmErr      error
	releaseErr      error
	getBookingFound bool
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
		{Seat: booking.Seat{ID: 7, ShowID: 1, Label: "A7", PricePaise: 25000}, Status: booking.SeatHeld},
	}, nil
}

func (s *stubRepository) GetBooking(ctx context.Context, bookingID int64) (booking.Booking, error) {
	if !s.getBookingFound {
		return booking.Booking{}, booking.ErrBookingNotFound
	}
	return booking.Booking{ID: bookingID, ShowID: 1, SeatID: 7, UserID: "alice", HoldID: 12}, nil
}

func (s *stubRepository) CreateHold(ctx context.Context, in booking.HoldSeatInput, ttl time.Duration) (booking.Hold, bool, error) {
	if s.createHoldErr != nil {
		return booking.Hold{}, false, s.createHoldErr
	}
	h := booking.Hold{ID: 12, ShowID: in.ShowID, SeatID: in.SeatID, UserID: in.UserID,
		Status: booking.HoldActive, ExpiresAt: time.Now().Add(ttl), SecondsLeft: int64(ttl.Seconds())}
	return h, !s.existingHold, nil
}

func (s *stubRepository) ConfirmHold(ctx context.Context, in booking.ConfirmInput) (booking.Booking, error) {
	if s.confirmErr != nil {
		return booking.Booking{}, s.confirmErr
	}
	return booking.Booking{ID: 1, ShowID: 1, SeatID: 7, UserID: in.UserID, HoldID: in.HoldID, CreatedAt: time.Now()}, nil
}

func (s *stubRepository) ReleaseHold(ctx context.Context, userID string, holdID int64) (booking.Hold, error) {
	if s.releaseErr != nil {
		return booking.Hold{}, s.releaseErr
	}
	return booking.Hold{ID: holdID, ShowID: 1, SeatID: 7, UserID: userID, Status: booking.HoldReleased}, nil
}

// stubLocker is a SeatLocker. acquireErr simulates Redis being down; heldBy
// simulates another user holding the Redis lock.
type stubLocker struct {
	acquireErr error
	heldBy     string
}

func (l stubLocker) Acquire(ctx context.Context, showID, seatID int64, owner string) (bool, error) {
	if l.acquireErr != nil {
		return false, l.acquireErr
	}
	return l.heldBy == "" || l.heldBy == owner, nil
}

func (l stubLocker) Release(ctx context.Context, showID, seatID int64, owner string) error {
	return nil
}

// stubPinger simulates a dependency for /readyz.
type stubPinger struct{ err error }

func (p stubPinger) Ping(ctx context.Context) error { return p.err }

// newTestRouter builds the full router with the stubs. Logs are discarded.
func newTestRouter(repo *stubRepository, locker stubLocker) http.Handler {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc := booking.NewService(repo, locker, 300*time.Second)
	return NewRouter(svc, stubPinger{}, stubPinger{}, logger)
}

// send performs one request against the router and returns the recorded
// response. httptest.NewRecorder is a fake ResponseWriter that stores what the
// handler wrote.
func send(t *testing.T, router http.Handler, method, path, userID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
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
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp.Error.Code
}

const holdBody = `{"show_id": 1, "seat_id": 7}`

// ---------- POST /v1/holds ----------

func TestCreateHold_201Created(t *testing.T) {
	rec := send(t, newTestRouter(&stubRepository{}, stubLocker{}), http.MethodPost, "/v1/holds", "alice", holdBody)

	require.Equal(t, http.StatusCreated, rec.Code)
	var resp HoldResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, int64(12), resp.HoldID)
	assert.Equal(t, "ACTIVE", resp.Status)
	assert.Equal(t, int64(300), resp.SecondsLeft)
	assert.NotEmpty(t, rec.Header().Get("X-Request-ID"), "every response carries its request ID")
}

func TestCreateHold_200ExistingHold(t *testing.T) {
	rec := send(t, newTestRouter(&stubRepository{existingHold: true}, stubLocker{}), http.MethodPost, "/v1/holds", "alice", holdBody)

	assert.Equal(t, http.StatusOK, rec.Code, "a retry returns the same hold with 200, not 201")
}

func TestCreateHold_400(t *testing.T) {
	tests := []struct {
		name, userID, body, wantCode string
	}{
		{"missing user", "", holdBody, "MISSING_USER_ID"},
		{"not json", "alice", `hello`, "INVALID_INPUT"},
		{"unknown field", "alice", `{"show_id": 1, "seatid": 7}`, "INVALID_INPUT"},
		{"zero seat id", "alice", `{"show_id": 1, "seat_id": 0}`, "INVALID_INPUT"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := send(t, newTestRouter(&stubRepository{}, stubLocker{}), http.MethodPost, "/v1/holds", tc.userID, tc.body)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, tc.wantCode, decodeErrorCode(t, rec))
		})
	}
}

func TestCreateHold_404(t *testing.T) {
	router := newTestRouter(&stubRepository{}, stubLocker{})

	rec := send(t, router, http.MethodPost, "/v1/holds", "alice", `{"show_id": 1, "seat_id": 999}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "SEAT_NOT_FOUND", decodeErrorCode(t, rec))

	rec = send(t, router, http.MethodPost, "/v1/holds", "alice", `{"show_id": 999, "seat_id": 7}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "SHOW_NOT_FOUND", decodeErrorCode(t, rec))
}

func TestCreateHold_409(t *testing.T) {
	// Redis says another user holds the seat.
	rec := send(t, newTestRouter(&stubRepository{}, stubLocker{heldBy: "bob"}), http.MethodPost, "/v1/holds", "alice", holdBody)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "SEAT_HELD", decodeErrorCode(t, rec))

	// Postgres says the seat is already booked.
	rec = send(t, newTestRouter(&stubRepository{createHoldErr: booking.ErrSeatAlreadyBooked}, stubLocker{}),
		http.MethodPost, "/v1/holds", "alice", holdBody)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "SEAT_ALREADY_BOOKED", decodeErrorCode(t, rec))
}

func TestCreateHold_503LockUnavailable(t *testing.T) {
	redisDown := stubLocker{acquireErr: fmt.Errorf("%w: dial tcp: connection refused", booking.ErrLockUnavailable)}

	rec := send(t, newTestRouter(&stubRepository{}, redisDown), http.MethodPost, "/v1/holds", "alice", holdBody)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "LOCK_UNAVAILABLE", decodeErrorCode(t, rec))
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	assert.NotContains(t, rec.Body.String(), "dial tcp", "internal error details stay in the logs")
}

func TestCreateHold_503ServiceBusy(t *testing.T) {
	busy := fmt.Errorf("lock seat 7: %w: %w", booking.ErrBusy, context.DeadlineExceeded)

	rec := send(t, newTestRouter(&stubRepository{createHoldErr: busy}, stubLocker{}), http.MethodPost, "/v1/holds", "alice", holdBody)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "SERVICE_BUSY", decodeErrorCode(t, rec))
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
}

func TestCreateHold_500HidesInternalError(t *testing.T) {
	dbErr := errors.New(`relation "holds" does not exist on host db-prod-1`)

	rec := send(t, newTestRouter(&stubRepository{createHoldErr: dbErr}, stubLocker{}), http.MethodPost, "/v1/holds", "alice", holdBody)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "INTERNAL_ERROR", decodeErrorCode(t, rec))
	assert.NotContains(t, rec.Body.String(), "db-prod-1", "raw database errors must never reach the client")
}

// ---------- POST /v1/bookings (confirm) ----------

func TestConfirmBooking_201Created(t *testing.T) {
	rec := send(t, newTestRouter(&stubRepository{}, stubLocker{}), http.MethodPost, "/v1/bookings", "alice", `{"hold_id": 12}`)

	require.Equal(t, http.StatusCreated, rec.Code)
	var resp BookingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, int64(12), resp.HoldID)
	assert.Equal(t, "alice", resp.UserID)
}

func TestConfirmBooking_201EvenWhenRedisIsDown(t *testing.T) {
	redisDown := stubLocker{acquireErr: booking.ErrLockUnavailable}

	rec := send(t, newTestRouter(&stubRepository{}, redisDown), http.MethodPost, "/v1/bookings", "alice", `{"hold_id": 12}`)

	assert.Equal(t, http.StatusCreated, rec.Code, "confirm uses Postgres only")
}

func TestConfirmBooking_Errors(t *testing.T) {
	tests := []struct {
		name       string
		repo       *stubRepository
		userID     string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"missing user", &stubRepository{}, "", `{"hold_id": 12}`, http.StatusBadRequest, "MISSING_USER_ID"},
		{"bad hold id", &stubRepository{}, "alice", `{"hold_id": 0}`, http.StatusBadRequest, "INVALID_INPUT"},
		{"old phase 1 body", &stubRepository{}, "alice", `{"show_id": 1, "seat_id": 7}`, http.StatusBadRequest, "INVALID_INPUT"},
		{"hold not found", &stubRepository{confirmErr: booking.ErrHoldNotFound}, "alice", `{"hold_id": 12}`, http.StatusNotFound, "HOLD_NOT_FOUND"},
		{"hold already used", &stubRepository{confirmErr: booking.ErrHoldAlreadyUsed}, "alice", `{"hold_id": 12}`, http.StatusConflict, "HOLD_ALREADY_USED"},
		{"seat already booked", &stubRepository{confirmErr: booking.ErrSeatAlreadyBooked}, "alice", `{"hold_id": 12}`, http.StatusConflict, "SEAT_ALREADY_BOOKED"},
		{"hold expired", &stubRepository{confirmErr: booking.ErrHoldExpired}, "alice", `{"hold_id": 12}`, http.StatusGone, "HOLD_EXPIRED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := send(t, newTestRouter(tc.repo, stubLocker{}), http.MethodPost, "/v1/bookings", tc.userID, tc.body)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, tc.wantCode, decodeErrorCode(t, rec))
		})
	}
}

// ---------- DELETE /v1/holds/{holdID} ----------

func TestCancelHold(t *testing.T) {
	rec := send(t, newTestRouter(&stubRepository{}, stubLocker{}), http.MethodDelete, "/v1/holds/12", "alice", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var resp HoldResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "RELEASED", resp.Status)

	rec = send(t, newTestRouter(&stubRepository{releaseErr: booking.ErrHoldNotFound}, stubLocker{}), http.MethodDelete, "/v1/holds/12", "alice", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = send(t, newTestRouter(&stubRepository{releaseErr: booking.ErrHoldExpired}, stubLocker{}), http.MethodDelete, "/v1/holds/12", "alice", "")
	assert.Equal(t, http.StatusGone, rec.Code)

	rec = send(t, newTestRouter(&stubRepository{}, stubLocker{}), http.MethodDelete, "/v1/holds/abc", "alice", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = send(t, newTestRouter(&stubRepository{}, stubLocker{}), http.MethodDelete, "/v1/holds/12", "", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "MISSING_USER_ID", decodeErrorCode(t, rec))
}

// ---------- reads ----------

func TestListSeats(t *testing.T) {
	router := newTestRouter(&stubRepository{}, stubLocker{})

	rec := send(t, router, http.MethodGet, "/v1/shows/1/seats", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var resp SeatMapResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Seats, 1)
	assert.Equal(t, "A7", resp.Seats[0].Label)
	assert.Equal(t, "HELD", resp.Seats[0].Status)

	assert.Equal(t, http.StatusNotFound, send(t, router, http.MethodGet, "/v1/shows/42/seats", "", "").Code)
	assert.Equal(t, http.StatusBadRequest, send(t, router, http.MethodGet, "/v1/shows/abc/seats", "", "").Code)
}

func TestGetBooking(t *testing.T) {
	rec := send(t, newTestRouter(&stubRepository{getBookingFound: true}, stubLocker{}), http.MethodGet, "/v1/bookings/5", "", "")
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = send(t, newTestRouter(&stubRepository{}, stubLocker{}), http.MethodGet, "/v1/bookings/5", "", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "BOOKING_NOT_FOUND", decodeErrorCode(t, rec))
}

// ---------- /readyz ----------

func TestReadyz(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc := booking.NewService(&stubRepository{}, stubLocker{}, time.Minute)
	down := stubPinger{err: errors.New("connection refused")}

	tests := []struct {
		name           string
		postgres       Pinger
		redis          Pinger
		wantStatus     int
		wantPG, wantRD string
	}{
		{"all up", stubPinger{}, stubPinger{}, http.StatusOK, "up", "up"},
		{"redis down is still ready", stubPinger{}, down, http.StatusOK, "up", "down"},
		{"postgres down is not ready", down, stubPinger{}, http.StatusServiceUnavailable, "down", "up"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := NewRouter(svc, tc.postgres, tc.redis, logger)

			rec := send(t, router, http.MethodGet, "/readyz", "", "")

			assert.Equal(t, tc.wantStatus, rec.Code)
			var resp ReadinessResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, tc.wantPG, resp.Postgres)
			assert.Equal(t, tc.wantRD, resp.Redis)
		})
	}
}
