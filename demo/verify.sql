-- verify.sql - invariant checks. In a correct system EVERY query returns 0 rows.
-- Used by every demo script (they count the "CHECK" and "(0 rows)" lines).

-- ---------- bookings and holds (Phase 1-2) ----------

\echo 'CHECK 1: seats booked more than once'
SELECT show_id, seat_id, COUNT(*)
FROM bookings
GROUP BY show_id, seat_id
HAVING COUNT(*) > 1;

\echo 'CHECK 2: bookings whose hold is missing, not CONSUMED, or for a different user/seat/show'
SELECT b.id AS booking_id, b.hold_id, h.status, b.user_id, h.user_id AS hold_user
FROM bookings b
LEFT JOIN holds h ON h.id = b.hold_id
WHERE h.id IS NULL
   OR h.status <> 'CONSUMED'
   OR h.user_id <> b.user_id
   OR h.seat_id <> b.seat_id
   OR h.show_id <> b.show_id;

\echo 'CHECK 3: seats with more than one ACTIVE hold'
SELECT show_id, seat_id, COUNT(*)
FROM holds
WHERE status = 'ACTIVE'
GROUP BY show_id, seat_id
HAVING COUNT(*) > 1;

-- ---------- payments (Phase 3) ----------

\echo 'CHECK 4: bookings without exactly one SUCCEEDED payment for their hold'
SELECT b.id AS booking_id, b.hold_id, COUNT(p.id) AS succeeded_payments
FROM bookings b
LEFT JOIN payments p ON p.hold_id = b.hold_id AND p.status = 'SUCCEEDED'
GROUP BY b.id, b.hold_id
HAVING COUNT(p.id) <> 1;

\echo 'CHECK 5: SUCCEEDED payments whose hold has no booking (money taken, nothing sold)'
SELECT p.id AS payment_id, p.hold_id
FROM payments p
LEFT JOIN bookings b ON b.hold_id = p.hold_id
WHERE p.status = 'SUCCEEDED'
  AND b.id IS NULL;

\echo 'CHECK 6: holds with more than one PENDING/SUCCEEDED payment'
SELECT hold_id, COUNT(*)
FROM payments
WHERE status IN ('PENDING', 'SUCCEEDED')
GROUP BY hold_id
HAVING COUNT(*) > 1;

\echo 'CHECK 7: REFUND_PENDING/REFUNDED payments whose hold has a booking (refunded a sold seat)'
SELECT p.id AS payment_id, p.hold_id, p.status, b.id AS booking_id
FROM payments p
JOIN bookings b ON b.hold_id = p.hold_id
WHERE p.status IN ('REFUND_PENDING', 'REFUNDED');
