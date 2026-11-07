-- Load-test data (make seed-load). Separate from the demo seed (seed.sql).
--
--   'Load: Rush'      1 show,      100 seats  the hot show for loadtest/rush.js
--   'Load: Smoke'     1 show,       20 seats  the CI smoke test (loadtest/smoke.js)
--   'Load: Steady N'  200 shows x 200 seats  (40,000 seats) for capacity, spike, soak
--
-- The k6 scripts find these shows by title (GET /v1/shows), so ids don't matter.
-- Idempotent: shows are inserted only if missing, seats use ON CONFLICT DO NOTHING.
-- `make reset-data` frees every seat again (it keeps shows and seats).

BEGIN;

INSERT INTO shows (title, venue, starts_at)
SELECT t.title, 'Load Test Arena', '2027-06-01 19:00:00+05:30'
FROM (
    SELECT 'Load: Rush' AS title
    UNION ALL SELECT 'Load: Smoke'
    UNION ALL SELECT 'Load: Steady ' || n FROM generate_series(1, 200) AS n
) AS t
WHERE NOT EXISTS (
    SELECT 1 FROM shows s WHERE s.title = t.title AND s.venue = 'Load Test Arena'
);

-- Seat labels: row letter + number, 10 seats per row (A1..A10, B1..B10, ...).
-- Rush: rows A-J (100). Smoke: rows A-B (20). Steady: rows A-T (200).
INSERT INTO seats (show_id, seat_label, price_paise)
SELECT s.id, chr(64 + r) || n, 25000
FROM shows s
CROSS JOIN generate_series(1, 20) AS r
CROSS JOIN generate_series(1, 10) AS n
WHERE s.venue = 'Load Test Arena'
  AND r <= CASE WHEN s.title = 'Load: Rush'  THEN 10
                WHEN s.title = 'Load: Smoke' THEN 2
                ELSE 20 END
ORDER BY s.id, r, n
ON CONFLICT ON CONSTRAINT seats_show_label_unique DO NOTHING;

COMMIT;

SELECT CASE WHEN sh.title LIKE 'Load: Steady %' THEN 'Load: Steady (200 shows)' ELSE sh.title END AS shows,
       COUNT(DISTINCT sh.id) AS show_count, COUNT(se.id) AS seats
FROM shows sh JOIN seats se ON se.show_id = sh.id
WHERE sh.venue = 'Load Test Arena'
GROUP BY 1 ORDER BY 1;
