-- Demo data: 1 show with 100 seats (rows A-J, numbers 1-10), each Rs 250.
--
-- Idempotent: running this file twice changes nothing the second time.
--   * the show is inserted only if a show with the same title+venue does not exist
--   * seats use ON CONFLICT DO NOTHING on the (show_id, seat_label) unique constraint
-- Run with: make seed

BEGIN;

INSERT INTO shows (title, venue, starts_at)
SELECT 'Coldplay Live', 'DY Patil Stadium, Navi Mumbai', '2027-01-25 19:00:00+05:30'
WHERE NOT EXISTS (
    SELECT 1 FROM shows
    WHERE title = 'Coldplay Live' AND venue = 'DY Patil Stadium, Navi Mumbai'
);

-- Build all 100 seats with a cross join of 10 row letters x 10 seat numbers.
-- WITH ORDINALITY numbers the row letters 1..10 so seats are inserted in order
-- A1..A10, B1..B10, ... (so on a fresh database seat ids are 1..100).
INSERT INTO seats (show_id, seat_label, price_paise)
SELECT s.id, r.row_letter || n.seat_number, 25000
FROM shows s
CROSS JOIN unnest(ARRAY['A','B','C','D','E','F','G','H','I','J'])
           WITH ORDINALITY AS r(row_letter, row_order)
CROSS JOIN generate_series(1, 10) AS n(seat_number)
WHERE s.title = 'Coldplay Live' AND s.venue = 'DY Patil Stadium, Navi Mumbai'
ORDER BY r.row_order, n.seat_number
ON CONFLICT ON CONSTRAINT seats_show_label_unique DO NOTHING;

COMMIT;

-- Show what is in the database now.
SELECT sh.id AS show_id, sh.title, COUNT(se.id) AS seats
FROM shows sh LEFT JOIN seats se ON se.show_id = sh.id
GROUP BY sh.id, sh.title
ORDER BY sh.id;
