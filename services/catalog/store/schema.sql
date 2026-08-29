CREATE SCHEMA IF NOT EXISTS catalog;

-- A physical building. kind drives nothing technical today, but it is the seam
-- along which the two contention regimes differ: a cinema is 100-300 seats with
-- mild, broad load; an arena is 20,000 with everybody arriving in the same minute.
CREATE TABLE IF NOT EXISTS catalog.venues (
    id   uuid PRIMARY KEY,
    name text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('cinema','arena'))
);

-- Floor, tier, box — or for a cinema, just "the room".
--
-- Sections exist so that a seat map is never fetched or pushed whole. A
-- 20,000-seat arena chart cannot move as one blob, and building the API around
-- sections from the start is what stops that being a rewrite at milestone 8.
CREATE TABLE IF NOT EXISTS catalog.sections (
    id            uuid PRIMARY KEY,
    venue_id      uuid NOT NULL REFERENCES catalog.venues(id) ON DELETE CASCADE,
    name          text NOT NULL,
    display_order int  NOT NULL DEFAULT 0
);

-- A physical seat. Belongs to the venue, NOT to an event — it is written once and
-- read constantly, and it is never created or destroyed by ticket sales.
CREATE TABLE IF NOT EXISTS catalog.seats (
    id          uuid PRIMARY KEY,
    section_id  uuid NOT NULL REFERENCES catalog.sections(id) ON DELETE CASCADE,
    row_label   text NOT NULL,
    seat_number int  NOT NULL,
    -- Coordinates for drawing the map. Kept here so the frontend never computes
    -- geometry from row/number and get it wrong differently to everyone else.
    x numeric(6,2) NOT NULL DEFAULT 0,
    y numeric(6,2) NOT NULL DEFAULT 0,
    UNIQUE (section_id, row_label, seat_number)
);

CREATE INDEX IF NOT EXISTS seats_section_idx ON catalog.seats (section_id);

-- A showing. on_sale_at is first-class from day one even though for movies it is
-- always in the past — the entire concert case is about that one timestamp.
CREATE TABLE IF NOT EXISTS catalog.events (
    id         uuid PRIMARY KEY,
    venue_id   uuid NOT NULL REFERENCES catalog.venues(id),
    title      text NOT NULL,
    starts_at  timestamptz NOT NULL,
    on_sale_at timestamptz NOT NULL,
    -- When inventory was told to open this event's seats.
    --
    -- OPENING THE SEATS IS THE ON-SALE. Before it happens there are no rows in
    -- inventory.event_seats, so a hold fails on its own — no on_sale_at check on
    -- the hot path, and no window between checking the clock and taking a seat.
    -- NULL means not yet opened; the workers loop fills it in once.
    seats_opened_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Idempotent for an existing database: the column was added at milestone 8.
ALTER TABLE catalog.events ADD COLUMN IF NOT EXISTS seats_opened_at timestamptz;

CREATE INDEX IF NOT EXISTS events_on_sale_idx ON catalog.events (on_sale_at, starts_at);

-- Price per section per event. Real arenas price tiers differently; a cinema has
-- one row here.
CREATE TABLE IF NOT EXISTS catalog.event_prices (
    event_id    uuid NOT NULL REFERENCES catalog.events(id) ON DELETE CASCADE,
    section_id  uuid NOT NULL REFERENCES catalog.sections(id),
    price_minor bigint NOT NULL CHECK (price_minor > 0),
    PRIMARY KEY (event_id, section_id)
);
