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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shreyas0917/seatlock/internal/booking"
	"github.com/shreyas0917/seatlock/internal/idempotency"
	"github.com/shreyas0917/seatlock/internal/metrics"
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

// BeginPayment + CompletePayment: confirmErr (if set) is returned by
// BeginPayment, so the confirm endpoint answers with that error.
func (s *stubRepository) BeginPayment(ctx context.Context, in booking.ConfirmInput) (booking.Payment, bool, error) {
	if s.confirmErr != nil {
		return booking.Payment{}, false, s.confirmErr
	}
	return booking.Payment{ID: 3, HoldID: in.HoldID, UserID: in.UserID, AmountPaise: 25000,
		ChargeKey: "pay-3", Status: booking.PaymentPending}, true, nil
}

func (s *stubRepository) CompletePayment(ctx context.Context, paymentID int64, chargeID string) (booking.CompleteResult, error) {
	b := booking.Booking{ID: 1, ShowID: 1, SeatID: 7, UserID: "alice", HoldID: 12, CreatedAt: time.Now()}
	return booking.CompleteResult{Kind: booking.Booked, Booking: b}, nil
}

func (s *stubRepository) MarkPaymentDeclined(ctx context.Context, paymentID int64) error { return nil }

func (s *stubRepository) MarkPaymentAbandoned(ctx context.Context, paymentID int64) error { return nil }

func (s *stubRepository) RecordReconcileFailure(ctx context.Context, paymentID int64, failure string) error {
	return nil
}

func (s *stubRepository) MarkRefunded(ctx context.Context, paymentID, outboxID int64) (booking.Payment, error) {
	return booking.Payment{ID: paymentID, Status: booking.PaymentRefunded}, nil
}

func (s *stubRepository) RecordOutboxFailure(ctx context.Context, outboxID int64, failure string) error {
	return nil
}

func (s *stubRepository) ReleaseHold(ctx context.Context, userID string, holdID int64) (booking.Hold, int64, error) {
	if s.releaseErr != nil {
		return booking.Hold{}, 0, s.releaseErr
	}
	return booking.Hold{ID: holdID, ShowID: 1, SeatID: 7, UserID: userID, Status: booking.HoldReleased}, 1, nil
}

func (s *stubRepository) MarkOutboxDone(ctx context.Context, outboxID int64) error { return nil }

func (s *stubRepository) HasLiveHold(ctx context.Context, showID, seatID int64, owner string) (bool, error) {
	return false, nil
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

// stubGateway is a PaymentGateway: chargeErr simulates no answer, decline a
// declined card.
type stubGateway struct {
	chargeErr error
	decline   bool
	voided    bool
}

func (g stubGateway) Charge(ctx context.Context, req booking.ChargeRequest) (booking.ChargeResult, error) {
	if g.chargeErr != nil {
		return booking.ChargeResult{}, g.chargeErr
	}
	if g.voided {
		return booking.ChargeResult{Status: booking.ChargeVoided}, nil
	}
	if g.decline {
		return booking.ChargeResult{ChargeID: "ch_1", Status: booking.ChargeDeclined}, nil
	}
	return booking.ChargeResult{ChargeID: "ch_1", Status: booking.ChargeSucceeded}, nil
}

func (g stubGateway) Lookup(ctx context.Context, key string) (booking.ChargeResult, error) {
	return booking.ChargeResult{}, booking.ErrChargeNotFound
}

func (g stubGateway) Void(ctx context.Context, key string) (booking.ChargeResult, error) {
	return booking.ChargeResult{Status: booking.ChargeVoided}, nil
}

func (g stubGateway) Refund(ctx context.Context, req booking.RefundRequest) (booking.RefundResult, error) {
	return booking.RefundResult{RefundID: "re_1"}, nil
}

// fakeIdem is an in-memory IdempotencyStore with the same rules as
// internal/idempotency (minus leases): new key -> proceed, same key and
// request after completion -> replay, different request -> ErrKeyReused,
// still running -> ErrInProgress, released -> runs again.
type fakeIdem struct {
	mu      sync.Mutex
	entries map[string]*fakeIdemEntry
}

type fakeIdemEntry struct {
	hash      string
	token     string
	completed bool
	resp      idempotency.Response
}

func newFakeIdem() *fakeIdem { return &fakeIdem{entries: map[string]*fakeIdemEntry{}} }

func (f *fakeIdem) Begin(ctx context.Context, userID, key, hash string) (idempotency.Claim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[userID+"/"+key]
	switch {
	case !ok:
		f.entries[userID+"/"+key] = &fakeIdemEntry{hash: hash, token: "t1"}
		return idempotency.Claim{Kind: idempotency.Proceed, Token: "t1"}, nil
	case e.hash != hash:
		return idempotency.Claim{}, idempotency.ErrKeyReused
	case e.completed:
		return idempotency.Claim{Kind: idempotency.Replay, Stored: e.resp}, nil
	case e.token != "":
		return idempotency.Claim{}, idempotency.ErrInProgress
	default: // released: take it again
		e.token = "t2"
		return idempotency.Claim{Kind: idempotency.Proceed, Token: "t2"}, nil
	}
}

func (f *fakeIdem) Complete(ctx context.Context, userID, key, token string, resp idempotency.Response) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.entries[userID+"/"+key]
	if e == nil || e.token != token {
		return false, nil
	}
	e.completed, e.resp, e.token = true, resp, ""
	return true, nil
}

