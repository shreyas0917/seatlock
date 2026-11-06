// Package booking is the heart of SeatLock: the domain models, the business
// rules (service.go), the SQL (repository_postgres.go) and the domain errors
// (errors.go).
//
// Request flow:
//
//	HTTP request -> httpapi handler -> booking.Service -> booking.Repository -> PostgreSQL
//
// This file holds the plain data types that move through that flow. They have
// no JSON tags on purpose: how data looks on the wire is the HTTP layer's job
// (internal/httpapi/responses.go), not the domain's.
package booking

import "time"

// Show is one performance at one venue at one time.
type Show struct {
	ID        int64
	Title     string
	Venue     string
	StartsAt  time.Time
	CreatedAt time.Time
}

// Seat is a single seat for a show. Prices are in paise (1 rupee = 100 paise)
// and stored as integers, because floating point cannot represent money exactly.
type Seat struct {
	ID         int64
	ShowID     int64
	Label      string // e.g. "A7"
	PricePaise int64
}

// SeatStatus says whether a seat can still be booked.
//
// Declaring a named type (instead of using a plain string everywhere) lets the
// compiler and the reader know which strings are valid statuses.
type SeatStatus string

const (
	SeatAvailable SeatStatus = "AVAILABLE"
	SeatBooked    SeatStatus = "BOOKED"
)

// SeatWithStatus is one entry in a show's seat map.
type SeatWithStatus struct {
	Seat
	Status SeatStatus
}

// Booking records that a user owns a seat for a show.
type Booking struct {
	ID        int64
	ShowID    int64
	SeatID    int64
	UserID    string
	CreatedAt time.Time
}

// BookSeatInput is what a client sends to book a seat.
type BookSeatInput struct {
	UserID string
	ShowID int64
	SeatID int64
}
