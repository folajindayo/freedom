-- Freedom 0008: market integrity and session control.
--
-- Surveillance case management, closed periods, and the operational tables that
-- turn a detection into something a human actually works.

BEGIN;

-- closed_periods excludes overlapping ranges per instrument, which needs
-- btree_gist. db/bootstrap.sql installs it locally; a hosted database that
-- never ran bootstrap must get it here. Trusted since PG13, so the migrating
-- role may install it, and IF NOT EXISTS makes this a no-op where it exists.
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- A detection with no case management is a log file. Alerts need an owner, a
-- state, and a decision that someone put their name to.
ALTER TABLE surveillance_alerts
    ADD COLUMN state       text NOT NULL DEFAULT 'open'
                CHECK (state IN ('open','triaged','escalated','referred','closed','false_positive')),
    ADD COLUMN assigned_to text,
    ADD COLUMN triaged_at  timestamptz,
    ADD COLUMN closed_by   text,
    ADD COLUMN notes       text;

CREATE INDEX surveillance_work_queue ON surveillance_alerts (severity, raised_at)
    WHERE state IN ('open', 'triaged');

-- Closed periods.
--
-- Insiders may not trade around results and corporate actions, because they
-- know first. Freedom already bars related parties from auction-only symbols
-- outright, but a symbol that graduates to continuous trading loses that
-- blanket ban and needs the narrower rule.
CREATE TABLE closed_periods (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    instrument_id text NOT NULL REFERENCES instruments(id),
    reason        text NOT NULL CHECK (reason IN
                  ('results','corporate_action','offering','regulatory')),
    period        daterange NOT NULL,
    declared_by   text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    EXCLUDE USING gist (instrument_id WITH =, period WITH &&)
);

-- Order-to-trade ratios and message rates, per member per day.
--
-- A member spraying orders they never intend to trade is either malfunctioning
-- or layering, and both need the same response: stop accepting their orders.
CREATE TABLE member_activity (
    member_id     uuid NOT NULL REFERENCES members(id),
    session_date  date NOT NULL,
    orders_sent   int NOT NULL DEFAULT 0,
    orders_filled int NOT NULL DEFAULT 0,
    cancels       int NOT NULL DEFAULT 0,
    rejects       int NOT NULL DEFAULT 0,
    PRIMARY KEY (member_id, session_date)
);

ALTER TABLE members
    -- The kill switch. Set by surveillance or by the member themselves; the
    -- order gateway refuses everything while it is on.
    ADD COLUMN halted            boolean NOT NULL DEFAULT false,
    ADD COLUMN halted_reason     text,
    ADD COLUMN max_orders_per_session int NOT NULL DEFAULT 10000,
    ADD COLUMN max_order_to_trade_ratio int NOT NULL DEFAULT 50;

-- Fat-finger bounds, per instrument.
ALTER TABLE instruments
    ADD COLUMN max_order_units bigint,
    -- A session that moves more than this from the reference is suspicious
    -- enough to halt rather than publish.
    ADD COLUMN halt_move_bps int NOT NULL DEFAULT 3000;

COMMIT;
