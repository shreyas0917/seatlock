// responses.go defines the JSON shapes of requests and responses.
//
// Where it fits: handlers convert domain types (booking.Show, booking.Booking,
// ...) into these structs before writing JSON. Keeping them separate from the
// domain models means we can change the database or the business logic without
// accidentally changing the public API, and swag reads these structs to
// generate the Swagger docs.
//
// About struct tags (first appearance): the text in backquotes after a field,
// like `json:"show_id"`, is a "struct tag". The encoding/json package reads it
// to know the JSON key name. swag reads the `example:"..."` tag for the docs.
package httpapi

import (
	"time"

	"github.com/shreyas0917/seatlock/internal/booking"
)

// StatusResponse is returned by /healthz.
type StatusResponse struct {
	Status string `json:"status" example:"ok"`
}

// ReadinessResponse is returned by /readyz: the state of each dependency.
type ReadinessResponse struct {
	Postgres string `json:"postgres" example:"up" enums:"up,down"`
	Redis    string `json:"redis" example:"up" enums:"up,down"`
}

// ErrorResponse is the single error shape used by every endpoint.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody holds a machine-readable code and a human-readable message.
type ErrorBody struct {
	Code    string `json:"code" example:"SEAT_ALREADY_BOOKED"`
	Message string `json:"message" example:"this seat has already been booked"`
}

// ShowResponse is one show.
type ShowResponse struct {
	ID       int64     `json:"id" example:"1"`
	Title    string    `json:"title" example:"Coldplay Live"`
	Venue    string    `json:"venue" example:"DY Patil Stadium, Navi Mumbai"`
	StartsAt time.Time `json:"starts_at" example:"2027-01-25T13:30:00Z"`
}

// ShowListResponse wraps the list in an object, so fields (like pagination)
// can be added later without breaking clients.
type ShowListResponse struct {
	Shows []ShowResponse `json:"shows"`
}

// SeatResponse is one seat in the seat map.
type SeatResponse struct {
	ID         int64  `json:"id" example:"7"`
	Label      string `json:"label" example:"A7"`
	PricePaise int64  `json:"price_paise" example:"25000"`
	Status     string `json:"status" example:"AVAILABLE" enums:"AVAILABLE,HELD,BOOKED"`
}

// SeatMapResponse is the seat map of one show.
type SeatMapResponse struct {
	ShowID int64          `json:"show_id" example:"1"`
	Seats  []SeatResponse `json:"seats"`
}

// CreateHoldRequest is the body of POST /v1/holds.
type CreateHoldRequest struct {
	ShowID int64 `json:"show_id" example:"1"`
	SeatID int64 `json:"seat_id" example:"7"`
}

// HoldResponse is one hold.
type HoldResponse struct {
	HoldID      int64     `json:"hold_id" example:"12"`
	ShowID      int64     `json:"show_id" example:"1"`
	SeatID      int64     `json:"seat_id" example:"7"`
	UserID      string    `json:"user_id" example:"user-42"`
	Status      string    `json:"status" example:"ACTIVE" enums:"ACTIVE,CONSUMED,RELEASED,EXPIRED"`
	ExpiresAt   time.Time `json:"expires_at" example:"2026-11-07T10:05:00Z"`
	SecondsLeft int64     `json:"seconds_left" example:"300"`
}

// ConfirmBookingRequest is the body of POST /v1/bookings.
type ConfirmBookingRequest struct {
	HoldID int64 `json:"hold_id" example:"12"`
}

// BookingResponse is one booking.
type BookingResponse struct {
	ID        int64     `json:"id" example:"1"`
	ShowID    int64     `json:"show_id" example:"1"`
	SeatID    int64     `json:"seat_id" example:"7"`
	UserID    string    `json:"user_id" example:"user-42"`
	HoldID    int64     `json:"hold_id" example:"12"`
	CreatedAt time.Time `json:"created_at" example:"2026-11-07T10:00:00Z"`
}

func toShowResponse(s booking.Show) ShowResponse {
	return ShowResponse{ID: s.ID, Title: s.Title, Venue: s.Venue, StartsAt: s.StartsAt}
}

func toSeatResponse(s booking.SeatWithStatus) SeatResponse {
	return SeatResponse{ID: s.ID, Label: s.Label, PricePaise: s.PricePaise, Status: string(s.Status)}
}

func toBookingResponse(b booking.Booking) BookingResponse {
	return BookingResponse{ID: b.ID, ShowID: b.ShowID, SeatID: b.SeatID, UserID: b.UserID, HoldID: b.HoldID, CreatedAt: b.CreatedAt}
}

func toHoldResponse(h booking.Hold) HoldResponse {
	return HoldResponse{
		HoldID: h.ID, ShowID: h.ShowID, SeatID: h.SeatID, UserID: h.UserID,
		Status: string(h.Status), ExpiresAt: h.ExpiresAt, SecondsLeft: h.SecondsLeft,
	}
}
