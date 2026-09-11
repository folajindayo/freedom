-- Freedom 0006: the exchange proper.
--
-- Order audit, fills, reservations, lot disposals, the trading calendar, halts,
-- related parties and surveillance. Plus corrections to tables laid down in
-- 0002 and 0003 that do not survive contact with a real matching engine.

BEGIN;

-- ---------------------------------------------------------------- members

-- Broker firms. Freedom Securities is the only one at launch, but orders carry
-- a member from the first row: retrofitting membership onto live trades means
-- touching every order, fill and settlement record, and backfilling an
-- isolation boundary onto data that was never separated.
CREATE TABLE members (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code          text NOT NULL UNIQUE,
    legal_name    text NOT NULL,
    roles         text[] NOT NULL DEFAULT ARRAY['broker']
                  CHECK (roles <@ ARRAY['broker','market_maker','issuer_agent']),
    status        text NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','active','suspended','terminated')),
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- A member's client. For Freedom Securities this is a cardholder.
CREATE TABLE client_accounts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    member_id     uuid NOT NULL REFERENCES members(id),
    cardholder_id uuid REFERENCES cardholders(id),
    company_id    uuid REFERENCES companies(id),
    label         text NOT NULL,
    status        text NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active','restricted','closed')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- Exactly one beneficial owner.
    CONSTRAINT one_owner CHECK (num_nonnulls(cardholder_id, company_id) = 1)
);

CREATE UNIQUE INDEX client_accounts_cardholder ON client_accounts (cardholder_id)
    WHERE cardholder_id IS NOT NULL;

-- ---------------------------------------------------------------- instrument policy

ALTER TABLE instruments
    -- Mandatory from listing. It is the tie-break of last resort, the centre of
    -- the price band, and the carry-forward value; a listing without one has no
    -- band, no tie-break and no buyback price. Making it NOT NULL removes a
    -- branch from the uncross ladder rather than leaving it to be handled.
    ADD COLUMN reference_price_kobo bigint NOT NULL DEFAULT 0
        CHECK (reference_price_kobo >= 0),
    ADD COLUMN static_band_bps  int NOT NULL DEFAULT 2000,   -- ±20% on a thin SME name
    ADD COLUMN dynamic_band_bps int NOT NULL DEFAULT 1000,
    ADD COLUMN min_order_notional_kobo bigint NOT NULL DEFAULT 10000,   -- ₦100
    ADD COLUMN max_order_notional_kobo bigint,
    -- Staleness is a number, not a judgement. Past the maximum the reference is
    -- stale and the buyback escrows rather than buying at a price from weeks ago.
    ADD COLUMN carry_forward_sessions     int NOT NULL DEFAULT 0,
    ADD COLUMN max_carry_forward_sessions int NOT NULL DEFAULT 5,
    -- The minimum traded volume a trailing VWAP must cover before it may cap
    -- anything. A VWAP computed from one dust trade is not a control.
    ADD COLUMN min_vwap_volume_units bigint NOT NULL DEFAULT 100000000, -- 1 share
    ADD COLUMN clob_disabled_at timestamptz,
    ADD COLUMN clob_review_state text NOT NULL DEFAULT 'auction_only'
        CHECK (clob_review_state IN ('auction_only','under_review','continuous','demoted'));

-- ---------------------------------------------------------------- orders

-- Replay determinism. A global bigserial means replaying a session against a
-- fresh database produces different sequence numbers and therefore a different
-- canonical hash, so no session can be audited by replay. Priority must be
-- dense and per-auction, assigned under the instrument's advisory lock.
ALTER TABLE orders DROP COLUMN seq;

