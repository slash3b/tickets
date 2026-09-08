-- The contended core. Three tables, and the whole system's correctness rests on
-- the constraints here rather than on application code being careful.

CREATE SCHEMA IF NOT EXISTS inventory;

-- A hold is a temporary claim. Its STATE, not the seat's, carries the lifecycle.
--
--   active      user is at checkout. The short TTL applies and the sweeper may expire it.
--   converting  payment is in flight. THE SHORT TTL NO LONGER APPLIES.
--   consumed    order confirmed, seats sold. Terminal.
--   released    seats went back to the pool. Terminal.
--
-- Why `converting` exists is the single most important decision in this design:
-- if the short TTL kept running while the bank was thinking, a slow bank would
-- expire the hold, the sweeper would return the seats, someone else would buy
-- them, and only then would the payment succeed. Taking money for a seat you
-- cannot deliver is the worst outcome available, and it would be self-inflicted.
CREATE TABLE IF NOT EXISTS inventory.holds (
    id            uuid PRIMARY KEY,
    event_id      uuid        NOT NULL,
    state         text        NOT NULL CHECK (state IN ('active','converting','consumed','released')),
    expires_at    timestamptz NOT NULL,             -- short TTL, only meaningful while active
    hard_deadline timestamptz NOT NULL,             -- backstop, applies while converting
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    -- WHY a hold ended. Durable, because "who held this seat when it was lost"
    -- and "did this hold die of a slow bank or an abandoned checkout" are
    -- different questions with different consequences, and the difference is
    -- invisible once the row just says 'released'.
    released_reason text CHECK (released_reason IN
        ('expired','hard_deadline','cancelled','consumed'))
);

CREATE INDEX IF NOT EXISTS holds_sweep_idx
    ON inventory.holds (state, expires_at);

-- THE IDEMPOTENCY KEY, so a retried hold returns the original instead of
-- claiming seats twice.
--
-- ADDED AFTER THE TABLE EXISTED, which pkg/migrate says it cannot do — it only
-- applies CREATE ... IF NOT EXISTS and has no versions. ADD COLUMN IF NOT EXISTS
-- is the one alteration that is genuinely idempotent and order-independent, so it
-- is safe under that scheme. Anything that CHANGES an existing column still is
-- not, and still needs a real migration tool first.
ALTER TABLE inventory.holds ADD COLUMN IF NOT EXISTS idempotency_key text;

-- PARTIAL, because the key is optional and most holds will not carry one. A
-- plain UNIQUE would also work — Postgres treats NULLs as distinct — but it would
-- index every keyless row for nothing, on a table the sweeper already scans on a
-- ticker.
--
-- THIS CONSTRAINT IS THE WHOLE MECHANISM. Two concurrent requests carrying the
-- same key cannot both insert; the loser gets 23505 and re-reads the winner's
-- hold. Enforcing it in the database rather than by a check-then-insert in Go is
-- what makes it true under concurrency instead of usually true.
CREATE UNIQUE INDEX IF NOT EXISTS holds_idempotency_key_idx
    ON inventory.holds (idempotency_key) WHERE idempotency_key IS NOT NULL;

-- The contended row. One per sellable seat per event.
--
-- status is the ONLY thing that decides whether a seat can be sold, and it has
-- exactly three values. Anything more granular belongs on the hold. Keeping this
-- machine this small is what makes the oversell invariant checkable by eye.
CREATE TABLE IF NOT EXISTS inventory.event_seats (
    event_id   uuid        NOT NULL,
    seat_id    uuid        NOT NULL,
    status     text        NOT NULL CHECK (status IN ('available','held','sold')),
    hold_id    uuid        REFERENCES inventory.holds(id),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, seat_id),

    -- THE OVERSELL INVARIANT, ENFORCED BY THE DATABASE.
    --
    -- A seat that is held or sold must name the hold that owns it; an available
    -- seat must name none. Application code cannot violate this even by accident,
    -- which is the point: correctness that depends on every future caller
    -- remembering a rule is not correctness.
    CONSTRAINT seat_hold_consistency CHECK (
        (status = 'available' AND hold_id IS NULL) OR
        (status IN ('held','sold') AND hold_id IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS event_seats_hold_idx
    ON inventory.event_seats (hold_id) WHERE hold_id IS NOT NULL;

-- Which seats a hold claimed. Kept separate from event_seats so that history
-- survives the seat going back to 'available' — without it, "who held this seat
-- when it was lost" has no answer.
CREATE TABLE IF NOT EXISTS inventory.hold_seats (
    hold_id  uuid NOT NULL REFERENCES inventory.holds(id) ON DELETE CASCADE,
    event_id uuid NOT NULL,
    seat_id  uuid NOT NULL,
    PRIMARY KEY (hold_id, seat_id)
);

CREATE INDEX IF NOT EXISTS hold_seats_seat_idx
    ON inventory.hold_seats (event_id, seat_id);
