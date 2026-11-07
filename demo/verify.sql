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

-- ---------- background worker (Phase 4) ----------
-- These assume the worker is running: it resolves every abandoned payment,
-- drains refunds and expires holds well within these limits.

\echo 'CHECK 8: PENDING payments older than 15 minutes (the reconciler should have resolved them)'
SELECT id AS payment_id, hold_id, created_at
FROM payments
WHERE status = 'PENDING' AND created_at < NOW() - interval '15 minutes';

\echo 'CHECK 9: REFUND_PENDING payments older than 15 minutes (the outbox drainer should have refunded them)'
SELECT id AS payment_id, hold_id, updated_at
FROM payments
WHERE status = 'REFUND_PENDING' AND updated_at < NOW() - interval '15 minutes';

\echo 'CHECK 10: ACTIVE holds expired for more than 1 minute without a PENDING payment (the expirer should have expired them)'
SELECT h.id AS hold_id, h.expires_at
FROM holds h
WHERE h.status = 'ACTIVE' AND h.expires_at < NOW() - interval '1 minute'
  AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.hold_id = h.id AND p.status = 'PENDING');

\echo 'CHECK 11: ABANDONED payments that have a booking (no money was taken for them)'
SELECT p.id AS payment_id, b.id AS booking_id
FROM payments p
JOIN bookings b ON b.hold_id = p.hold_id
WHERE p.status = 'ABANDONED'
  AND NOT EXISTS (SELECT 1 FROM payments s WHERE s.hold_id = p.hold_id AND s.status = 'SUCCEEDED');
