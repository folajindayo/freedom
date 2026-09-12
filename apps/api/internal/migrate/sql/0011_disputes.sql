-- Freedom 0011: card disputes and the buyback unwind.
--
-- The chargeback lock on buyback shares has existed since the first commit, and
-- the thing it exists to make possible has not. This is that thing.

BEGIN;

-- A cardholder's dispute, and the clocks that run against it.
--
-- Every stage has a deadline and a DEFAULT OUTCOME when that deadline passes.
-- A dispute clock with no default is a dispute that sits forever: the side that
-- benefits from silence simply stays silent, and on a card network that side is
-- whoever is holding the money.
CREATE TABLE disputes (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    presentment_id  uuid NOT NULL REFERENCES presentments(id),
    cardholder_id   uuid NOT NULL REFERENCES cardholders(id),
    merchant_id     uuid NOT NULL REFERENCES merchants(id),

    -- ISO 8583 / scheme-style reason groups. The group decides who bears the
    -- burden of proof and therefore which way the default outcome falls.
    reason_code     text NOT NULL CHECK (reason_code IN
                    ('fraud','processing_error','authorisation','consumer_dispute')),
    reason_detail   text NOT NULL,
    amount_kobo     bigint NOT NULL CHECK (amount_kobo > 0),

    state           text NOT NULL DEFAULT 'initiated' CHECK (state IN
                    ('initiated','evidence_requested','represented','pre_arbitration',
                     'arbitration','won_cardholder','won_merchant','withdrawn')),

    -- Each clock is the deadline for whoever must act next.
    respond_by      date NOT NULL,
    filed_on        date NOT NULL,

    provisional_credit boolean NOT NULL DEFAULT false,
    provisional_tx_id  uuid REFERENCES ledger_tx(id),
    resolution_tx_id   uuid REFERENCES ledger_tx(id),

    resolved_at     timestamptz,
    resolved_by     text,
    outcome_note    text,
    created_at      timestamptz NOT NULL DEFAULT now(),

    -- One live dispute per presentment. A second would let the same
    -- transaction be charged back twice.
    UNIQUE (presentment_id)
);

CREATE INDEX disputes_due ON disputes (respond_by)
    WHERE state NOT IN ('won_cardholder','won_merchant','withdrawn');

-- Every stage transition, so the history of a dispute is reconstructable when
-- somebody asks why it went the way it did.
CREATE TABLE dispute_events (
    id            bigserial PRIMARY KEY,
    dispute_id    uuid NOT NULL REFERENCES disputes(id),
    from_state    text NOT NULL,
    to_state      text NOT NULL,
    -- true when the clock decided rather than a person. These are the rows an
    -- auditor counts: a network where most disputes resolve by default is a
    -- network that is not working them.
    by_default    boolean NOT NULL DEFAULT false,
    actor         text,
    note          text,
    at            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX dispute_events_dispute ON dispute_events (dispute_id, id);

-- What the buyback gave, and what came back.
--
-- Recorded separately from the intent because an unwind is its own event with
-- its own price: the shares are clawed back at today's value, not at what they
-- cost, and the difference has to land somewhere named.
CREATE TABLE buyback_unwinds (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    intent_id       uuid NOT NULL REFERENCES buyback_intents(id),
    dispute_id      uuid NOT NULL REFERENCES disputes(id),
    units_clawed    bigint NOT NULL DEFAULT 0 CHECK (units_clawed >= 0),
    units_short     bigint NOT NULL DEFAULT 0 CHECK (units_short >= 0),
    cash_recovered_kobo bigint NOT NULL DEFAULT 0,
    loss_kobo       bigint NOT NULL DEFAULT 0,
    method          text NOT NULL CHECK (method IN ('shares','cash','mixed','written_off')),
    ledger_tx_id    uuid REFERENCES ledger_tx(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (intent_id)
);

-- Where an unwind's shortfall lands. Without a named home for it, a loss
-- becomes an unbalanced transaction and the whole batch fails.
ALTER TYPE account_kind ADD VALUE IF NOT EXISTS 'buyback_loss_reserve';

COMMIT;