ALTER TABLE orders
    ADD COLUMN member_id     uuid REFERENCES members(id),
    ADD COLUMN client_account_id uuid REFERENCES client_accounts(id),
    ADD COLUMN auction_seq   bigint,
    ADD COLUMN tif           text NOT NULL DEFAULT 'day'
                             CHECK (tif IN ('day','gtc','ioc','fok')),
    ADD COLUMN expires_on    date,
    -- A cash-denominated buy: "spend ₦5,000 on MAMAPUT". A market buy cannot be
    -- reserved because it has no bound, so this is what retail gets instead —
    -- and it is the shape the product already speaks, since a ₦5 tap buys 0.125
    -- shares rather than a share count.
    ADD COLUMN notional_kobo bigint CHECK (notional_kobo IS NULL OR notional_kobo > 0),
    ADD COLUMN reserved_kobo  bigint NOT NULL DEFAULT 0 CHECK (reserved_kobo >= 0),
    ADD COLUMN reserved_units bigint NOT NULL DEFAULT 0 CHECK (reserved_units >= 0),
    ADD COLUMN client_order_id text,
    ADD COLUMN replaces_order_id uuid REFERENCES orders(id),
    -- Resolved from related_parties at entry and frozen onto the order, so the
    -- uncross stays a pure function of the book.
    ADD COLUMN owner_key     text,
    ADD COLUMN reject_reason text;

ALTER TABLE orders DROP CONSTRAINT orders_state_check;
ALTER TABLE orders ADD CONSTRAINT orders_state_check CHECK (state IN
    ('open','filled','partial','cancelled','rejected','expired','amended'));

-- Quantity or notional, never both, never neither.
ALTER TABLE orders ALTER COLUMN qty_units DROP NOT NULL;
ALTER TABLE orders ADD CONSTRAINT orders_qty_or_notional CHECK (
    num_nonnulls(qty_units, notional_kobo) = 1);

CREATE UNIQUE INDEX orders_auction_priority ON orders (auction_id, auction_seq)
    WHERE auction_id IS NOT NULL;
CREATE UNIQUE INDEX orders_client_order_id ON orders (member_id, client_order_id)
    WHERE client_order_id IS NOT NULL;

