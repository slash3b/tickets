CREATE SCHEMA IF NOT EXISTS orders;

-- The purchase, and its position in the saga.
--
--   created           row exists, hold is still active
--   awaiting_payment  hold moved to converting, charge attempted
--   paid              money has moved. SEATS ARE NOT YET SOLD.
--   confirmed         seats sold, hold consumed. Terminal, happy.
--   failed            declined or abandoned. Hold released. Terminal.
--   reconciling       we hold money but could not deliver seats. Needs a refund.
--   refunded          money returned. Terminal, unhappy but correct.
--
-- EVERYTHING INTERESTING LIVES IN THE GAP BETWEEN `paid` AND `confirmed`. It is
-- crossed by FORWARD RECOVERY, never by rollback: once money has moved the seats
-- are still held in `converting` and nobody else can take them, so the right
-- answer is to retry the commit rather than to un-charge. Only the hard deadline
-- sends an order to `reconciling`.
CREATE TABLE IF NOT EXISTS orders.orders (
    id         uuid PRIMARY KEY,
    hold_id    uuid NOT NULL UNIQUE,
    event_id   uuid NOT NULL,
    user_id    uuid NOT NULL,
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    state      text NOT NULL CHECK (state IN
        ('created','awaiting_payment','paid','confirmed','failed','reconciling','refunded')),
    failure_reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The resumer's work queue: anything not in a terminal state.
CREATE INDEX IF NOT EXISTS orders_in_flight_idx
    ON orders.orders (state, updated_at)
    WHERE state IN ('created','awaiting_payment','paid','reconciling');

-- Every step, written BEFORE it is attempted.
--
-- This is what makes a crash resumable rather than restartable. Without it, a
-- process that dies mid-saga leaves a row saying `awaiting_payment` and no way to
-- know whether the charge was actually sent — and the only safe assumption then
-- is the expensive one.
CREATE TABLE IF NOT EXISTS orders.saga_log (
    id         bigserial PRIMARY KEY,
    order_id   uuid NOT NULL REFERENCES orders.orders(id) ON DELETE CASCADE,
    step       text NOT NULL,
    outcome    text NOT NULL CHECK (outcome IN ('attempting','ok','failed')),
    detail     text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS saga_log_order_idx ON orders.saga_log (order_id, id);
