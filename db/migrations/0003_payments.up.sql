-- 0003_payments: payments, idempotency keys and the outbox.
--
-- Flow from Phase 3 on:  hold -> confirm = PAY, then book  (or refund).
-- Everything the confirm/payment path needs lives in PostgreSQL, the
-- authority. Nothing in it depends on Redis, so confirm keeps working when
-- Redis is down ("Option B").

BEGIN;

-- One row per attempt to charge the user for a hold.
CREATE TABLE payments (
    id                 BIGSERIAL   PRIMARY KEY,
    hold_id            BIGINT      NOT NULL REFERENCES holds (id),
    user_id            TEXT        NOT NULL,
    amount_paise       INTEGER     NOT NULL CHECK (amount_paise > 0),
    -- The idempotency key sent to the payment gateway. It is DERIVED from
    -- the payment id ("pay-42"), so every retry of THIS payment re-sends the
    -- SAME key, and the gateway returns its stored result instead of
    -- charging again. A generated column means the key can never drift
    -- from the id.
    charge_key         TEXT        GENERATED ALWAYS AS ('pay-' || id::text) STORED
                                   CONSTRAINT payments_charge_key_unique UNIQUE,
    provider_charge_id TEXT,       -- the gateway's charge id, once known
    -- PENDING        : sent (or about to be sent) to the gateway, outcome unknown
    -- SUCCEEDED      : money taken AND the booking exists
    -- DECLINED       : the gateway said no; no money taken
    -- REFUND_PENDING : money taken, but the hold was no longer valid -> must refund
    -- REFUNDED       : the refund went through
    status             TEXT        NOT NULL DEFAULT 'PENDING',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT payments_status_check
        CHECK (status IN ('PENDING', 'SUCCEEDED', 'DECLINED', 'REFUND_PENDING', 'REFUNDED'))
);

-- *** AT MOST ONE LIVE PAYMENT PER HOLD ***
-- A hold may have at most ONE payment that is PENDING (in flight) or
-- SUCCEEDED. So two confirm requests for one hold can never start two
-- separate charges, even with different Idempotency-Keys: the second one
-- finds and reuses the first one's PENDING payment (and its charge_key).
--
-- DECLINED (and refunded) payments are NOT covered on purpose: a declined
-- card took no money, so the user may try again (another card, more
-- balance) and that new attempt needs a new payment row and a new
-- charge_key. A declined key must not be reused: the gateway would just
-- replay "declined".
--
-- Why this index (lookups): "the live payment of this hold" is exactly the
-- question confirm asks.
CREATE UNIQUE INDEX payments_one_live_per_hold
    ON payments (hold_id)
    WHERE status IN ('PENDING', 'SUCCEEDED');

-- Idempotency keys for POST /v1/bookings.
--
-- WHY POSTGRES AND NOT REDIS: confirm must keep working while Redis is down
-- (existing reservations may complete). If idempotency lived in Redis, a
-- Redis outage would force a terrible choice: refuse every confirm, or
-- process them WITHOUT duplicate protection and risk charging twice. In
-- Postgres the key shares the fate of the data it protects.
CREATE TABLE idempotency_keys (
    user_id         TEXT        NOT NULL,
    key             TEXT        NOT NULL CHECK (length(key) BETWEEN 1 AND 64),
    -- SHA-256 of the request (method, path, body). Same key + different
    -- request = client bug -> 422.
    request_hash    TEXT        NOT NULL,
    -- IN_PROGRESS : a request is working on it (or crashed while doing so)
    -- COMPLETED   : the final response is stored below and is replayed
    status          TEXT        NOT NULL,
    -- The LEASE: a request owns the key until locked_until. If the server
    -- crashes mid-request, the lease runs out and a retry may take over,
    -- so a crash never blocks a key forever. lease_token proves WHICH
    -- request holds the lease (see internal/idempotency).
    lease_token     TEXT,
    locked_until    TIMESTAMPTZ,
    response_status INTEGER,
    response_body   JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Keys are per user: two users may pick the same key string.
    PRIMARY KEY (user_id, key),
    CONSTRAINT idempotency_status_check CHECK (status IN ('IN_PROGRESS', 'COMPLETED'))
);

-- The transactional outbox: side effects on OTHER systems (refund money at
-- the gateway, delete a Redis lock) that must happen eventually.
--
-- An outbox row is ALWAYS written in the SAME transaction as the state
-- change it belongs to. Why, with a crash example:
--
--   1. COMMIT: payment -> REFUND_PENDING        (database)
--   2. -- server crashes here --
--   3. call the gateway: refund                 (never happens)
--
-- Without an outbox, the user paid, has no booking, and nothing remembers
-- that a refund is owed. With the outbox, step 1 commits the REFUND row
-- together with the status change: both exist or neither does. Whatever
-- crashes afterwards, the owed refund is written down, and a worker
-- (Phase 4) retries it until it is DONE.
CREATE TABLE outbox (
    id              BIGSERIAL   PRIMARY KEY,
    kind            TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'PENDING',
    attempts        INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT outbox_kind_check   CHECK (kind IN ('REFUND', 'RELEASE_LOCK')),
    CONSTRAINT outbox_status_check CHECK (status IN ('PENDING', 'DONE'))
);
-- No extra index yet: Phase 3 only updates outbox rows by primary key.
-- Phase 4's worker will add an index for "PENDING rows due now".

COMMIT;
