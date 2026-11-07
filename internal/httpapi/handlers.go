// handlers.go contains one function per HTTP endpoint.
//
// Where it fits: router.go sends each request to a handler here. A handler
// ONLY does HTTP work: read the path/headers/body, call the booking service,
// and write JSON. Business rules live in the service; status codes for errors
// come from mapError in errors.go.
//
// The comment lines starting with "@" are swag annotations. `make swagger`
// reads them and generates the OpenAPI docs served at /swagger/index.html.
package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/shreyas0917/seatlock/internal/booking"
	"github.com/shreyas0917/seatlock/internal/metrics"
)

// BookingService is what the handlers need from the booking layer.
// *booking.Service satisfies it.
type BookingService interface {
	ListShows(ctx context.Context) ([]booking.Show, error)
	ListSeats(ctx context.Context, showID int64) ([]booking.SeatWithStatus, error)
	HoldSeat(ctx context.Context, in booking.HoldSeatInput) (booking.Hold, bool, error)
	CancelHold(ctx context.Context, userID string, holdID int64) (booking.Hold, error)
	ConfirmBooking(ctx context.Context, in booking.ConfirmInput) (booking.Booking, error)
	GetBooking(ctx context.Context, bookingID int64) (booking.Booking, error)
}

// Pinger is anything that can check a dependency is reachable.
// *pgxpool.Pool and *lock.RedisLocker both satisfy it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// userIDHeader identifies the caller. There is no authentication in SeatLock;
// the client simply says who it is.
const userIDHeader = "X-User-ID"

// readinessTimeout is how long /readyz waits for each dependency.
const readinessTimeout = 1 * time.Second

// handlers holds the dependencies shared by all endpoints.
type handlers struct {
	svc      BookingService
	idem     IdempotencyStore
	postgres Pinger
	redis    Pinger
	logger   *slog.Logger
	metrics  *metrics.Metrics // nil = metrics disabled
}

// healthz godoc
//
//	@Summary		Liveness check
//	@Description	Returns 200 while the process is running. Does not check dependencies.
//	@Tags			health
//	@Produce		json
//	@Success		200	{object}	StatusResponse
//	@Router			/healthz [get]
func (h *handlers) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, StatusResponse{Status: "ok"})
}