func (f *fakeIdem) Release(ctx context.Context, userID, key, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.entries[userID+"/"+key]; e != nil && e.token == token {
		e.token = ""
	}
	return nil
}

// stubPinger simulates a dependency for /readyz.
type stubPinger struct{ err error }

func (p stubPinger) Ping(ctx context.Context) error { return p.err }

// newTestRouter builds the full router with the stubs. Logs are discarded.
func newTestRouter(repo *stubRepository, locker stubLocker) http.Handler {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc := booking.NewService(repo, locker, stubGateway{}, 300*time.Second)
	return NewRouter(svc, newFakeIdem(), stubPinger{}, stubPinger{}, logger)
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

// sendConfirm sends POST /v1/bookings with an Idempotency-Key.
func sendConfirm(t *testing.T, router http.Handler, userID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return sendWithKey(t, router, userID, "test-key-1", body)
}

// sendWithKey sends POST /v1/bookings with the given Idempotency-Key ("" = none).
func sendWithKey(t *testing.T, router http.Handler, userID, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("X-User-ID", userID)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
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
	rec := sendConfirm(t, newTestRouter(&stubRepository{}, stubLocker{}), "alice", `{"hold_id": 12}`)

	require.Equal(t, http.StatusCreated, rec.Code)
	var resp BookingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, int64(12), resp.HoldID)
	assert.Equal(t, "alice", resp.UserID)
}

func TestConfirmBooking_201EvenWhenRedisIsDown(t *testing.T) {
	redisDown := stubLocker{acquireErr: booking.ErrLockUnavailable}

	rec := sendConfirm(t, newTestRouter(&stubRepository{}, redisDown), "alice", `{"hold_id": 12}`)

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
			rec := sendConfirm(t, newTestRouter(tc.repo, stubLocker{}), tc.userID, tc.body)

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
	svc := booking.NewService(&stubRepository{}, stubLocker{}, stubGateway{}, time.Minute)
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
			router := NewRouter(svc, newFakeIdem(), tc.postgres, tc.redis, logger)

			rec := send(t, router, http.MethodGet, "/readyz", "", "")

			assert.Equal(t, tc.wantStatus, rec.Code)
			var resp ReadinessResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, tc.wantPG, resp.Postgres)
			assert.Equal(t, tc.wantRD, resp.Redis)
		})
	}
}

// ---------- payment errors (Phase 3) ----------

func TestConfirmBooking_PaymentErrors(t *testing.T) {
	tests := []struct {
		name       string
		gateway    stubGateway
		wantStatus int
		wantCode   string
	}{
		{"declined", stubGateway{decline: true}, http.StatusPaymentRequired, "PAYMENT_DECLINED"},
		{"no answer", stubGateway{chargeErr: fmt.Errorf("charge: %w", booking.ErrGatewayNoAnswer)}, http.StatusGatewayTimeout, "PAYMENT_PENDING"},
		{"voided key", stubGateway{voided: true}, http.StatusGatewayTimeout, "PAYMENT_NOT_COMPLETED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
			svc := booking.NewService(&stubRepository{}, stubLocker{}, tc.gateway, time.Minute)
			router := NewRouter(svc, newFakeIdem(), stubPinger{}, stubPinger{}, logger)

			rec := sendConfirm(t, router, "alice", `{"hold_id": 12}`)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, tc.wantCode, decodeErrorCode(t, rec))
		})
	}
}

func TestCancelHold_409PaymentInProgress(t *testing.T) {
	rec := send(t, newTestRouter(&stubRepository{releaseErr: booking.ErrPaymentInProgress}, stubLocker{}),
		http.MethodDelete, "/v1/holds/12", "alice", "")

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "PAYMENT_IN_PROGRESS", decodeErrorCode(t, rec))
}

// ---------- Idempotency-Key (Phase 3) ----------

func TestConfirm_400MissingOrBadIdempotencyKey(t *testing.T) {
	router := newTestRouter(&stubRepository{}, stubLocker{})

	rec := sendWithKey(t, router, "alice", "", `{"hold_id": 12}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "MISSING_IDEMPOTENCY_KEY", decodeErrorCode(t, rec))

	rec = sendWithKey(t, router, "alice", strings.Repeat("k", 65), `{"hold_id": 12}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "MISSING_IDEMPOTENCY_KEY", decodeErrorCode(t, rec))
}

