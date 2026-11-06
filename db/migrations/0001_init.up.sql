-- 0001_init: shows, seats and bookings.
--
-- PostgreSQL is the SOURCE OF TRUTH for SeatLock. The constraints below are
-- not decoration: they are what actually stops a seat from being sold twice,
-- no matter how many API servers or goroutines race each other.

-- A show is one performance at one venue at one time.
CREATE TABLE shows (
    id         BIGSERIAL   PRIMARY KEY,
    title      TEXT        NOT NULL,
    venue      TEXT        NOT NULL,
    starts_at  TIMESTAMPTZ NOT NULL,  -- TIMESTAMPTZ stores an exact instant (UTC internally)
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Why this index: GET /v1/shows lists shows ordered by starts_at.
-- Without it Postgres must sort the whole table on every request.
CREATE INDEX idx_shows_starts_at ON shows (starts_at);

-- A seat belongs to exactly one show.
CREATE TABLE seats (
    id          BIGSERIAL PRIMARY KEY,
    show_id     BIGINT    NOT NULL REFERENCES shows (id) ON DELETE CASCADE,
    seat_label  TEXT      NOT NULL,  -- e.g. 'A7'
    -- Money is stored as an integer number of paise (1 rupee = 100 paise).
    -- Floats cannot represent most decimal values exactly (0.1 + 0.2 != 0.3),
    -- so they must never be used for money.
    price_paise INTEGER   NOT NULL CHECK (price_paise >= 0),

    -- A show cannot have two seats called 'A7'.
    -- Why this index (created automatically by UNIQUE): its first column is
    -- show_id, so it also makes "all seats of show X" (the seat map) fast.
    -- That is why there is no separate index on seats(show_id).
    CONSTRAINT seats_show_label_unique UNIQUE (show_id, seat_label),

    -- Lets bookings reference the PAIR (show_id, id) with a foreign key
    -- (a foreign key must point at a unique column set). See bookings below.
    CONSTRAINT seats_show_id_unique UNIQUE (show_id, id)
);

-- A booking says: this user owns this seat for this show.
CREATE TABLE bookings (
    id         BIGSERIAL   PRIMARY KEY,
    show_id    BIGINT      NOT NULL,
    seat_id    BIGINT      NOT NULL,
    user_id    TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Foreign key to seats on BOTH columns: the database itself guarantees
    -- the seat really belongs to that show, even if application code has a bug.
    CONSTRAINT bookings_seat_fk FOREIGN KEY (show_id, seat_id)
        REFERENCES seats (show_id, id),

    -- *** THE CORE SAFETY NET OF THE WHOLE PROJECT ***
    -- At most ONE booking per (show, seat). If two transactions insert the
    -- same pair at the same moment, Postgres lets exactly one commit and the
    -- other fails with error 23505 (unique_violation). The Go code maps that
    -- error to ErrSeatAlreadyBooked -> HTTP 409.
    -- Why this index (created automatically by UNIQUE): it also serves the
    -- seat-map query (LEFT JOIN bookings ON show_id, seat_id) and the
    -- foreign key above, so no extra index on bookings is needed in Phase 1.
    CONSTRAINT bookings_show_seat_unique UNIQUE (show_id, seat_id)
);