// readyz godoc
//
//	@Summary		Readiness check
//	@Description	Reports Postgres and Redis. Returns 200 whenever Postgres answers within 1s,
//	@Description	even if Redis is down; returns 503 only when Postgres is down.
//	@Tags			health
//	@Produce		json
//	@Success		200	{object}	ReadinessResponse
//	@Failure		503	{object}	ReadinessResponse
//	@Router			/readyz [get]
//
// Why a Redis outage must NOT make readyz fail: a load balancer removes every
// instance whose readiness check fails. Redis is shared, so if it goes down,
// EVERY instance would fail at the same moment and the whole API would
// vanish, including confirms, seat maps and booking lookups, which still
// work without Redis (only NEW holds are refused, with 503 LOCK_UNAVAILABLE).
// So readyz reports Redis for humans and dashboards, but only Postgres, the
// thing no request can do without, decides 200 vs 503.
func (h *handlers) readyz(w http.ResponseWriter, r *http.Request) {
	resp := ReadinessResponse{
		Postgres: h.pingStatus(r.Context(), "postgres", h.postgres),
		Redis:    h.pingStatus(r.Context(), "redis", h.redis),
	}

	status := http.StatusOK
	if resp.Postgres != "up" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

// pingStatus pings one dependency and returns "up" or "down".
func (h *handlers) pingStatus(ctx context.Context, name string, p Pinger) string {
	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	if err := p.Ping(ctx); err != nil {
		h.logger.Warn("readiness: dependency down", "dependency", name, "error", err.Error())
		return "down"
	}
	return "up"
}

// listShows godoc
//
//	@Summary	List shows
//	@Tags		shows
//	@Produce	json
//	@Success	200	{object}	ShowListResponse
//	@Failure	500	{object}	ErrorResponse
//	@Failure	503	{object}	ErrorResponse	"Database busy"
//	@Router		/v1/shows [get]
func (h *handlers) listShows(w http.ResponseWriter, r *http.Request) {
	shows, err := h.svc.ListShows(r.Context())
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	resp := ShowListResponse{Shows: make([]ShowResponse, 0, len(shows))}
	for _, s := range shows {
		resp.Shows = append(resp.Shows, toShowResponse(s))
	}
	writeJSON(w, http.StatusOK, resp)
}

// listSeats godoc
//
//	@Summary		Seat map of a show
//	@Description	Every seat of the show with its price and status (AVAILABLE, HELD or BOOKED),
//	@Description	computed from PostgreSQL only.
//	@Tags			shows
//	@Produce		json
//	@Param			showID	path		int	true	"Show ID"
//	@Success		200		{object}	SeatMapResponse
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Failure		503		{object}	ErrorResponse	"Database busy"
//	@Router			/v1/shows/{showID}/seats [get]
func (h *handlers) listSeats(w http.ResponseWriter, r *http.Request) {
	showID, err := pathID(r, "showID")
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	seats, err := h.svc.ListSeats(r.Context(), showID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	resp := SeatMapResponse{ShowID: showID, Seats: make([]SeatResponse, 0, len(seats))}
	for _, s := range seats {
		resp.Seats = append(resp.Seats, toSeatResponse(s))
	}
	writeJSON(w, http.StatusOK, resp)
}

// createHold godoc
//
//	@Summary		Hold a seat
//	@Description	Reserves a seat for the user for 5 minutes. Returns 201 for a new hold, or 200
//	@Description	with the same hold if this user already holds the seat (safe to retry).
//	@Description	If the seat lock (Redis) is unavailable, no hold is created: 503 LOCK_UNAVAILABLE.
//	@Tags			holds
//	@Accept			json
//	@Produce		json
//	@Param			X-User-ID	header		string				true	"Who is holding"
//	@Param			request		body		CreateHoldRequest	true	"Seat to hold"
//	@Success		201			{object}	HoldResponse		"New hold"
//	@Success		200			{object}	HoldResponse		"Existing hold of this user"
//	@Failure		400			{object}	ErrorResponse		"Bad input or missing X-User-ID"
//	@Failure		404			{object}	ErrorResponse		"Show or seat not found"
//	@Failure		409			{object}	ErrorResponse		"SEAT_HELD or SEAT_ALREADY_BOOKED"
//	@Failure		503			{object}	ErrorResponse		"LOCK_UNAVAILABLE or SERVICE_BUSY"
//	@Failure		500			{object}	ErrorResponse
//	@Router			/v1/holds [post]
func (h *handlers) createHold(w http.ResponseWriter, r *http.Request) {
	var req CreateHoldRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}

	hold, created, err := h.svc.HoldSeat(r.Context(), booking.HoldSeatInput{
		UserID: r.Header.Get(userIDHeader),
		ShowID: req.ShowID,
		SeatID: req.SeatID,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	status := http.StatusOK // the user's existing hold, returned again
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, toHoldResponse(hold))
}

// cancelHold godoc
//
//	@Summary		Cancel a hold
//	@Description	Releases the user's active hold so others can take the seat. Works even when
//	@Description	Redis is down (PostgreSQL is updated; the Redis lock then expires by TTL).
//	@Tags			holds
//	@Produce		json
//	@Param			X-User-ID	header		string	true	"Owner of the hold"
//	@Param			holdID		path		int		true	"Hold ID"
//	@Success		200			{object}	HoldResponse
//	@Failure		400			{object}	ErrorResponse
//	@Failure		404			{object}	ErrorResponse	"HOLD_NOT_FOUND (also for another user's hold)"
//	@Failure		409			{object}	ErrorResponse	"HOLD_ALREADY_USED"
//	@Failure		410			{object}	ErrorResponse	"HOLD_EXPIRED"
//	@Failure		503			{object}	ErrorResponse	"SERVICE_BUSY"
//	@Failure		500			{object}	ErrorResponse
//	@Router			/v1/holds/{holdID} [delete]
func (h *handlers) cancelHold(w http.ResponseWriter, r *http.Request) {
	holdID, err := pathID(r, "holdID")
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	hold, err := h.svc.CancelHold(r.Context(), r.Header.Get(userIDHeader), holdID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toHoldResponse(hold))
}

// getBooking godoc
//
//	@Summary	Get a booking
//	@Tags		bookings
//	@Produce	json
//	@Param		bookingID	path		int	true	"Booking ID"
//	@Success	200			{object}	BookingResponse
//	@Failure	400			{object}	ErrorResponse
//	@Failure	404			{object}	ErrorResponse
//	@Failure	500			{object}	ErrorResponse
//	@Failure	503			{object}	ErrorResponse	"Database busy"
//	@Router		/v1/bookings/{bookingID} [get]
func (h *handlers) getBooking(w http.ResponseWriter, r *http.Request) {
	bookingID, err := pathID(r, "bookingID")
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	b, err := h.svc.GetBooking(r.Context(), bookingID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBookingResponse(b))
}

// pathID reads a numeric URL parameter such as {showID}.
// Non-numbers become booking.ErrInvalidInput (400).
func pathID(r *http.Request, name string) (int64, error) {
	raw := chi.URLParam(r, name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a number, got %q", booking.ErrInvalidInput, name, raw)
	}
	return id, nil
}
