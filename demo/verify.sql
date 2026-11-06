-- Double-booking detector: lists every (show, seat) pair that has more than
-- one booking. In a correct system this ALWAYS returns 0 rows.
SELECT show_id, seat_id, COUNT(*)
FROM bookings
GROUP BY show_id, seat_id
HAVING COUNT(*) > 1;
