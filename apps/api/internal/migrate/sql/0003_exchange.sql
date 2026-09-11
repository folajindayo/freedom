-- Ọja 0003: the private exchange, and the buyback that feeds it.
--
-- Nigerian SME retailers are not listed anywhere, so there is no market in
-- which to buy the shares the card promises. This is that market.

BEGIN;

-- ---------------------------------------------------------------- issuers of equity

CREATE TABLE companies (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    legal_name    text NOT NULL,
    rc_number     text UNIQUE,                        -- CAC registration
    created_at    timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE merchants
    ADD CONSTRAINT merchants_company_fk FOREIGN KEY (company_id) REFERENCES companies(id);

CREATE TABLE instruments (
    -- The asset id, so an instrument and its ledger asset cannot drift apart.
    id            text PRIMARY KEY REFERENCES assets(id),
    symbol        text NOT NULL UNIQUE CHECK (symbol ~ '^[A-Z][A-Z0-9]{2,11}$'),
    company_id    uuid NOT NULL REFERENCES companies(id),

    -- The hard ceiling on issuance. Every tap sells treasury shares, which is
    -- continuous issuance and therefore continuous dilution of existing
    -- holders; without a ceiling the buyback would dilute without limit.
    shares_authorised_units bigint NOT NULL CHECK (shares_authorised_units > 0),

    status        text NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft','listed','halted','suspended','delisted')),

    -- A continuous order book on an illiquid name produces a stale, easily
    -- manipulated print. Symbols start auction-only and earn the book.
    clob_enabled  boolean NOT NULL DEFAULT false,
    clob_enabled_at timestamptz,

    tick_kobo     bigint NOT NULL DEFAULT 1 CHECK (tick_kobo > 0),
    lot_units     bigint NOT NULL DEFAULT 1 CHECK (lot_units > 0),

    listed_at     timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Dilution has to be reconstructable, so issuance is an event log rather than a
-- running total that gets updated.
CREATE TABLE cap_table_events (
    id            bigserial PRIMARY KEY,
    instrument_id text NOT NULL REFERENCES instruments(id),
    kind          text NOT NULL CHECK (kind IN
                  ('authorised','treasury_release','transfer','buyback_unwind','split','dividend')),
    units_delta   bigint NOT NULL,
    ledger_tx_id  uuid REFERENCES ledger_tx(id),
    note          text,
    occurred_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX cap_table_events_instrument ON cap_table_events (instrument_id, id);

-- Policy only. The treasury BALANCE lives in the ledger like every other
-- balance; duplicating it here would create two truths.
CREATE TABLE treasury_pools (
    instrument_id       text PRIMARY KEY REFERENCES instruments(id),
    account_id          uuid NOT NULL REFERENCES accounts(id),

    -- The daily release cap is a price-manipulation defence, not a convenience.
    -- It bounds how much scheme money a collusive listing can extract in one
    -- session no matter what it does to its own reference price.
    daily_release_units bigint NOT NULL CHECK (daily_release_units > 0),
    released_units      bigint NOT NULL DEFAULT 0 CHECK (released_units >= 0),
    release_date        date NOT NULL
);

-- ---------------------------------------------------------------- auctions

CREATE TABLE auctions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    instrument_id text NOT NULL REFERENCES instruments(id),
    session_date  date NOT NULL,
    state         text NOT NULL DEFAULT 'scheduled'
                  CHECK (state IN ('scheduled','accepting','frozen','uncrossed','published','cancelled')),

    opens_at      timestamptz NOT NULL,
    freezes_at    timestamptz NOT NULL,

    prev_reference_kobo bigint,
    clearing_price_kobo bigint,
    matched_units       bigint,
    imbalance_units     bigint,
    imbalance_side      text CHECK (imbalance_side IN ('buy','sell')),
    uncrossed_at        timestamptz,

    UNIQUE (instrument_id, session_date)
);

CREATE TABLE orders (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    instrument_id text NOT NULL REFERENCES instruments(id),
    account_id    uuid NOT NULL REFERENCES accounts(id),

    side          text NOT NULL CHECK (side IN ('buy','sell')),
    type          text NOT NULL CHECK (type IN ('limit','market')),
    limit_kobo    bigint CHECK (limit_kobo IS NULL OR limit_kobo > 0),
    qty_units     bigint NOT NULL CHECK (qty_units > 0),
    filled_units  bigint NOT NULL DEFAULT 0 CHECK (filled_units >= 0),

    venue         text NOT NULL CHECK (venue IN ('auction','clob')),
    auction_id    uuid REFERENCES auctions(id),

    -- Time priority. A bigserial rather than a timestamp because two orders can
    -- share a microsecond and priority must still be total.
    seq           bigserial NOT NULL,

    state         text NOT NULL DEFAULT 'open'
                  CHECK (state IN ('open','filled','partial','cancelled','rejected')),
    created_at    timestamptz NOT NULL DEFAULT now(),

    CHECK (filled_units <= qty_units),
    CHECK (type = 'market' OR limit_kobo IS NOT NULL)
);

CREATE INDEX orders_book ON orders (instrument_id, venue, side, limit_kobo, seq)
    WHERE state IN ('open','partial');

-- Append-only market data, after Caelum's price_observations. Corrections are
-- new rows, never updates, so a price that moved can always be explained.
CREATE TABLE price_observations (
    id            bigserial PRIMARY KEY,
    instrument_id text NOT NULL REFERENCES instruments(id),
    obs_date      date NOT NULL,
    source        text NOT NULL CHECK (source IN ('auction','clob','carry_forward','manual')),
    price_kobo    bigint NOT NULL CHECK (price_kobo > 0),
    volume_units  bigint NOT NULL DEFAULT 0,
    content_hash  text NOT NULL,
    observed_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (instrument_id, obs_date, source, content_hash)
);

CREATE VIEW price_observations_current AS
    SELECT DISTINCT ON (instrument_id)
           instrument_id, obs_date, source, price_kobo, volume_units, observed_at
      FROM price_observations
     ORDER BY instrument_id, obs_date DESC, id DESC;

CREATE TABLE data_quality_incidents (
    id            bigserial PRIMARY KEY,
    instrument_id text REFERENCES instruments(id),
    kind          text NOT NULL,                      -- band_breach, wash_suspected, stale_reference
    detail        jsonb NOT NULL,
    raised_at     timestamptz NOT NULL DEFAULT now(),
    resolved_at   timestamptz
);

-- ---------------------------------------------------------------- holdings

-- Lots, not a running total. Cost basis cannot be reconstructed after the fact
-- and capital gains reporting will need it; transferable_from is the chargeback
-- lock that makes an unwind possible at all.
CREATE TABLE holding_lots (
    id            bigserial PRIMARY KEY,
    account_id    uuid NOT NULL REFERENCES accounts(id),
    instrument_id text NOT NULL REFERENCES instruments(id),
    units         bigint NOT NULL CHECK (units > 0),
    units_open    bigint NOT NULL CHECK (units_open >= 0),
    cost_kobo     bigint NOT NULL CHECK (cost_kobo >= 0),

    -- Shares bought by a buyback cannot be sold until the chargeback window on
    -- the transaction that funded them has closed. Otherwise a fraudster taps,
    -- receives equity, sells it, and charges the tap back.
    transferable_from date NOT NULL,

    acquired_at   timestamptz NOT NULL DEFAULT now(),
    ledger_tx_id  uuid REFERENCES ledger_tx(id),
    CHECK (units_open <= units)
);

CREATE INDEX holding_lots_account ON holding_lots (account_id, instrument_id, transferable_from);

-- ---------------------------------------------------------------- buyback

CREATE TABLE buyback_batches (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    instrument_id text NOT NULL REFERENCES instruments(id),
    session_date  date NOT NULL,
    funding_kobo  bigint NOT NULL DEFAULT 0,
    units_bought  bigint NOT NULL DEFAULT 0,
    price_kobo    bigint,
    source        text NOT NULL DEFAULT 'treasury' CHECK (source IN ('treasury','clob')),
    state         text NOT NULL DEFAULT 'open'
                  CHECK (state IN ('open','executed','allocated','failed')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (instrument_id, session_date)
);

CREATE TABLE buyback_intents (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    presentment_id uuid NOT NULL REFERENCES presentments(id),
    clearing_batch_id uuid NOT NULL REFERENCES clearing_batches(id),
    merchant_id   uuid NOT NULL REFERENCES merchants(id),
    cardholder_id uuid NOT NULL REFERENCES cardholders(id),

    -- NULL when the merchant is not listed: the funding accrues to escrow and
    -- the merchant portal shows them how much equity their customers are
    -- already waiting to own. That is the listings pipeline, not an error path.
    instrument_id text REFERENCES instruments(id),

    funding_kobo  bigint NOT NULL CHECK (funding_kobo > 0),
    funding_breakdown jsonb NOT NULL,     -- {"scheme":500,"merchant":10000,"promo":0}

    state         text NOT NULL DEFAULT 'pending'
                  CHECK (state IN ('pending','batched','allocated','escrowed','deferred_dust','unwound')),
    batch_id      uuid REFERENCES buyback_batches(id),

    allocated_units bigint,
    price_kobo      bigint,
    residual_kobo   bigint NOT NULL DEFAULT 0 CHECK (residual_kobo >= 0),

    created_at    timestamptz NOT NULL DEFAULT now(),

    -- Exactly-once, in the schema rather than in the batch job's memory.
    -- Clearing will die halfway at some point and be re-run; this is what makes
    -- that safe.
    UNIQUE (presentment_id)
);

CREATE INDEX buyback_intents_pending ON buyback_intents (instrument_id, created_at)
    WHERE state = 'pending';

COMMIT;
