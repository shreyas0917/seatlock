// handlers_confirm.go is POST /v1/bookings: confirm a hold (pay, then book),
// made safe to repeat with the Idempotency-Key header.
//
// Where it fits:
//
//	request -> confirmBooking -> idempotency.Store.Begin
//	                               |- Replay     -> stored response, byte for byte
//	                               |- 422 / 409  -> key reused / still in progress
//	                               '- Proceed    -> booking.Service.ConfirmBooking
//	                                                -> Complete (final answer) or Release (unknown)
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/shreyas0917/seatlock/internal/booking"
	"github.com/shreyas0917/seatlock/internal/idempotency"
)

// IdempotencyStore is what the confirm handler needs from internal/idempotency.
// *idempotency.Store satisfies it; handler tests use a small fake.
type IdempotencyStore interface {
	Begin(ctx context.Context, userID, key, requestHash string) (idempotency.Claim, error)
	Complete(ctx context.Context, userID, key, token string, resp idempotency.Response) (bool, error)
	Release(ctx context.Context, userID, key, token string) error
}

// idempotencyKeyHeader carries the client's key for one logical request.
const idempotencyKeyHeader = "Idempotency-Key"

// confirmBooking godoc
//
//	@Summary		Confirm a hold: pay, then book
//	@Description	Charges the seat price for the user's active hold and turns it into a booking.
//	@Description	Requires an Idempotency-Key: sending the same request again with the same key
//	@Description	returns the stored response (header Idempotent-Replayed: true) and never charges twice.
//	@Description	Uses PostgreSQL only, so it works while Redis is down.
//	@Tags			bookings
//	@Accept			json
//	@Produce		json
//	@Param			X-User-ID		header		string					true	"Owner of the hold"
//	@Param			Idempotency-Key	header		string					true	"Unique key for this confirm (1-64 characters)"
//	@Param			request			body		ConfirmBookingRequest	true	"Hold to confirm"
//	@Success		201				{object}	BookingResponse
//	@Failure		400				{object}	ErrorResponse	"Bad input, missing X-User-ID or Idempotency-Key"
//	@Failure		402				{object}	ErrorResponse	"PAYMENT_DECLINED (hold kept; retry with a new key)"
//	@Failure		404				{object}	ErrorResponse	"HOLD_NOT_FOUND (also for another user's hold)"
//	@Failure		409				{object}	ErrorResponse	"HOLD_ALREADY_USED, SEAT_ALREADY_BOOKED or REQUEST_IN_PROGRESS"
//	@Failure		410				{object}	ErrorResponse	"HOLD_EXPIRED (error.payment = REFUNDED / REFUND_PENDING if money was taken)"
//	@Failure		422				{object}	ErrorResponse	"IDEMPOTENCY_KEY_REUSED"
//	@Failure		503				{object}	ErrorResponse	"SERVICE_BUSY"
//	@Failure		504				{object}	ErrorResponse	"PAYMENT_PENDING (outcome unknown; retry with the SAME key)"
//	@Failure		500				{object}	ErrorResponse
//	@Router			/v1/bookings [post]
func (h *handlers) confirmBooking(w http.ResponseWriter, r *http.Request) {
	// 1. Validate everything that does not need the database, BEFORE the key
	//    is stored: a malformed request never occupies a key.
	userID := strings.TrimSpace(r.Header.Get(userIDHeader))
	if userID == "" {
		h.writeError(w, r, booking.ErrMissingUserID)
		return
	}
	key := r.Header.Get(idempotencyKeyHeader)
	if key == "" || len(key) > idempotency.MaxKeyLength {
		h.writeError(w, r, idempotency.ErrMissingKey)
		return
	}
	var req ConfirmBookingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	if req.HoldID <= 0 {
		h.writeError(w, r, fmt.Errorf("%w: hold_id must be a positive number", booking.ErrInvalidInput))
		return
	}

	// 2. Claim the key. The hash covers the decoded body, so the same key
	//    with another hold_id is detected (422).
	hash := idempotency.RequestHash(r.Method, r.URL.Path, fmt.Sprintf("hold_id=%d", req.HoldID))
	claim, err := h.idem.Begin(r.Context(), userID, key, hash)
	if err != nil {
		h.writeError(w, r, err) // 422 key reused, 409 in progress, 503 busy
		return
	}
	if claim.Kind == idempotency.Replay {
		w.Header().Set("Idempotent-Replayed", "true")
		writeRawJSON(w, claim.Stored.Status, claim.Stored.Body)
		return
	}

	// 3. We own the key: run the confirm and turn the outcome into a response.
	status, body := h.runConfirm(r, booking.ConfirmInput{UserID: userID, HoldID: req.HoldID})

	// 4. Final answers are stored for replay. Unknown outcomes are not: the
	//    key is given back, so a retry with it runs again and reaches a
	//    final answer (for a 504, by asking the gateway with the same charge_key).
	if isFinal(status) {
		stored, err := h.idem.Complete(r.Context(), userID, key, claim.Token, idempotency.Response{Status: status, Body: body})
		switch {
		case err != nil:
			h.logger.Warn("could not store idempotent response",
				"request_id", middleware.GetReqID(r.Context()), "error", err.Error())
		case !stored:
			// Our lease expired and another request took the key over.
			// We do not overwrite its answer; we just return ours.
			h.logger.Warn("idempotency lease was taken over; response not stored",
				"request_id", middleware.GetReqID(r.Context()))
		}
	} else if err := h.idem.Release(r.Context(), userID, key, claim.Token); err != nil {
		h.logger.Warn("could not release idempotency key; it frees itself when the lease ends",
			"request_id", middleware.GetReqID(r.Context()), "error", err.Error())
	}

	setRetryAfter(w, status)
	writeRawJSON(w, status, body)
}

// runConfirm calls the service and returns the HTTP status and JSON body.
func (h *handlers) runConfirm(r *http.Request, in booking.ConfirmInput) (int, []byte) {
	b, err := h.svc.ConfirmBooking(r.Context(), in)

	var status int
	var payload any
	if err != nil {
		status, payload = h.errorResponse(r, err)
	} else {
		status, payload = http.StatusCreated, toBookingResponse(b)
	}
	body, mErr := json.Marshal(payload)
	if mErr != nil {
		// Cannot happen for these plain structs; answer 500 and do not store.
		h.logger.Error("encode response", "error", mErr.Error())
		return http.StatusInternalServerError, []byte(`{"error":{"code":"INTERNAL_ERROR","message":"internal server error"}}`)
	}
	return status, body
}

// isFinal reports whether a confirm response is a FINAL answer, worth
// storing and replaying: 201 booked, 402 declined, 404 no such hold,
// 409 hold used / seat booked, 410 expired. 5xx (busy, payment outcome
// unknown, internal error) are not final: a retry may well succeed.
func isFinal(status int) bool {
	switch status {
	case http.StatusCreated, http.StatusPaymentRequired, http.StatusNotFound, http.StatusConflict, http.StatusGone:
		return true
	default:
		return false
	}
}