-- Every state change, append-only. This is the replay source of truth: replay
-- folds these into a book and never reads the mutable orders row.
CREATE TABLE order_events (
    id            bigserial PRIMARY KEY,
    instrument_id text NOT NULL REFERENCES instruments(id),
    auction_id    uuid REFERENCES auctions(id),
    order_id      uuid NOT NULL REFERENCES orders(id),
    kind          text NOT NULL CHECK (kind IN
                  ('new','amend','cancel','reject','expire','fill','stp_cancel','halt_cancel')),
    reason        text,
    -- Enough to rebuild the order as it stood at this event.
    payload       jsonb NOT NULL,
    auction_seq   bigint,
    at            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX order_events_auction ON order_events (auction_id, id);
CREATE INDEX order_events_order   ON order_events (order_id, id);

-- ---------------------------------------------------------------- fills

-- Fills, not trades.
--
-- In a call auction there is no buyer-and-seller pair: there is a set of buys
-- totalling Exec and a set of sells totalling Exec, and any pairing between them
-- is a fiction invented afterwards. So a fill belongs to one order, and
-- contra_order_id is NULL for auction fills and set only on the CLOB, where
-- matching genuinely is pairwise.
CREATE TABLE fills (
    id             bigserial PRIMARY KEY,
    instrument_id  text NOT NULL REFERENCES instruments(id),
    venue          text NOT NULL CHECK (venue IN ('auction','clob')),
    auction_id     uuid REFERENCES auctions(id),
    order_id       uuid NOT NULL REFERENCES orders(id),
    contra_order_id uuid REFERENCES orders(id),
    side           text NOT NULL CHECK (side IN ('buy','sell')),
    units          bigint NOT NULL CHECK (units > 0),
    price_kobo     bigint NOT NULL CHECK (price_kobo > 0),
    -- Allocated by largest remainder from one pool consideration, NOT computed
    -- as units × price. Rounding each side independently makes buyers pay more
    -- than sellers receive and the ledger's balance trigger rejects the session.
    consideration_kobo bigint NOT NULL CHECK (consideration_kobo > 0),
    fee_kobo       bigint NOT NULL DEFAULT 0 CHECK (fee_kobo >= 0),
    ledger_tx_id   uuid NOT NULL REFERENCES ledger_tx(id),
    session_date   date NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (auction_id, order_id)
);

CREATE INDEX fills_instrument ON fills (instrument_id, session_date);

-- Which specific lots a sell consumed. Reserving by lot identity rather than by
-- quantity is what makes the chargeback lock enforceable: a locked lot cannot be
-- reserved because it was never selected.
CREATE TABLE order_lot_reservations (
    order_id      uuid   NOT NULL REFERENCES orders(id),
    lot_id        bigint NOT NULL REFERENCES holding_lots(id),
    units         bigint NOT NULL CHECK (units > 0),
    released      boolean NOT NULL DEFAULT false,
    PRIMARY KEY (order_id, lot_id)
);

CREATE TABLE lot_disposals (
    id            bigserial PRIMARY KEY,
    fill_id       bigint NOT NULL REFERENCES fills(id),
    lot_id        bigint NOT NULL REFERENCES holding_lots(id),
    units         bigint NOT NULL CHECK (units > 0),
    basis_kobo    bigint NOT NULL CHECK (basis_kobo >= 0),
    proceeds_kobo bigint NOT NULL,
    fee_kobo      bigint NOT NULL DEFAULT 0,
    -- Derived, never a ledger entry: the ledger already moved the cash, and a
    -- "realised gains" account would count it twice.
    realised_kobo bigint NOT NULL,
    UNIQUE (fill_id, lot_id)
);

ALTER TABLE holding_lots
    ADD COLUMN units_reserved  bigint NOT NULL DEFAULT 0 CHECK (units_reserved >= 0),
    -- Basis still attributable to the open units. Decremented by largest
    -- remainder so a fully consumed lot lands on exactly zero.
    ADD COLUMN cost_open_kobo  bigint NOT NULL DEFAULT 0 CHECK (cost_open_kobo >= 0),
    ADD CONSTRAINT lots_reserved_within_open CHECK (units_reserved <= units_open);

UPDATE holding_lots SET cost_open_kobo = cost_kobo;

-- ---------------------------------------------------------------- sessions

ALTER TABLE auctions
    ADD COLUMN frozen_at     timestamptz,
    ADD COLUMN publishes_at  timestamptz,
    ADD COLUMN zero_volume   boolean NOT NULL DEFAULT false,
    -- The audit story, as data. book_hash seals what was crossed; result_hash
    -- is what Cross produced; engine_version says under which rules, so a
    -- session settled last year still replays under last year's ladder.
    ADD COLUMN book_hash     text,
    ADD COLUMN result_hash   text,
    ADD COLUMN engine_version int NOT NULL DEFAULT 1,
    ADD COLUMN rule          text;

ALTER TABLE auctions DROP CONSTRAINT auctions_state_check;
ALTER TABLE auctions ADD CONSTRAINT auctions_state_check CHECK (state IN
    ('scheduled','accepting','frozen','uncrossed','published','halted','cancelled'));

-- Transitions enforced by the database. 'published' is terminal: an uncrossed
-- session is never re-uncrossed, and a correction is a new price observation
-- with source 'manual' rather than an edit to a settled session.
CREATE FUNCTION assert_auction_transition() RETURNS trigger AS $fn$
BEGIN
    IF OLD.state = NEW.state THEN RETURN NEW; END IF;
    IF (OLD.state, NEW.state) NOT IN (
         ('scheduled','accepting'), ('scheduled','cancelled'),
         ('accepting','frozen'),    ('accepting','cancelled'), ('accepting','halted'),
         ('frozen','uncrossed'),    ('frozen','cancelled'),    ('frozen','halted'),
         ('uncrossed','published'), ('uncrossed','cancelled'),
         ('halted','cancelled')) THEN
        RAISE EXCEPTION 'auction %: illegal transition % -> %', NEW.id, OLD.state, NEW.state;
    END IF;
    IF NEW.state = 'published' AND NOT NEW.zero_volume AND NEW.clearing_price_kobo IS NULL THEN
        RAISE EXCEPTION 'auction %: published with volume but no clearing price', NEW.id;
    END IF;
    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;

CREATE TRIGGER auctions_transition BEFORE UPDATE ON auctions
    FOR EACH ROW EXECUTE FUNCTION assert_auction_transition();

-- Generated ahead, not computed. Nigerian public holidays are a data problem —
-- Eid dates are announced, not derived — and an algorithm that guesses them
-- closes the market on the wrong day.
CREATE TABLE trading_calendar (
    session_date  date PRIMARY KEY,
    is_trading    boolean NOT NULL,
    opens_at      timestamptz,
    freezes_at    timestamptz,
    note          text
);

CREATE TABLE trading_halts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    instrument_id text NOT NULL REFERENCES instruments(id),
    reason        text NOT NULL CHECK (reason IN
                  ('volatility','news_pending','regulatory','data_quality','issuer_request')),
    halted_at     timestamptz NOT NULL DEFAULT now(),
    released_at   timestamptz,
    raised_by     text NOT NULL,
    released_by   text,
    detail        jsonb NOT NULL DEFAULT '{}'
);

