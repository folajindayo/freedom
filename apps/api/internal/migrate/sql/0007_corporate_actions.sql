-- Freedom 0007: corporate actions.
--
-- Splits, bonus issues and cash dividends. All three occur at Nigerian SMEs,
-- and the dividend is the best retention loop the product has: a shop paying
-- its own customers, in cash they can spend back at the shop.
--
-- The hard part is not paying them. It is that every one of these makes
-- historical prices non-comparable, and one particular consequence is
-- exploitable — see corporate_action_factors below.

BEGIN;

-- An exact product aggregate over numeric.
--
-- The obvious spelling for a running product is EXP(SUM(LN(x))), which is
-- double precision. This is a money path: a chain of splits must produce the
-- same adjusted price on every machine and every run, and float drift there is
-- indistinguishable from a real price move.
CREATE AGGREGATE product(numeric) (
    SFUNC    = numeric_mul,
    STYPE    = numeric,
    INITCOND = '1'
);

CREATE TABLE corporate_actions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    instrument_id text NOT NULL REFERENCES instruments(id),
    kind          text NOT NULL CHECK (kind IN
                  ('split','bonus','cash_dividend','return_of_capital')),

    -- Ratios as exact integers: num new shares for every den old ones. A 2:1
    -- split is 2/1; a 1-for-4 bonus issue is 5/4. Never a float, and never a
    -- numeric whose scale was decided by a cast somewhere.
    ratio_num     bigint CHECK (ratio_num IS NULL OR ratio_num > 0),
    ratio_den     bigint CHECK (ratio_den IS NULL OR ratio_den > 0),

    dps_kobo      bigint CHECK (dps_kobo IS NULL OR dps_kobo > 0),
    -- Nigerian withholding on dividends. Held separately and remitted; it is
    -- never the scheme's income.
    wht_bps       int NOT NULL DEFAULT 1000,

    announced_on  date NOT NULL,
    ex_date       date NOT NULL,
    record_date   date NOT NULL,
    pay_date      date,

    state         text NOT NULL DEFAULT 'announced'
                  CHECK (state IN ('announced','record_taken','applied','cancelled')),
    ledger_tx_id  uuid REFERENCES ledger_tx(id),
    created_at    timestamptz NOT NULL DEFAULT now(),

    CHECK (record_date >= ex_date),
    CONSTRAINT ratio_or_dividend CHECK (
        (kind IN ('split','bonus') AND ratio_num IS NOT NULL AND ratio_den IS NOT NULL
                                   AND dps_kobo IS NULL)
     OR (kind IN ('cash_dividend','return_of_capital') AND dps_kobo IS NOT NULL
                                   AND ratio_num IS NULL))
);

CREATE INDEX corporate_actions_pending ON corporate_actions (instrument_id, ex_date)
    WHERE state <> 'applied';

-- The holder-of-record snapshot.
--
-- Taken at the record date and never recomputed. The holder set at pay time is
-- not the holder set at record date — people trade in between — and rebuilding
-- it afterwards from ledger history is a reconstruction nobody would trust with
-- their own dividend.
CREATE TABLE corporate_action_entitlements (
    action_id     uuid NOT NULL REFERENCES corporate_actions(id),
    account_id    uuid NOT NULL REFERENCES accounts(id),
    units_held    bigint NOT NULL CHECK (units_held > 0),
    gross_kobo    bigint NOT NULL DEFAULT 0 CHECK (gross_kobo >= 0),
    wht_kobo      bigint NOT NULL DEFAULT 0 CHECK (wht_kobo >= 0),
    units_delta   bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (action_id, account_id)
);

-- How to compare a price from before an action with one from after.
--
-- This table is not a nicety. buyback's trailing-VWAP cap is the defence
-- against a listing ramping its own price, and it reads price history. After a
-- 2:1 split every prior session reads at TWICE the real price, so the cap sits
-- at twice the current price and stops binding for the length of the window.
-- An issuer who wants to ramp simply splits the week before.
--
-- So the cap reads the adjusted view below, and these factors are what make it
-- correct across an action.
CREATE TABLE corporate_action_factors (
    instrument_id  text NOT NULL REFERENCES instruments(id),
    effective_date date NOT NULL,
    action_id      uuid NOT NULL REFERENCES corporate_actions(id),
    -- Multiply a historical price by the product of every factor effective
    -- AFTER that observation to express it in today's terms.
    price_factor   numeric(20,12) NOT NULL CHECK (price_factor > 0),
    unit_factor    numeric(20,12) NOT NULL CHECK (unit_factor > 0),
    PRIMARY KEY (instrument_id, effective_date, action_id)
);

CREATE VIEW price_observations_adjusted AS
    SELECT po.id, po.instrument_id, po.obs_date, po.source, po.price_kobo,
           po.volume_units, po.trade_count, po.observed_at,
           (po.price_kobo * COALESCE((
              SELECT product(f.price_factor) FROM corporate_action_factors f
               WHERE f.instrument_id = po.instrument_id
                 AND f.effective_date > po.obs_date), 1))::bigint AS adj_price_kobo,
           (po.volume_units * COALESCE((
              SELECT product(f.unit_factor) FROM corporate_action_factors f
               WHERE f.instrument_id = po.instrument_id
                 AND f.effective_date > po.obs_date), 1))::bigint AS adj_volume_units
      FROM price_observations po;

COMMIT;
