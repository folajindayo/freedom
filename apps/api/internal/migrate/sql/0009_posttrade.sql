-- Freedom 0009: post-trade operations.
--
-- Reconciliation, the cash rail, statements and off-market transfers. None of
-- this is glamorous and all of it is what an auditor actually looks at.

BEGIN;

-- Reconciliation breaks.
--
-- The system is correct today because tests say so, not because anything would
-- notice if it stopped being. This is the thing that notices. Breaks are
-- recorded in BOTH directions — present here and missing there, and the
-- reverse — because a one-directional check finds only half of them and is
-- therefore worse than none, since it looks like coverage.
CREATE TABLE recon_runs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_date date NOT NULL,
    kind          text NOT NULL CHECK (kind IN
                  ('ledger_vs_lots','fills_vs_ledger','reserves','treasury','cash_rail')),
    checked       int NOT NULL DEFAULT 0,
    breaks        int NOT NULL DEFAULT 0,
    ran_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_date, kind, ran_at)
);

CREATE TABLE recon_breaks (
    id            bigserial PRIMARY KEY,
    run_id        uuid NOT NULL REFERENCES recon_runs(id),
    kind          text NOT NULL,
    subject       text NOT NULL,          -- account, instrument, or fill id
    expected      bigint,
    actual        bigint,
    detail        jsonb NOT NULL DEFAULT '{}',
    state         text NOT NULL DEFAULT 'open'
                  CHECK (state IN ('open','investigating','resolved','written_off')),
    resolved_by   text,
    resolved_at   timestamptz,
    note          text
);

CREATE INDEX recon_breaks_open ON recon_breaks (kind, id) WHERE state = 'open';

-- Off-market transfers: inheritance, court orders, gifts, corrections.
--
-- Every one of these moves shares without a trade, so every one needs a reason
-- and an approver on the record. This is the table a regulator reads first.
CREATE TABLE share_transfers (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    instrument_id text NOT NULL REFERENCES instruments(id),
    from_account  uuid NOT NULL REFERENCES accounts(id),
    to_account    uuid NOT NULL REFERENCES accounts(id),
    units         bigint NOT NULL CHECK (units > 0),
    reason        text NOT NULL CHECK (reason IN
                  ('inheritance','court_order','gift','correction','account_closure')),
    reference     text NOT NULL,          -- probate number, court reference, case id
    requested_by  text NOT NULL,
    approved_by   text,
    state         text NOT NULL DEFAULT 'requested'
                  CHECK (state IN ('requested','approved','executed','rejected')),
    ledger_tx_id  uuid REFERENCES ledger_tx(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    executed_at   timestamptz,
    CHECK (from_account <> to_account)
);

-- Statements. Generated and kept, not rendered on demand: a holder disputes
-- what they were SENT, so what they were sent has to still exist.
CREATE TABLE holding_statements (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id    uuid NOT NULL REFERENCES accounts(id),
    period_start  date NOT NULL,
    period_end    date NOT NULL,
    content       jsonb NOT NULL,
    generated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, period_end)
);

-- The cash rail: the one genuinely non-atomic boundary in the system.
--
-- Everything else settles inside one database transaction because Freedom
-- controls both legs. This does not: once an instruction leaves for NIBSS the
-- outcome is somebody else's to report, and it may arrive late, twice, or not
-- at all.
CREATE TABLE settlement_instructions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    direction     text NOT NULL CHECK (direction IN ('in','out')),
    account_id    uuid NOT NULL REFERENCES accounts(id),
    amount_kobo   bigint NOT NULL CHECK (amount_kobo > 0),
    rail          text NOT NULL,
    external_ref  text,
    state         text NOT NULL DEFAULT 'pending'
                  CHECK (state IN ('pending','sent','settled','failed','returned')),
    ledger_tx_id  uuid REFERENCES ledger_tx(id),
    -- An instruction may be submitted more than once and must only move money
    -- once; a confirmation may arrive twice and must only be believed once.
    idempotency_key text NOT NULL UNIQUE,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX settlement_instructions_open ON settlement_instructions (state, created_at)
    WHERE state IN ('pending','sent');

COMMIT;
