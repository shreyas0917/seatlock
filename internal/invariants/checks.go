// Package invariants is the ONE definition of SeatLock's correctness checks:
// queries that list rows breaking a rule that must always hold (no double
// booking, no money taken without a seat or a refund, ...). In a correct
// system every check returns 0 rows.
//
// The same list is used in two places, so they can never drift apart:
//   - demo/verify.sql is GENERATED from it (`make verify-sql`), and a unit
//     test fails if the committed file differs from the list;
//   - the worker's "invariants" job runs every check every 15s and exports
//     the row counts as the gauge seatlock_invariant_violations{check}, so
//     a violation becomes an ALERT (DoubleBookingDetected) instead of a line
//     somebody might notice in a log one day.
package invariants

import (
	"fmt"
	"strings"
)

// Check is one invariant: a name (the metric label: short, fixed), a
// human description, and a query that returns the VIOLATING rows.
type Check struct {
	Name        string
	Description string
	SQL         string
}

// Checks is the list, in display order. Keep queries cheap: they run every
// 15 seconds against the live database (see Store).
var Checks = []Check{
	// ---------- bookings and holds (Phase 1-2) ----------
	{
		Name:        "double_booking",
		Description: "seats booked more than once",
		SQL: `SELECT show_id, seat_id, COUNT(*)
FROM bookings
GROUP BY show_id, seat_id
HAVING COUNT(*) > 1`,
	},
	{
		Name:        "booking_hold_mismatch",
		Description: "bookings whose hold is missing, not CONSUMED, or for a different user/seat/show",
		SQL: `SELECT b.id AS booking_id, b.hold_id, h.status, b.user_id, h.user_id AS hold_user
FROM bookings b
LEFT JOIN holds h ON h.id = b.hold_id
WHERE h.id IS NULL
   OR h.status <> 'CONSUMED'
   OR h.user_id <> b.user_id
   OR h.seat_id <> b.seat_id
   OR h.show_id <> b.show_id`,
	},
	{
		Name:        "multiple_active_holds",
		Description: "seats with more than one ACTIVE hold",
		SQL: `SELECT show_id, seat_id, COUNT(*)
FROM holds
WHERE status = 'ACTIVE'
GROUP BY show_id, seat_id
HAVING COUNT(*) > 1`,
	},
	// ---------- payments (Phase 3) ----------
	{
		Name:        "booking_without_payment",
		Description: "bookings without exactly one SUCCEEDED payment for their hold",
		SQL: `SELECT b.id AS booking_id, b.hold_id, COUNT(p.id) AS succeeded_payments
FROM bookings b
LEFT JOIN payments p ON p.hold_id = b.hold_id AND p.status = 'SUCCEEDED'
GROUP BY b.id, b.hold_id
HAVING COUNT(p.id) <> 1`,
	},
	{
		Name:        "paid_without_booking",
		Description: "SUCCEEDED payments whose hold has no booking (money taken, nothing sold)",
		SQL: `SELECT p.id AS payment_id, p.hold_id
FROM payments p
LEFT JOIN bookings b ON b.hold_id = p.hold_id
WHERE p.status = 'SUCCEEDED'
  AND b.id IS NULL`,
	},
	{
		Name:        "multiple_live_payments",
		Description: "holds with more than one PENDING/SUCCEEDED payment",
		SQL: `SELECT hold_id, COUNT(*)
FROM payments
WHERE status IN ('PENDING', 'SUCCEEDED')
GROUP BY hold_id
HAVING COUNT(*) > 1`,
	},
	{
		Name:        "refunded_with_booking",
		Description: "REFUND_PENDING/REFUNDED payments whose hold has a booking (refunded a sold seat)",
		SQL: `SELECT p.id AS payment_id, p.hold_id, p.status, b.id AS booking_id
FROM payments p
JOIN bookings b ON b.hold_id = p.hold_id
WHERE p.status IN ('REFUND_PENDING', 'REFUNDED')`,
	},
	// ---------- background worker (Phase 4) ----------
	// These assume the worker is running: it resolves every abandoned
	// payment, drains refunds and expires holds well within these limits.
	{
		Name:        "payment_pending_too_long",
		Description: "PENDING payments older than 15 minutes (the reconciler should have resolved them)",
		SQL: `SELECT id AS payment_id, hold_id, created_at
FROM payments
WHERE status = 'PENDING' AND created_at < NOW() - interval '15 minutes'`,
	},
	{
		Name:        "refund_pending_too_long",
		Description: "REFUND_PENDING payments older than 15 minutes (the outbox drainer should have refunded them)",
		SQL: `SELECT id AS payment_id, hold_id, updated_at
FROM payments
WHERE status = 'REFUND_PENDING' AND updated_at < NOW() - interval '15 minutes'`,
	},
	{
		Name:        "expired_hold_not_cleaned",
		Description: "ACTIVE holds expired for more than 1 minute without a PENDING payment (the expirer should have expired them)",
		SQL: `SELECT h.id AS hold_id, h.expires_at
FROM holds h
WHERE h.status = 'ACTIVE' AND h.expires_at < NOW() - interval '1 minute'
  AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.hold_id = h.id AND p.status = 'PENDING')`,
	},
	{
		Name:        "abandoned_with_booking",
		Description: "ABANDONED payments that have a booking (no money was taken for them)",
		SQL: `SELECT p.id AS payment_id, b.id AS booking_id
FROM payments p
JOIN bookings b ON b.hold_id = p.hold_id
WHERE p.status = 'ABANDONED'
  AND NOT EXISTS (SELECT 1 FROM payments s WHERE s.hold_id = p.hold_id AND s.status = 'SUCCEEDED')`,
	},
}

// RenderVerifySQL renders Checks as demo/verify.sql (a psql script: each
// check is announced with \echo so the demos can count "CHECK" and "(0 rows)").
func RenderVerifySQL() string {
	var b strings.Builder
	b.WriteString(`-- verify.sql - invariant checks. In a correct system EVERY query returns 0 rows.
--
-- GENERATED from internal/invariants/checks.go by "make verify-sql". Do not
-- edit by hand: the worker exports the SAME checks as the live gauge
-- seatlock_invariant_violations{check}, and a unit test fails if this file
-- and the Go list differ.
--
-- Used by every demo script (they count the "CHECK" and "(0 rows)" lines).
`)
	for i, c := range Checks {
		fmt.Fprintf(&b, "\n\\echo 'CHECK %d: %s (%s)'\n%s;\n", i+1, c.Description, c.Name, c.SQL)
	}
	return b.String()
}
