// service_test.go unit-tests the business rules in service.go and
// service_holds.go.
//
// It uses fakeRepository (an in-memory stand-in for Postgres) and fakeLocker
// (an in-memory stand-in for Redis that can also pretend to be DOWN), so
// these tests need no database or Redis and run in milliseconds (`make
// test`). This is the payoff of the Repository and SeatLocker interfaces: the
// service cannot tell a fake from the real thing. The real SQL and Redis are
// tested separately (`make test-integration`).
package booking

import (
	"context"
	"fmt"
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
	holds    map[int64]Hold
	nextID   int64

	// createHoldErr, if set, is returned by CreateHold (simulates Postgres rejecting).
	createHoldErr error
	// createHoldCalls counts CreateHold calls, to prove when it is NOT called.
	createHoldCalls int
	// outboxDone lists the outbox rows marked DONE.
	outboxDone []int64

	payments map[int64]Payment
	// completeErr, if set, is returned by CompletePayment (Tx B fails).
	completeErr error
	// expireDuringPayment makes CompletePayment act as if the hold expired
	// while the gateway was working.
	expireDuringPayment bool
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
		holds:    map[int64]Hold{},
		payments: map[int64]Payment{},
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

func (f *fakeRepository) GetBooking(ctx context.Context, bookingID int64) (Booking, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.bookings[bookingID]
	if !ok {
		return Booking{}, ErrBookingNotFound
	}
	return b, nil
}

func (f *fakeRepository) CreateHold(ctx context.Context, in HoldSeatInput, ttl time.Duration) (Hold, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createHoldCalls++
	if f.createHoldErr != nil {
		return Hold{}, false, f.createHoldErr
	}
	for _, h := range f.holds {
		if h.SeatID == in.SeatID && h.Status == HoldActive {
			if h.UserID == in.UserID {
				return h, false, nil
			}
			return Hold{}, false, ErrSeatHeld
		}
	}
	f.nextID++
	h := Hold{ID: f.nextID, ShowID: in.ShowID, SeatID: in.SeatID, UserID: in.UserID,
		Status: HoldActive, ExpiresAt: time.Now().Add(ttl), SecondsLeft: int64(ttl.Seconds())}
	f.holds[h.ID] = h
	return h, true, nil
}

// --- payments: a simplified in-memory version of repository_payments.go ---

func (f *fakeRepository) BeginPayment(ctx context.Context, in ConfirmInput) (Payment, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.holds[in.HoldID]
	if !ok || h.UserID != in.UserID {
		return Payment{}, false, ErrHoldNotFound
	}
	for _, p := range f.payments {
		if p.HoldID == h.ID && p.Status == PaymentPending {
			return p, false, nil // reuse the payment in flight
		}
	}
	switch h.Status {
	case HoldConsumed:
		return Payment{}, false, ErrHoldAlreadyUsed
	case HoldActive:
	default:
		return Payment{}, false, &HoldExpiredError{}
	}
	f.nextID++
	p := Payment{ID: f.nextID, HoldID: h.ID, UserID: h.UserID, AmountPaise: 25000,
		ChargeKey: fmt.Sprintf("pay-%d", f.nextID), Status: PaymentPending}
	f.payments[p.ID] = p
	return p, true, nil
}

func (f *fakeRepository) CompletePayment(ctx context.Context, paymentID int64, chargeID string) (CompleteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.completeErr != nil {
		return CompleteResult{}, f.completeErr
	}
	p := f.payments[paymentID]
	h := f.holds[p.HoldID]
	if p.Status == PaymentSucceeded {
		for _, b := range f.bookings {
			if b.HoldID == h.ID {
				return CompleteResult{Kind: Booked, Booking: b, Payment: p}, nil
			}
		}
	}
	if h.Status != HoldActive || f.expireDuringPayment {
		p.Status = PaymentRefundPending
		p.ProviderChargeID = chargeID
		f.payments[p.ID] = p
		f.nextID++
		return CompleteResult{Kind: RefundRequired, Payment: p, OutboxID: f.nextID, ShowID: h.ShowID, SeatID: h.SeatID}, nil
	}
	f.nextID++
	b := Booking{ID: f.nextID, ShowID: h.ShowID, SeatID: h.SeatID, UserID: h.UserID, HoldID: h.ID, CreatedAt: time.Now()}
	f.bookings[b.ID] = b
	h.Status = HoldConsumed
	f.holds[h.ID] = h
	p.Status = PaymentSucceeded
	p.ProviderChargeID = chargeID
	f.payments[p.ID] = p
	return CompleteResult{Kind: Booked, Booking: b, Payment: p}, nil
}

func (f *fakeRepository) MarkPaymentDeclined(ctx context.Context, paymentID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.payments[paymentID]
	p.Status = PaymentDeclined
	f.payments[paymentID] = p
	return nil
}

func (f *fakeRepository) MarkRefunded(ctx context.Context, paymentID, outboxID int64) (Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.payments[paymentID]
	p.Status = PaymentRefunded
	f.payments[paymentID] = p
	f.outboxDone = append(f.outboxDone, outboxID)
	return p, nil
}

func (f *fakeRepository) RecordOutboxFailure(ctx context.Context, outboxID int64, failure string) error {
	return nil
}

func (f *fakeRepository) ReleaseHold(ctx context.Context, userID string, holdID int64) (Hold, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.holds[holdID]
	if !ok || h.UserID != userID {
		return Hold{}, 0, ErrHoldNotFound
	}
	if h.Status != HoldActive {
		return Hold{}, 0, ErrHoldExpired
	}
	h.Status = HoldReleased
	f.holds[h.ID] = h
	f.nextID++
	return h, f.nextID, nil
}

func (f *fakeRepository) MarkOutboxDone(ctx context.Context, outboxID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outboxDone = append(f.outboxDone, outboxID)
	return nil
}

// fakeLocker implements SeatLocker in memory. Set acquireErr to pretend Redis
// is down, or heldBy to pretend another user holds the lock.
type fakeLocker struct {
	mu           sync.Mutex
	acquireErr   error
	releaseErr   error
	heldBy       string
	acquireCalls int
	releaseCalls int
}

func (l *fakeLocker) Acquire(ctx context.Context, showID, seatID int64, owner string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acquireCalls++
	if l.acquireErr != nil {
		return false, l.acquireErr
	}
	if l.heldBy != "" && l.heldBy != owner {
		return false, nil
	}
	return true, nil
}

func (l *fakeLocker) Release(ctx context.Context, showID, seatID int64, owner string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releaseCalls++
	return l.releaseErr
}

// newTestService wires a Service to fresh fakes and returns all three, so a
// test can configure the fakes and inspect what was called.
func newTestService() (*Service, *fakeRepository, *fakeLocker) {
	svc, repo, locker, _ := newTestServiceWithGateway()
	return svc, repo, locker
}

// newTestServiceWithGateway also returns the fake payment gateway.
func newTestServiceWithGateway() (*Service, *fakeRepository, *fakeLocker, *fakeGateway) {
	repo := newFakeRepository()
	locker := &fakeLocker{}
	gateway := &fakeGateway{}
	return NewService(repo, locker, gateway, 300*time.Second), repo, locker, gateway
}

// fakeGateway implements PaymentGateway. chargeErr simulates "no answer",
// decline makes every charge declined, refundErr makes refunds fail.
type fakeGateway struct {
	mu         sync.Mutex
	chargeErr  error
	decline    bool
	refundErr  error
	chargeKeys []string // every charge key received
	refunds    int
}

func (g *fakeGateway) Charge(ctx context.Context, req ChargeRequest) (ChargeResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.chargeKeys = append(g.chargeKeys, req.IdempotencyKey)
	if g.chargeErr != nil {
		return ChargeResult{}, g.chargeErr
	}
	if g.decline {
		return ChargeResult{ChargeID: "ch_" + req.IdempotencyKey, Status: ChargeDeclined}, nil
	}
	return ChargeResult{ChargeID: "ch_" + req.IdempotencyKey, Status: ChargeSucceeded}, nil
}

func (g *fakeGateway) Refund(ctx context.Context, req RefundRequest) (RefundResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refundErr != nil {
		return RefundResult{}, g.refundErr
	}
	g.refunds++
	return RefundResult{RefundID: "re_" + req.IdempotencyKey}, nil
}

// bookViaHold books a seat of show 1 the only way that exists: hold, then confirm.
func bookViaHold(t *testing.T, svc *Service, userID string, seatID int64) Booking {
	t.Helper()
	ctx := context.Background()
	h, _, err := svc.HoldSeat(ctx, HoldSeatInput{UserID: userID, ShowID: 1, SeatID: seatID})
	require.NoError(t, err)
	b, err := svc.ConfirmBooking(ctx, ConfirmInput{UserID: userID, HoldID: h.ID})
	require.NoError(t, err)
	return b
}

func TestListSeats_ShowsBookedStatus(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	bookViaHold(t, svc, "alice", 2)

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
	svc, _, _ := newTestService()

	_, err := svc.ListSeats(context.Background(), 42)

	assert.ErrorIs(t, err, ErrShowNotFound)
}

func TestGetBooking(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	created := bookViaHold(t, svc, "alice", 1)

	got, err := svc.GetBooking(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created, got)

	_, err = svc.GetBooking(ctx, 999)
	assert.ErrorIs(t, err, ErrBookingNotFound)

	_, err = svc.GetBooking(ctx, 0)
	assert.ErrorIs(t, err, ErrInvalidInput)
}