func TestConfirm_SameKeyIsReplayedExactly(t *testing.T) {
	router := newTestRouter(&stubRepository{}, stubLocker{})

	first := sendWithKey(t, router, "alice", "k1", `{"hold_id": 12}`)
	second := sendWithKey(t, router, "alice", "k1", `{ "hold_id" : 12 }`) // same request, other spacing

	require.Equal(t, http.StatusCreated, first.Code)
	assert.Empty(t, first.Header().Get("Idempotent-Replayed"))
	assert.Equal(t, http.StatusCreated, second.Code)
	assert.Equal(t, "true", second.Header().Get("Idempotent-Replayed"))
	assert.Equal(t, first.Body.String(), second.Body.String(), "the replay is byte-for-byte identical")
}

func TestConfirm_422KeyReusedWithDifferentHold(t *testing.T) {
	router := newTestRouter(&stubRepository{}, stubLocker{})
	sendWithKey(t, router, "alice", "k1", `{"hold_id": 12}`)

	rec := sendWithKey(t, router, "alice", "k1", `{"hold_id": 13}`)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "IDEMPOTENCY_KEY_REUSED", decodeErrorCode(t, rec))
}

func TestConfirm_409RequestInProgress(t *testing.T) {
	idem := newFakeIdem()
	_, err := idem.Begin(context.Background(), "alice", "k1",
		idempotency.RequestHash(http.MethodPost, "/v1/bookings", "hold_id=12"))
	require.NoError(t, err) // another request holds the key right now
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	router := NewRouter(booking.NewService(&stubRepository{}, stubLocker{}, stubGateway{}, time.Minute), idem, stubPinger{}, stubPinger{}, logger)

	rec := sendWithKey(t, router, "alice", "k1", `{"hold_id": 12}`)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "REQUEST_IN_PROGRESS", decodeErrorCode(t, rec))
}

// A 504 (payment outcome unknown) is NOT stored: the retry with the same key
// runs again and can reach a final answer.
func TestConfirm_504IsNotStored_RetryRunsAgain(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	idem := newFakeIdem()
	noAnswer := stubGateway{chargeErr: fmt.Errorf("charge: %w", booking.ErrGatewayNoAnswer)}
	router := NewRouter(booking.NewService(&stubRepository{}, stubLocker{}, noAnswer, time.Minute), idem, stubPinger{}, stubPinger{}, logger)

	rec := sendWithKey(t, router, "alice", "k1", `{"hold_id": 12}`)
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))

	// The gateway answers now; the same key runs again (not a replay of 504).
	router = NewRouter(booking.NewService(&stubRepository{}, stubLocker{}, stubGateway{}, time.Minute), idem, stubPinger{}, stubPinger{}, logger)
	rec = sendWithKey(t, router, "alice", "k1", `{"hold_id": 12}`)
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Empty(t, rec.Header().Get("Idempotent-Replayed"))
}

func TestConfirm_410IncludesRefundState(t *testing.T) {
	rec := sendConfirm(t, newTestRouter(&stubRepository{confirmErr: &booking.HoldExpiredError{PaymentStatus: booking.PaymentRefunded}}, stubLocker{}),
		"alice", `{"hold_id": 12}`)

	assert.Equal(t, http.StatusGone, rec.Code)
	assert.Contains(t, rec.Body.String(), `"payment":"REFUNDED"`)
}

// ---------- HTTP metrics (Phase 5) ----------

func newMetricsRouter(t *testing.T) (http.Handler, *metrics.Metrics) {
	t.Helper()
	m := metrics.NewAPI()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc := booking.NewService(&stubRepository{}, stubLocker{}, stubGateway{}, time.Minute)
	return NewRouter(svc, newFakeIdem(), stubPinger{}, stubPinger{}, logger, WithMetrics(m)), m
}

// The route label is the chi PATTERN, never the raw path (cardinality).
func TestMetrics_RouteLabelIsThePattern(t *testing.T) {
	router, m := newMetricsRouter(t)

	send(t, router, http.MethodDelete, "/v1/holds/123", "alice", "")
	send(t, router, http.MethodDelete, "/v1/holds/456", "alice", "")
	send(t, router, http.MethodGet, "/no/such/path/789", "", "")
	send(t, router, "FOOBAR", "/v1/shows", "", "")

	assert.Equal(t, 2.0, testutil.ToFloat64(m.HTTPRequests.WithLabelValues("/v1/holds/{holdID}", "DELETE", "200")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.HTTPRequests.WithLabelValues("unmatched", "GET", "404")))
	families, err := m.Registry.Gather()
	require.NoError(t, err)
	methods := map[string]bool{}
	for _, f := range families {
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				assert.NotContains(t, l.GetValue(), "123", "raw ids must never become label values")
				assert.NotContains(t, l.GetValue(), "789")
				if l.GetName() == "method" {
					methods[l.GetValue()] = true
				}
			}
		}
	}
	assert.True(t, methods["OTHER"], "unknown methods are folded into OTHER")
	assert.False(t, methods["FOOBAR"], "a client-chosen method never becomes a label value")
}

// The public router does not serve /metrics (it lives on the internal port).
func TestMetrics_NotOnThePublicRouter(t *testing.T) {
	router, _ := newMetricsRouter(t)

	rec := send(t, router, http.MethodGet, "/metrics", "", "")

	assert.Equal(t, http.StatusNotFound, rec.Code)
}
