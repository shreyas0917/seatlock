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
)

// BookingService is what the handlers need from the booking layer.
// *booking.Service satisfies it; handler tests use a small stub instead.
type BookingService interface {
	ListShows(ctx context.Context) ([]booking.Show, error)
	ListSeats(ctx context.Context, showID int64) ([]booking.SeatWithStatus, error)
	BookSeat(ctx context.Context, in booking.BookSeatInput) (booking.Booking, error)
	GetBooking(ctx context.Context, bookingID int64) (booking.Booking, error)
}

// Pinger is anything that can check the database is reachable.
// *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// userIDHeader identifies the caller. There is no authentication in SeatLock;
// the client simply says who it is.
const userIDHeader = "X-User-ID"

// readinessTimeout is how long /readyz waits for Postgres before giving up.
const readinessTimeout = 1 * time.Second

// handlers holds the dependencies shared by all endpoints.
type handlers struct {
	svc    BookingService
	db     Pinger
	logger *slog.Logger
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
//	@Description	Returns 200 only if PostgreSQL answers a ping within 1 second, otherwise 503.
//	@Tags			health
//	@Produce		json
//	@Success		200	{object}	StatusResponse
//	@Failure		503	{object}	StatusResponse
//	@Router			/readyz [get]
func (h *handlers) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	if err := h.db.Ping(ctx); err != nil {
		h.logger.Warn("readiness check failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, StatusResponse{Status: "database unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, StatusResponse{Status: "ready"})
}

// listShows godoc
//
//	@Summary	List shows
//	@Tags		shows
//	@Produce	json
//	@Success	200	{object}	ShowListResponse
//	@Failure	500	{object}	ErrorResponse
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
//	@Description	Every seat of the show with its price and status (AVAILABLE or BOOKED).
//	@Tags			shows
//	@Produce		json
//	@Param			showID	path		int	true	"Show ID"
//	@Success		200		{object}	SeatMapResponse
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
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

// createBooking godoc
//
//	@Summary		Book a seat
//	@Description	Books one seat directly (no holds in Phase 1). If several users book the
//	@Description	same seat at once, exactly one gets 201 and the rest get 409.
//	@Tags			bookings
//	@Accept			json
//	@Produce		json
//	@Param			X-User-ID	header		string					true	"Who is booking"
//	@Param			request		body		CreateBookingRequest	true	"Seat to book"
//	@Success		201			{object}	BookingResponse
//	@Failure		400			{object}	ErrorResponse	"Bad input or missing X-User-ID"
//	@Failure		404			{object}	ErrorResponse	"Show or seat not found"
//	@Failure		409			{object}	ErrorResponse	"Seat already booked"
//	@Failure		500			{object}	ErrorResponse
//	@Router			/v1/bookings [post]
func (h *handlers) createBooking(w http.ResponseWriter, r *http.Request) {
	var req CreateBookingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}

	// The handler only passes the header along; deciding that an empty user
	// is invalid is a business rule, so the service checks it.
	b, err := h.svc.BookSeat(r.Context(), booking.BookSeatInput{
		UserID: r.Header.Get(userIDHeader),
		ShowID: req.ShowID,
		SeatID: req.SeatID,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toBookingResponse(b))
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
