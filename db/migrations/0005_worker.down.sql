-- Reverses 0005_worker.
--
-- The old CHECK constraints do not allow the new statuses, so rows using them
-- are mapped first to the closest old meaning:
--   payments ABANDONED -> DECLINED (both: no money taken, may pay again)
--   outbox   FAILED    -> PENDING  (back in the queue)
BEGIN;

DROP INDEX IF EXISTS idempotency_keys_completed;
DROP INDEX IF EXISTS holds_active_expires;

DROP INDEX IF EXISTS outbox_pending_due;
UPDATE outbox SET status = 'PENDING' WHERE status = 'FAILED';
ALTER TABLE outbox DROP CONSTRAINT outbox_status_check;
ALTER TABLE outbox ADD CONSTRAINT outbox_status_check CHECK (status IN ('PENDING', 'DONE'));

DROP INDEX IF EXISTS payments_pending_created;
ALTER TABLE payments
    DROP COLUMN IF EXISTS last_error,
    DROP COLUMN IF EXISTS reconcile_attempts,
    DROP COLUMN IF EXISTS next_check_at;
UPDATE payments SET status = 'DECLINED' WHERE status = 'ABANDONED';
ALTER TABLE payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments ADD CONSTRAINT payments_status_check
    CHECK (status IN ('PENDING', 'SUCCEEDED', 'DECLINED', 'REFUND_PENDING', 'REFUNDED'));

COMMIT;
