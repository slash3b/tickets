CREATE SCHEMA IF NOT EXISTS payments;

-- One row per payment attempt for an order.
--
-- The state machine has FOUR states, and the fourth is the one that matters:
--
--   pending    created, not yet answered by the bank
--   succeeded  money moved. Terminal.
--   failed     the bank explicitly declined. Terminal, and definite.
--   unknown    THE BANK DID NOT ANSWER. Money may or may not have moved.
--
-- `unknown` is not a variant of `failed`. Collapsing the two is the single most
-- expensive mistake available here: it means refusing a customer who has already
-- been charged, or re-charging one who was. A payment sits in `unknown` until the
-- reconciler establishes what actually happened.
CREATE TABLE IF NOT EXISTS payments.payments (
    id           uuid PRIMARY KEY,

    -- One payment per order. The UNIQUE constraint is what stops a duplicated
    -- request creating a second charge — enforced by the database rather than by
    -- every caller remembering to check first.
    order_id     uuid NOT NULL UNIQUE,

    -- Derived from order_id, never random and never from a clock, so a retry
    -- after a crash produces the SAME key and the bank recognises it. A key that
    -- changes between attempts makes the bank's idempotency useless.
    idempotency_key text NOT NULL UNIQUE,

    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    state        text   NOT NULL CHECK (state IN ('pending','succeeded','failed','unknown')),

    bank_charge_id text,
    decline_code   text,

    -- How many times the reconciler has asked. Rising numbers on one row mean the
    -- bank cannot answer about it, which is a different problem from a payment
    -- that simply has not been tried yet.
    reconcile_attempts int NOT NULL DEFAULT 0,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- A succeeded payment must name its charge; a failed one must name a reason.
    -- Again: enforced here rather than trusted to callers.
    CONSTRAINT succeeded_has_charge CHECK (
        state <> 'succeeded' OR bank_charge_id IS NOT NULL),
    CONSTRAINT failed_has_reason CHECK (
        state <> 'failed' OR decline_code IS NOT NULL)
);

-- The reconciler's work queue.
CREATE INDEX IF NOT EXISTS payments_unresolved_idx
    ON payments.payments (state, updated_at) WHERE state IN ('pending','unknown');
