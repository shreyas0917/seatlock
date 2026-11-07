-- Reverses 0003_payments. Nothing references these tables, so the order
-- only matters for readability.
BEGIN;

DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS payments;

COMMIT;
