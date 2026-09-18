-- Freedom 0018: the market maker quotes, and its quote can be the price.
--
-- 0012 gave a designated market maker an obligation and a per-session
-- measurement of it. Nothing generated the quote. This adds the record of
-- what the house quoting engine put into each session, the inventory it
-- steers towards, and a fifth price source: when nothing crosses but a firm
-- two-sided quote stood inside the band at the freeze, the session publishes
-- its mid rather than carrying the old reference forward.

BEGIN;

-- The inventory the provider is steered towards. The quoting engine skews
-- its quote up when the provider holds less than this (people have been
-- buying) and down when it holds more (people have been selling). Zero means
-- no skew.
ALTER TABLE liquidity_providers
    ADD COLUMN target_units bigint NOT NULL DEFAULT 0 CHECK (target_units >= 0);

-- One row per provider per session: the quote as computed and the orders it
-- became. lp_performance says whether the obligation was met; this says what
-- the engine decided and why, so a quote that looks wrong can be explained.
CREATE TABLE mm_quotes (
    provider_id     uuid NOT NULL REFERENCES liquidity_providers(id),
    session_date    date NOT NULL,
    instrument_id   text NOT NULL REFERENCES instruments(id),
    centre_kobo     bigint NOT NULL CHECK (centre_kobo > 0),
    skew_bps        int NOT NULL,
    spread_bps      int NOT NULL CHECK (spread_bps >= 0),
    held_units      bigint NOT NULL DEFAULT 0,
    target_units    bigint NOT NULL DEFAULT 0,
    bid_kobo        bigint,
    ask_kobo        bigint,
    bid_units       bigint NOT NULL DEFAULT 0,
    ask_units       bigint NOT NULL DEFAULT 0,
    bid_order_id    uuid REFERENCES orders(id),
    ask_order_id    uuid REFERENCES orders(id),
    note            text,
    quoted_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_id, session_date)
);

CREATE INDEX mm_quotes_instrument ON mm_quotes (instrument_id, session_date DESC);

-- 'quote': the mid of a firm two-sided quote that stood at the freeze of a
-- session in which nothing crossed. A price somebody named and was obliged
-- to trade at, which is more than a carry-forward is and less than a print.
ALTER TABLE price_observations DROP CONSTRAINT price_observations_source_check;
ALTER TABLE price_observations ADD CONSTRAINT price_observations_source_check
    CHECK (source IN ('auction','clob','carry_forward','manual','quote'));

-- Precedence on one date: a correction, then a print, then a quote, then a
-- carry-forward. A quote-only session must beat the carry-forward row the
-- same session would otherwise have written, and never beat a real print.
DROP VIEW price_observations_current;
CREATE VIEW price_observations_current AS
    SELECT DISTINCT ON (instrument_id)
           instrument_id, obs_date, source, price_kobo, volume_units,
           session_vwap_kobo, trade_count, observed_at
      FROM price_observations
     ORDER BY instrument_id, obs_date DESC,
              CASE source WHEN 'manual' THEN 0 WHEN 'auction' THEN 1
                          WHEN 'clob' THEN 2 WHEN 'quote' THEN 3 ELSE 4 END,
              id DESC;

COMMIT;
