-- Freedom 0013: net settlement and listing admission.
--
-- Settlement positions are what a clearing batch actually produces for the
-- outside world: one number per participant, which either moves over NIBSS or
-- does not. And admission is the workflow the listing rulebook describes.

BEGIN;

-- One participant's net position for a business date.
--
-- Netting is the point: a member that acquired ₦40m and issued ₦38m settles
-- ₦2m, not two gross flows. The whole reason a scheme exists between two banks
-- is that neither wants to send the other every transaction separately.
CREATE TABLE settlement_positions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_id        uuid NOT NULL REFERENCES clearing_batches(id),
    participant_id  uuid NOT NULL REFERENCES participants(id),

    -- Signed: positive means the network owes them, negative means they owe it.
    net_kobo        bigint NOT NULL,
    -- The gross components, kept because a participant reconciling a net figure
    -- needs to see what went into it.
    acquired_kobo   bigint NOT NULL DEFAULT 0,
    issued_kobo     bigint NOT NULL DEFAULT 0,
    interchange_kobo bigint NOT NULL DEFAULT 0,
    fees_kobo       bigint NOT NULL DEFAULT 0,

    state           text NOT NULL DEFAULT 'computed'
                    CHECK (state IN ('computed','capped','instructed','settled','failed')),
    -- Set when a position breached the participant's net debit cap. The scheme
    -- refusing to settle is the control; recording why is what makes it
    -- defensible afterwards.
    cap_breach_kobo bigint,
    instruction_id  uuid REFERENCES settlement_instructions(id),
    computed_at     timestamptz NOT NULL DEFAULT now(),
    settled_at      timestamptz,

    UNIQUE (batch_id, participant_id)
);

CREATE INDEX settlement_positions_open ON settlement_positions (state)
    WHERE state IN ('computed','capped','instructed');

-- A listing application, worked against the rulebook.
--
-- The criteria live in docs/LISTING-RULES.md; this is the record of a specific
-- company being measured against them, with each finding recorded rather than
-- summarised into a yes or a no.
CREATE TABLE listing_applications (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id      uuid NOT NULL REFERENCES companies(id),
    merchant_id     uuid REFERENCES merchants(id),
    proposed_symbol text NOT NULL,

    sponsor_member  uuid REFERENCES members(id),
    state           text NOT NULL DEFAULT 'submitted'
                    CHECK (state IN ('submitted','in_review','approved','rejected','withdrawn','listed')),

    findings        jsonb NOT NULL DEFAULT '[]',
    decided_by      text,
    decided_at      timestamptz,
    note            text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (proposed_symbol)
);

COMMIT;
