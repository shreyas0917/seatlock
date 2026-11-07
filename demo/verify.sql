-- verify.sql - invariant checks. In a correct system EVERY query returns 0 rows.
-- Used by demo/race.sh and demo/redis-down.sh (they count the "(0 rows)" lines).

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
