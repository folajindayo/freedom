-- Freedom 0010: the institution.
--
-- An exchange is a rulebook with a matching engine attached, and this is the
-- half that is not the matching engine: disclosure, an index, investor
-- protection, complaints, and the clock everything else claims to be timed by.

BEGIN;

-- Company announcements.
--
-- The offence is SELECTIVE disclosure, not disclosure — so the model is
-- publication to everyone at one instant, with the instant recorded. A
-- disclosure that reached one holder before another is the thing this table
-- exists to make provable, in either direction.
CREATE TABLE disclosures (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    instrument_id text NOT NULL REFERENCES instruments(id),
    kind          text NOT NULL CHECK (kind IN
                  ('results','corporate_action','material_event','director_dealing',
                   'shareholding','listing_particulars','suspension')),
    headline      text NOT NULL,
    body          text NOT NULL,
    submitted_by  text NOT NULL,
    submitted_at  timestamptz NOT NULL DEFAULT now(),
    -- NULL until published. Between submission and publication the market is
    -- normally halted, because the issuer knows something the market does not.
    published_at  timestamptz,
    halt_id       uuid REFERENCES trading_halts(id),
    UNIQUE (instrument_id, kind, submitted_at)
);

CREATE INDEX disclosures_unpublished ON disclosures (instrument_id)
    WHERE published_at IS NULL;

-- The index.
--
-- A benchmark for the venue, and the natural destination for a buyback against
-- a merchant who has not listed: the cardholder gets a diversified claim on the
-- market rather than an IOU against one shop.
CREATE TABLE indices (
    id            text PRIMARY KEY,
    name          text NOT NULL,
    base_date     date NOT NULL,
    base_value    bigint NOT NULL CHECK (base_value > 0),  -- in basis points, so 1000000 = 100.0000
    method        text NOT NULL DEFAULT 'market_cap_weighted'
                  CHECK (method IN ('market_cap_weighted','equal_weighted')),
    -- No constituent may exceed this share of the index. Without a cap, one
    -- large listing IS the index and the benchmark stops meaning anything.
    max_weight_bps int NOT NULL DEFAULT 2000,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE index_constituents (
    index_id      text NOT NULL REFERENCES indices(id),
    instrument_id text NOT NULL REFERENCES instruments(id),
    added_on      date NOT NULL,
    removed_on    date,
    PRIMARY KEY (index_id, instrument_id, added_on)
);

CREATE TABLE index_values (
    index_id      text NOT NULL REFERENCES indices(id),
    obs_date      date NOT NULL,
    value_bps     bigint NOT NULL CHECK (value_bps > 0),
    constituents  int NOT NULL,
    computed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (index_id, obs_date)
);

-- Investor protection.
--
-- Statutory for a Nigerian exchange. The fund is a ledger account like anything
-- else; this records the claims against it and who decided them.
CREATE TABLE protection_claims (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    claimant      uuid NOT NULL REFERENCES cardholders(id),
    member_id     uuid REFERENCES members(id),
    amount_kobo   bigint NOT NULL CHECK (amount_kobo > 0),
    awarded_kobo  bigint CHECK (awarded_kobo IS NULL OR awarded_kobo >= 0),
    grounds       text NOT NULL CHECK (grounds IN
                  ('member_default','unauthorised_trading','failure_to_deliver','fraud')),
    narrative     text NOT NULL,
    state         text NOT NULL DEFAULT 'submitted'
                  CHECK (state IN ('submitted','investigating','upheld','rejected','paid')),
    decided_by    text,
    decided_at    timestamptz,
    ledger_tx_id  uuid REFERENCES ledger_tx(id),
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Complaints and their escalation path.
--
-- An investor who cannot get an answer must have somewhere to go that is not
-- the person who gave them the answer. The deadline is what makes it real:
-- a complaint with no clock is a complaint nobody has to answer.
CREATE TABLE complaints (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    complainant   uuid REFERENCES cardholders(id),
    against       text NOT NULL CHECK (against IN ('member','issuer','exchange')),
    member_id     uuid REFERENCES members(id),
    instrument_id text REFERENCES instruments(id),
    subject       text NOT NULL,
    narrative     text NOT NULL,
    state         text NOT NULL DEFAULT 'open'
                  CHECK (state IN ('open','with_member','with_exchange','escalated_sec','resolved','withdrawn')),
    respond_by    date NOT NULL,
    resolved_at   timestamptz,
    resolution    text,
    handled_by    text,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX complaints_overdue ON complaints (respond_by)
    WHERE state NOT IN ('resolved','withdrawn');

-- Clock integrity.
--
-- Every audit-trail claim the exchange makes depends on its clock. A venue that
-- cannot show its time was traceable cannot defend a sequence of events, and
-- sequencing is the whole case in a manipulation investigation.
CREATE TABLE clock_checks (
    id            bigserial PRIMARY KEY,
    checked_at    timestamptz NOT NULL DEFAULT now(),
    source        text NOT NULL,
    offset_ms     bigint NOT NULL,
    within_bounds boolean NOT NULL
);

COMMIT;
