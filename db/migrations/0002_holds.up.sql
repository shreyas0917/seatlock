-- 0002_holds: seat holds, and every booking must come from a hold.
--
-- Flow from Phase 2 on:  hold a seat (5 minutes)  ->  confirm the hold  ->  booking.
--
-- PostgreSQL is the AUTHORITY for holds. Redis (internal/lock) is only a fast
-- first filter; whatever Redis says, these constraints have the final word.

BEGIN;

CREATE TABLE holds (
    id         BIGSERIAL   PRIMARY KEY,
    show_id    BIGINT      NOT NULL,
    seat_id    BIGINT      NOT NULL,
    user_id    TEXT        NOT NULL,
    -- ACTIVE   : the user holds the seat until expires_at
    -- CONSUMED : the hold was confirmed and turned into a booking
    -- RELEASED : the user cancelled the hold
    -- EXPIRED  : expires_at passed without a confirm
    status     TEXT        NOT NULL DEFAULT 'ACTIVE',
    -- Always computed with the DATABASE clock (NOW() + interval), never with
    -- the Go server's clock, so every API instance agrees on when a hold ends.
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Only these four statuses can ever be stored; a typo in Go code fails loudly.
    CONSTRAINT holds_status_check
        CHECK (status IN ('ACTIVE', 'CONSUMED', 'RELEASED', 'EXPIRED')),

    -- Same pattern as bookings: the seat must belong to the show.
    CONSTRAINT holds_seat_fk FOREIGN KEY (show_id, seat_id)
        REFERENCES seats (show_id, id)
);

-- *** DATABASE-LEVEL GUARANTEE: AT MOST ONE ACTIVE HOLD PER SEAT ***
-- A PARTIAL unique index only contains rows that match its WHERE clause. So
-- uniqueness is enforced only among ACTIVE holds: a seat can have any number
-- of old CONSUMED / RELEASED / EXPIRED holds (history), but never two ACTIVE
-- ones, even if application code or Redis gets it wrong.
--
-- Why this index (lookups): every query that asks "what is the active hold
-- for this seat?" uses it - the hold transaction (expire old + check
-- existing), and the seat map's HELD status. Because it only contains ACTIVE
-- rows, it stays small however much history piles up.
CREATE UNIQUE INDEX holds_one_active_per_seat
    ON holds (show_id, seat_id)
    WHERE status = 'ACTIVE';

-- No other index on holds is needed yet: confirm and cancel look a hold up by
-- its primary key (id), which already has an index.

-- Every booking must now point at the hold it came from.
--
-- Phase 1 bookings have no hold, and a NOT NULL column cannot be added to a
-- table with rows that have no value for it. TRUNCATE is SAFE HERE because
-- the only bookings so far are demo/test data. (On a real production table we
-- would instead add the column as NULL, backfill it, then add NOT NULL.)
TRUNCATE bookings;

ALTER TABLE bookings
    ADD COLUMN hold_id BIGINT NOT NULL
        -- One hold can produce at most ONE booking (a second confirm of the
        -- same hold fails with 23505 on this constraint).
        CONSTRAINT bookings_hold_unique UNIQUE
        -- A booking must reference a real hold.
        CONSTRAINT bookings_hold_fk REFERENCES holds (id);

COMMIT;
