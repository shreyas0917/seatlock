-- Reverses 0002_holds. bookings.hold_id references holds, so the column (and
-- its constraints) must go before the holds table can be dropped.
BEGIN;

ALTER TABLE bookings DROP COLUMN IF EXISTS hold_id;
DROP TABLE IF EXISTS holds;

COMMIT;