CREATE UNIQUE INDEX trading_halts_one_open ON trading_halts (instrument_id)
    WHERE released_at IS NULL;

-- ---------------------------------------------------------------- integrity

-- Who may not trade a symbol. On an auction-only instrument a related party may
-- not enter an order at all: the issuer's channels are the treasury release and
-- a bid of last resort, both price takers by construction. A designated market
-- maker that is a related party would be the manipulation vector wearing a
-- badge, so the check applies to them too.
CREATE TABLE related_parties (
    instrument_id text NOT NULL REFERENCES instruments(id),
    account_id    uuid NOT NULL REFERENCES accounts(id),
    group_key     text NOT NULL,
    relation      text NOT NULL CHECK (relation IN
                  ('issuer','treasury','director','staff','affiliate','merchant')),
    effective     daterange NOT NULL,
    PRIMARY KEY (instrument_id, account_id, group_key)
);

CREATE INDEX related_parties_account ON related_parties (account_id);

CREATE TABLE surveillance_alerts (
    id            bigserial PRIMARY KEY,
    instrument_id text NOT NULL REFERENCES instruments(id),
    session_date  date,
    detection     text NOT NULL,
    severity      text NOT NULL CHECK (severity IN ('info','warn','block')),
    subject_group text,
    evidence      jsonb NOT NULL,
    raised_at     timestamptz NOT NULL DEFAULT now(),
    reviewed_at   timestamptz,
    outcome       text
);

CREATE INDEX surveillance_open ON surveillance_alerts (instrument_id)
    WHERE reviewed_at IS NULL AND severity = 'block';

-- ---------------------------------------------------------------- treasury

-- Append-only, replacing treasury_pools.released_units/release_date.
--
-- The old shape reset released_units whenever release_date differed from the
-- session being processed, so replaying session D after D+1 had run silently
-- restored the daily cap — the primary anti-extraction control failing open on
-- exactly the path most likely to be re-run.
CREATE TABLE treasury_releases (
    instrument_id text NOT NULL REFERENCES instruments(id),
    session_date  date NOT NULL,
    units         bigint NOT NULL DEFAULT 0 CHECK (units >= 0),
    PRIMARY KEY (instrument_id, session_date)
);

INSERT INTO treasury_releases (instrument_id, session_date, units)
SELECT instrument_id, release_date, released_units FROM treasury_pools
 WHERE released_units > 0;

ALTER TABLE treasury_pools DROP COLUMN released_units, DROP COLUMN release_date;

-- ---------------------------------------------------------------- market data

ALTER TABLE price_observations
    ADD COLUMN session_vwap_kobo bigint,
    ADD COLUMN trade_count       int NOT NULL DEFAULT 0,
    ADD COLUMN auction_id        uuid REFERENCES auctions(id);

-- A real print must always beat a carry-forward on the same date. Ordering by
-- id alone lets a carry-forward row written later in the batch win.
DROP VIEW price_observations_current;
CREATE VIEW price_observations_current AS
    SELECT DISTINCT ON (instrument_id)
           instrument_id, obs_date, source, price_kobo, volume_units,
           session_vwap_kobo, trade_count, observed_at
      FROM price_observations
     ORDER BY instrument_id, obs_date DESC,
              CASE source WHEN 'manual' THEN 0 WHEN 'auction' THEN 1
                          WHEN 'clob' THEN 2 ELSE 3 END,
              id DESC;

-- ---------------------------------------------------------------- corrections

-- Every other table dates a business day as `date NOT NULL`; this one was
-- timestamptz and clearing never wrote it at all.
ALTER TABLE presentments
    ALTER COLUMN business_date TYPE date USING business_date::date;

COMMIT;
