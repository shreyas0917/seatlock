// service_test.go unit-tests the business rules in service.go.
//
// It uses fakeRepository, an in-memory stand-in for Postgres, so these tests
// need no database and run in milliseconds (`make test`). The real SQL is
// tested separately in repository_postgres_test.go (`make test-integration`).
package booking

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRepository implements the Repository interface with plain Go maps.
// A mutex protects the maps because a Service may be called from many
// goroutines at once, just like the real one.
type fakeRepository struct {
	mu       sync.Mutex
	shows    map[int64]Show
	seats    map[int64]Seat
	bookings map[int64]Booking
	nextID   int64
}

// newFakeRepository returns a fake with show 1 (seats 1 and 2) and show 2 (seat 3).
func newFakeRepository() *fakeRepository {
	return &fakeRepository{
		shows: map[int64]Show{
			1: {ID: 1, Title: "Coldplay Live"},
			2: {ID: 2, Title: "Another Show"},
		},
		seats: map[int64]Seat{
			1: {ID: 1, ShowID: 1, Label: "A1", PricePaise: 25000},
			2: {ID: 2, ShowID: 1, Label: "A2", PricePaise: 25000},
			3: {ID: 3, ShowID: 2, Label: "A1", PricePaise: 25000},
		},
		bookings: map[int64]Booking{},
	}
}

func (f *fakeRepository) ListShows(ctx context.Context) ([]Show, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var shows []Show
	for _, s := range f.shows {
		shows = append(shows, s)
	}
	return shows, nil
}

func (f *fakeRepository) GetShow(ctx context.Context, showID int64) (Show, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	show, ok := f.shows[showID]
	if !ok {
		return Show{}, ErrShowNotFound
	}
	return show, nil
}

func (f *fakeRepository) GetSeat(ctx context.Context, seatID int64) (Seat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seat, ok := f.seats[seatID]
	if !ok {
		return Seat{}, ErrSeatNotFound
	}
	return seat, nil
}

func (f *fakeRepository) ListSeatsWithStatus(ctx context.Context, showID int64) ([]SeatWithStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []SeatWithStatus
	for _, seat := range f.seats {
		if seat.ShowID != showID {
			continue
		}
		status := SeatAvailable
		for _, b := range f.bookings {
			if b.SeatID == seat.ID {
				status = SeatBooked
			}
		}
		result = append(result, SeatWithStatus{Seat: seat, Status: status})
	}
	return result, nil
}

// CreateBooking imitates the UNIQUE(show_id, seat_id) constraint: the check
// and the insert happen while holding the lock, so they are atomic, just like
// inside Postgres.
func (f *fakeRepository) CreateBooking(ctx context.Context, in BookSeatInput) (Booking, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.bookings {
		if b.ShowID == in.ShowID && b.SeatID == in.SeatID {
			return Booking{}, ErrSeatAlreadyBooked
		}
	}
	f.nextID++
	b := Booking{ID: f.nextID, ShowID: in.ShowID, SeatID: in.SeatID, UserID: in.UserID, CreatedAt: time.Now()}
	f.bookings[b.ID] = b
	return b, nil
}

func (f *fakeRepository) GetBooking(ctx context.Context, bookingID int64) (Booking, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.bookings[bookingID]
	if !ok {
		return Booking{}, ErrBookingNotFound
	}
	return b, nil
}

func TestBookSeat_Success(t *testing.T) {
	svc := NewService(newFakeRepository())

	b, err := svc.BookSeat(context.Background(), BookSeatInput{UserID: "alice", ShowID: 1, SeatID: 1})

	require.NoError(t, err)
	assert.Equal(t, int64(1), b.ShowID)
	assert.Equal(t, int64(1), b.SeatID)
	assert.Equal(t, "alice", b.UserID)
}

// TestBookSeat_ValidationErrors is a "table-driven test": a list of cases run
// by one loop. Each case runs as a named subtest via t.Run, so a failure says
// exactly which case broke.
func TestBookSeat_ValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		input   BookSeatInput
		wantErr error
	}{
		{"missing user id", BookSeatInput{UserID: "", ShowID: 1, SeatID: 1}, ErrMissingUserID},
		{"blank user id", BookSeatInput{UserID: "   ", ShowID: 1, SeatID: 1}, ErrMissingUserID},
		{"zero show id", BookSeatInput{UserID: "alice", ShowID: 0, SeatID: 1}, ErrInvalidInput},
		{"negative seat id", BookSeatInput{UserID: "alice", ShowID: 1, SeatID: -5}, ErrInvalidInput},
		{"unknown show", BookSeatInput{UserID: "alice", ShowID: 99, SeatID: 1}, ErrShowNotFound},
		{"unknown seat", BookSeatInput{UserID: "alice", ShowID: 1, SeatID: 99}, ErrSeatNotFound},
		{"seat from another show", BookSeatInput{UserID: "alice", ShowID: 1, SeatID: 3}, ErrSeatNotInShow},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewService(newFakeRepository())

			_, err := svc.BookSeat(context.Background(), tc.input)

			// ErrorIs uses errors.Is, so it also matches wrapped errors
			// like "invalid input: show_id must be a positive number".
			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestBookSeat_SameSeatTwice(t *testing.T) {
	svc := NewService(newFakeRepository())
	ctx := context.Background()

	_, err := svc.BookSeat(ctx, BookSeatInput{UserID: "alice", ShowID: 1, SeatID: 1})
	require.NoError(t, err)

	_, err = svc.BookSeat(ctx, BookSeatInput{UserID: "bob", ShowID: 1, SeatID: 1})
	assert.ErrorIs(t, err, ErrSeatAlreadyBooked)
}

func TestListSeats_ShowsBookedStatus(t *testing.T) {
	svc := NewService(newFakeRepository())
	ctx := context.Background()
	_, err := svc.BookSeat(ctx, BookSeatInput{UserID: "alice", ShowID: 1, SeatID: 2})
	require.NoError(t, err)

	seats, err := svc.ListSeats(ctx, 1)

	require.NoError(t, err)
	require.Len(t, seats, 2)
	for _, s := range seats {
		if s.ID == 2 {
			assert.Equal(t, SeatBooked, s.Status)
		} else {
			assert.Equal(t, SeatAvailable, s.Status)
		}
	}
}

func TestListSeats_UnknownShow(t *testing.T) {
	svc := NewService(newFakeRepository())

	_, err := svc.ListSeats(context.Background(), 42)

	assert.ErrorIs(t, err, ErrShowNotFound)
}

func TestGetBooking(t *testing.T) {
	svc := NewService(newFakeRepository())
	ctx := context.Background()
	created, err := svc.BookSeat(ctx, BookSeatInput{UserID: "alice", ShowID: 1, SeatID: 1})
	require.NoError(t, err)

	got, err := svc.GetBooking(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created, got)

	_, err = svc.GetBooking(ctx, 999)
	assert.ErrorIs(t, err, ErrBookingNotFound)

	_, err = svc.GetBooking(ctx, 0)
	assert.ErrorIs(t, err, ErrInvalidInput)
}
