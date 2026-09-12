-- Freedom 0012: designated market makers.
--
-- The liquidity answer for a market whose buyers arrive automatically and whose
-- sellers do not. An obligation nobody measures is a claim in a prospectus you
-- cannot defend, so the measurement is built alongside the obligation.

BEGIN;

CREATE TABLE liquidity_providers (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    instrument_id   text NOT NULL REFERENCES instruments(id),
    member_id       uuid NOT NULL REFERENCES members(id),

    -- The obligation, expressed per auction session rather than per second.
    -- A symbol that trades once a day cannot be quoted continuously, so the
    -- duty is to put a two-sided quote into every session.
    min_quote_kobo  bigint NOT NULL CHECK (min_quote_kobo > 0),
    max_spread_bps  int NOT NULL CHECK (max_spread_bps > 0),
    min_uptime_bps  int NOT NULL DEFAULT 8000 CHECK (min_uptime_bps BETWEEN 0 AND 10000),

    -- What they are paid for it, as a rebate on the fees they generate.
    rebate_bps      int NOT NULL DEFAULT 0 CHECK (rebate_bps BETWEEN 0 AND 10000),

    effective       daterange NOT NULL,
    state           text NOT NULL DEFAULT 'active'
                    CHECK (state IN ('active','warned','suspended','terminated')),
    created_at      timestamptz NOT NULL DEFAULT now(),

    -- One provider per member per instrument per period.
    EXCLUDE USING gist (instrument_id WITH =, member_id WITH =, effective WITH &&)
);

-- Session-by-session performance. The obligation is only real because this
-- exists: a market maker who quoted on the days it suited them and not on the
-- days the price was moving has met no obligation at all, and only a per-session
-- record shows that.
CREATE TABLE lp_performance (
    provider_id     uuid NOT NULL REFERENCES liquidity_providers(id),
    session_date    date NOT NULL,
    quoted          boolean NOT NULL DEFAULT false,
    two_sided       boolean NOT NULL DEFAULT false,
    bid_kobo        bigint,
    ask_kobo        bigint,
    spread_bps      int,
    size_kobo       bigint,
    met             boolean NOT NULL DEFAULT false,
    shortfall       text,
    PRIMARY KEY (provider_id, session_date)
);

CREATE INDEX lp_performance_missed ON lp_performance (provider_id, session_date)
    WHERE NOT met;

COMMIT;
