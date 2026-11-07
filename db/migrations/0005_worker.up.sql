-- 0005_worker: what the background worker (cmd/worker) needs.
--
-- The worker repairs what the request path leaves behind:
--   - payments stuck in PENDING (a 504 nobody retried)   -> reconciler
--   - outbox rows whose inline attempt failed            -> outbox drainer
--   - ACTIVE holds past their expiry                     -> hold expirer
--   - old idempotency keys                               -> key cleanup

BEGIN;

-- ---------- payments ----------

-- ABANDONED: the reconciler found NO charge at the gateway and VOIDED the key
-- there (so a late request with that key can never take money). No money was
-- taken. Like DECLINED, it is not covered by payments_one_live_per_hold, so
-- the user may pay again (a new payment with a new charge_key).
ALTER TABLE payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments ADD CONSTRAINT payments_status_check
    CHECK (status IN ('PENDING', 'SUCCEEDED', 'DECLINED', 'REFUND_PENDING', 'REFUNDED', 'ABANDONED'));

-- Reconciliation state lives on the payment row itself (not in a separate job
-- table): one row = one source of truth, and claiming is one UPDATE.
--   next_check_at      : NULL = never checked. Otherwise the claim lease /
--                        backoff: the reconciler leaves the row alone until then.
--   reconcile_attempts : how many times the reconciler has claimed it.
--   last_error         : the last reason a check failed (e.g. gateway down).
ALTER TABLE payments
    ADD COLUMN next_check_at      TIMESTAMPTZ,
    ADD COLUMN reconcile_attempts INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN last_error         TEXT;

-- Why this index: the reconciler's scan is "PENDING payments created before
-- X". A partial index over PENDING rows only stays tiny, because almost every
-- payment leaves PENDING within seconds.
CREATE INDEX payments_pending_created ON payments (created_at) WHERE status = 'PENDING';

-- ---------- outbox ----------

-- FAILED: a "dead letter". The row was tried OUTBOX_MAX_ATTEMPTS times and is
-- no longer retried automatically; a human looks at it and re-queues it with
-- `make outbox-retry ID=<id>`.
ALTER TABLE outbox DROP CONSTRAINT outbox_status_check;
ALTER TABLE outbox ADD CONSTRAINT outbox_status_check
    CHECK (status IN ('PENDING', 'DONE', 'FAILED'));

-- Why this index: the drainer's scan is "PENDING rows due now"
-- (next_attempt_at <= NOW()). DONE rows, the vast majority, are not in it.
CREATE INDEX outbox_pending_due ON outbox (next_attempt_at) WHERE status = 'PENDING';

-- ---------- holds ----------

-- Why this index: the hold expirer's scan is "ACTIVE holds whose expires_at
-- has passed". Only ACTIVE rows are indexed, so history does not slow it down.
CREATE INDEX holds_active_expires ON holds (expires_at) WHERE status = 'ACTIVE';

-- ---------- idempotency keys ----------

-- Why this index: the cleanup job deletes COMPLETED keys whose updated_at
-- (completion time) is older than the retention window.
CREATE INDEX idempotency_keys_completed ON idempotency_keys (updated_at) WHERE status = 'COMPLETED';

COMMIT;
