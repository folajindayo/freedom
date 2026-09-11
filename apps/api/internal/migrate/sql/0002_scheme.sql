-- Freedom 0002: the network.
--
-- Participants, cards, merchants, terminals, and the authorisation and clearing
-- records that move between them.

BEGIN;

-- ---------------------------------------------------------------- participants

-- A member of the network. One legal entity may hold several roles; Freedom itself
-- is a participant holding all of them, which is what makes the split-out to
-- real member banks a data change rather than a rewrite.
CREATE TABLE participants (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code          text NOT NULL UNIQUE,              -- short scheme member code
    legal_name    text NOT NULL,
    roles         text[] NOT NULL CHECK (roles <@ ARRAY['issuer','acquirer','scheme']),
    status        text NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','active','suspended','terminated')),

    -- The most a participant may owe the network before settlement. Without it,
    -- one member's failure is every member's loss: the scheme must be able to
    -- refuse to settle a position it has no collateral for.
    net_debit_cap_kobo bigint NOT NULL DEFAULT 0 CHECK (net_debit_cap_kobo >= 0),

    created_at    timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------- BIN routing

-- PANs are up to 19 digits. 19 nines is 9.99e18, which is larger than a signed
-- bigint can hold, so they are text throughout — zero-padded so that ordinary
-- string comparison is numeric comparison.
CREATE TABLE bin_ranges (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    low           text NOT NULL CHECK (low ~ '^[0-9]{19}$'),
    high          text NOT NULL CHECK (high ~ '^[0-9]{19}$'),
    issuer_id     uuid NOT NULL REFERENCES participants(id),
    product_code  text NOT NULL,
    effective     daterange NOT NULL,
    CHECK (low <= high),

    -- Overlapping BIN ranges are a routine production incident on real
    -- networks: two issuers claim one prefix and traffic routes by whichever
    -- row the query happened to return. Made unrepresentable.
    -- numeric rather than text: a 19-digit PAN overflows bigint but numeric
    -- holds it exactly, and numeric ordering is the ordering we actually mean.
    EXCLUDE USING gist (
        numrange(low::numeric, high::numeric, '[]') WITH &&,
        effective WITH &&
    )
);

-- ---------------------------------------------------------------- cardholders

CREATE TABLE cardholders (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    phone       text NOT NULL UNIQUE,
    display_name text NOT NULL,

    -- CBN tiered KYC. The tier caps what the account may do, and equity
    -- allocation is gated on it below.
    kyc_tier    smallint NOT NULL DEFAULT 0 CHECK (kyc_tier BETWEEN 0 AND 3),
    bvn_verified_at timestamptz,

    -- Accepting the risk disclosure is a precondition of owning shares, not a
    -- checkbox on a marketing page. Enforced by equity_allocation_permitted().
    disclosure_accepted_at timestamptz,
    disclosure_version     text,

    status      text NOT NULL DEFAULT 'active'
                CHECK (status IN ('active','frozen','closed')),
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- A cardholder may only be allocated equity once they are verified and have
-- accepted the disclosure. Retrofitting this onto a live allocation engine
-- means reconciling shares already granted to people who were never eligible,
-- so it exists before the first share is issued.
CREATE FUNCTION equity_allocation_permitted(holder uuid) RETURNS boolean AS $fn$
    SELECT c.kyc_tier >= 1
       AND c.bvn_verified_at IS NOT NULL
       AND c.disclosure_accepted_at IS NOT NULL
       AND c.status = 'active'
      FROM cardholders c WHERE c.id = holder;
$fn$ LANGUAGE sql STABLE;

-- ---------------------------------------------------------------- cards

-- PAN handling.
--
-- Freedom is the issuer, so it holds PANs and is squarely in PCI DSS scope. The
-- full PAN lives only in card_pan_vault, encrypted under a key this database
-- never sees; everything else in the schema joins on card_id and displays
-- pan_last4. pan_token is a keyed HMAC under a pepper held outside the
-- database, so it supports lookup without being reversible — PCI DSS 4.0.1
-- requires a keyed hash rather than a bare one precisely so that a stolen
-- table cannot be brute-forced against the small PAN space.
CREATE TABLE cards (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cardholder_id uuid NOT NULL REFERENCES cardholders(id),
    issuer_id     uuid NOT NULL REFERENCES participants(id),
    product_code  text NOT NULL,

    pan_token     bytea NOT NULL UNIQUE,             -- HMAC(pepper, PAN)
    pan_last4     char(4) NOT NULL CHECK (pan_last4 ~ '^[0-9]{4}$'),
    pan_bin       char(8) NOT NULL CHECK (pan_bin ~ '^[0-9]{8}$'),
    expires_on    date NOT NULL,

    status        text NOT NULL DEFAULT 'active'
                  CHECK (status IN ('issued','active','frozen','expired','cancelled')),

    -- Pilot rails. These are not advisory: the NTAG215 credential is not
    -- authentication, and these caps are the fraud-loss budget.
    per_txn_cap_kobo  bigint NOT NULL DEFAULT 2000000 CHECK (per_txn_cap_kobo > 0),
    daily_cap_kobo    bigint NOT NULL DEFAULT 10000000 CHECK (daily_cap_kobo > 0),
    online_only       boolean NOT NULL DEFAULT true,

    frozen_reason text,
    frozen_at     timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX cards_holder ON cards (cardholder_id);

-- The credential bound to a physical tag. One card may be re-credentialed (a
-- replacement tag) without becoming a different card.
CREATE TABLE card_credentials (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    card_id     uuid NOT NULL REFERENCES cards(id),
    tech        text NOT NULL CHECK (tech IN ('ntag215','ntag424')),
    tag_uid     bytea NOT NULL UNIQUE,

    -- Highest counter accepted. Advanced only by a conditional UPDATE, never by
    -- read-then-write: two terminals tapping at once would otherwise both read
    -- the old value and both accept the same counter.
    last_counter bigint NOT NULL DEFAULT 0 CHECK (last_counter >= 0),

    -- NTAG 424: the key reference. Key material itself never enters this
    -- database — the store derives it from a master held in an HSM.
    key_ref     text,

    -- NTAG 215: the two live rolling tokens.
    token_current bytea,
    token_prev    bytea,
    token_seq     bigint NOT NULL DEFAULT 0,
    token_rotated_at timestamptz,

    status      text NOT NULL DEFAULT 'active'
                CHECK (status IN ('active','frozen','replaced')),
    clone_suspected_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT credential_matches_tech CHECK (
        (tech = 'ntag424' AND key_ref IS NOT NULL)
     OR (tech = 'ntag215' AND token_current IS NOT NULL))
);

CREATE INDEX card_credentials_card ON card_credentials (card_id);

-- ---------------------------------------------------------------- acceptance

CREATE TABLE merchants (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    acquirer_id   uuid NOT NULL REFERENCES participants(id),
    legal_name    text NOT NULL,
    trading_name  text NOT NULL,
    mcc           char(4) NOT NULL CHECK (mcc ~ '^[0-9]{4}$'),

    kyb_status    text NOT NULL DEFAULT 'pending'
                  CHECK (kyb_status IN ('pending','verified','rejected')),

    -- The merchant's opt-in equity rebate, in basis points of each ticket. This
    -- is the lever that makes the buyback material; see internal/fee.
    cofund_bps    int NOT NULL DEFAULT 0 CHECK (cofund_bps BETWEEN 0 AND 300),

    -- Set once the merchant lists. Until then buybacks against this merchant
    -- accrue into escrow rather than failing.
    company_id    uuid,

    status        text NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active','suspended','terminated')),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX merchants_acquirer ON merchants (acquirer_id);

-- A merchant's phone acting as a terminal.
CREATE TABLE terminals (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id   uuid NOT NULL REFERENCES merchants(id),
    label         text NOT NULL,

    -- Device attestation. Play Integrity has outages, and an acceptance fleet
    -- that fails closed on Google's availability is an outage of the whole
    -- network, so the verdict is recorded with its age and the risk engine
    -- decides — rather than the auth path hard-failing on a missing token.
    attestation_verdict text NOT NULL DEFAULT 'unverified'
                  CHECK (attestation_verdict IN ('unverified','trusted','degraded','failed')),
    attested_at   timestamptz,

    status        text NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active','suspended','retired')),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX terminals_merchant ON terminals (merchant_id);

-- ---------------------------------------------------------------- fee schedules

CREATE TABLE fee_schedules (
    version        int PRIMARY KEY,
    effective_from date NOT NULL,
    effective_to   date,
    definition     jsonb NOT NULL,
    published_at   timestamptz NOT NULL DEFAULT now(),
    CHECK (effective_to IS NULL OR effective_to > effective_from)
);

-- ---------------------------------------------------------------- authorisation

CREATE TABLE authorizations (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    card_id       uuid NOT NULL REFERENCES cards(id),
    credential_id uuid NOT NULL REFERENCES card_credentials(id),
    terminal_id   uuid NOT NULL REFERENCES terminals(id),
    merchant_id   uuid NOT NULL REFERENCES merchants(id),
    acquirer_id   uuid NOT NULL REFERENCES participants(id),
    issuer_id     uuid NOT NULL REFERENCES participants(id),

    -- ISO 8583 shaped, because one day a real terminal or member bank will
    -- integrate and these are the fields they will send.
    stan          char(6) NOT NULL CHECK (stan ~ '^[0-9]{6}$'),
    rrn           char(12) NOT NULL,

    requested_kobo bigint NOT NULL CHECK (requested_kobo > 0),
    approved_kobo  bigint NOT NULL DEFAULT 0 CHECK (approved_kobo >= 0),
    -- Decremented by captures and reversals. When it reaches zero the
    -- authorisation is closed and its hold is released.
    outstanding_kobo bigint NOT NULL DEFAULT 0 CHECK (outstanding_kobo >= 0),

    result        text NOT NULL CHECK (result IN ('approved','partial','declined','reversed')),
    decline_code  text,

    -- Pinned at authorisation. Clearing prices with THIS version, never with
    -- whichever is live when the batch runs.
    fee_schedule_version int NOT NULL REFERENCES fee_schedules(version),

    stand_in      boolean NOT NULL DEFAULT false,
    offline       boolean NOT NULL DEFAULT false,
    counter       bigint NOT NULL,

    hold_tx_id    uuid REFERENCES ledger_tx(id),
    business_date date NOT NULL,
    expires_at    timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),

    -- The switch's replay guard. A terminal retrying a timed-out tap sends the
    -- same STAN, and it must not become a second authorisation.
    UNIQUE (acquirer_id, business_date, terminal_id, stan)
);

CREATE INDEX authorizations_card ON authorizations (card_id, created_at DESC);
CREATE INDEX authorizations_open ON authorizations (expires_at)
    WHERE outstanding_kobo > 0;

-- What actually gets cleared. A presentment may arrive with no authorisation
-- (a forced or offline transaction), and reversals routinely arrive before the
-- authorisation they reverse.
CREATE TABLE presentments (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    authorization_id uuid REFERENCES authorizations(id),
    merchant_id   uuid NOT NULL REFERENCES merchants(id),
    card_id       uuid NOT NULL REFERENCES cards(id),

    kind          text NOT NULL CHECK (kind IN
                  ('first','partial_reversal','full_reversal','refund','chargeback','representment')),
    -- Signed: refunds and reversals are negative.
    amount_kobo   bigint NOT NULL CHECK (amount_kobo <> 0),

    arn           text NOT NULL UNIQUE,              -- acquirer reference number
    fee_schedule_version int NOT NULL REFERENCES fee_schedules(version),

    clearing_batch_id uuid,
    business_date timestamptz,
    received_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX presentments_unbatched ON presentments (received_at)
    WHERE clearing_batch_id IS NULL;

CREATE TABLE clearing_batches (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_date date NOT NULL,
    cycle         smallint NOT NULL DEFAULT 1,
    cutoff_at     timestamptz NOT NULL,
    state         text NOT NULL DEFAULT 'open'
                  CHECK (state IN ('open','cutoff','computing','final','settled')),
    finalised_at  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_date, cycle)
);

ALTER TABLE presentments
    ADD CONSTRAINT presentments_batch_fk
    FOREIGN KEY (clearing_batch_id) REFERENCES clearing_batches(id);

COMMIT;
