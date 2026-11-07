// json.go has small helpers for reading and writing JSON over HTTP.
//
// Where it fits: every handler in handlers.go uses writeJSON to send a
// response and decodeJSON to read a request body, so all responses look the
// same and body-size limits are applied everywhere.
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/shreyas0917/seatlock/internal/booking"
)

// maxBodyBytes limits request bodies. A booking request is a few bytes; this
// stops a client from making the server read a huge body into memory.
const maxBodyBytes = 1 << 20 // 1 MiB

// writeJSON sends v as JSON with the given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// WriteHeader must be called before writing the body.
	w.WriteHeader(status)
	// If encoding fails here the status is already sent and the client has
	// probably disconnected; there is nothing useful left to do.
	_ = json.NewEncoder(w).Encode(v)
}

// writeRawJSON sends an already-encoded JSON body (a stored idempotent response).
func writeRawJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// decodeJSON reads the request body into dst. Any problem (bad JSON, unknown
// fields, extra data, too large) becomes booking.ErrInvalidInput, so the
// central error mapper answers 400 Bad Request.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	// Reject fields we do not know, so a typo like "seatid" is reported
	// instead of being silently ignored.
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: request body must be valid JSON (%v)", booking.ErrInvalidInput, err)
	}
	// The body must contain exactly one JSON object.
	if dec.More() {
		return fmt.Errorf("%w: request body must contain a single JSON object", booking.ErrInvalidInput)
	}
	return nil
}
