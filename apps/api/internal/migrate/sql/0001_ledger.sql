-- Freedom 0001: the ledger.
--
-- Every value in this system is an integer in an asset's minor unit: naira as
-- kobo (scale 2), equity as 1e-8 share units (scale 8). Floating point never
-- appears, at rest or in transit.
--
-- The core invariant is that the entries of one transaction sum to zero
-- SEPARATELY FOR EACH ASSET. A buyback is a single economic event with two
-- independently balanced legs — naira leaves the fee pool for a company's
-- treasury, equity leaves that treasury for a cardholder's wallet — and summing
-- kobo against share units would be arithmetic on unlike things.

BEGIN;

-- ---------------------------------------------------------------- assets

CREATE TABLE assets (
    id      text PRIMARY KEY,                       -- 'NGN', 'EQ:MAMAPUT'
    class   text NOT NULL CHECK (class IN ('fiat', 'equity')),
    scale   smallint NOT NULL CHECK (scale BETWEEN 0 AND 12),
    label   text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO assets (id, class, scale, label) VALUES ('NGN', 'fiat', 2, 'Nigerian naira');

-- ---------------------------------------------------------------- accounts

-- Sign convention, uniformly: positive is a balance the owner HAS.
--   cardholder available    positive = spendable naira
--   cardholder hold         positive = authorised, not yet cleared
--   cardholder stock_wallet positive = equity owned
--   company    treasury     positive = unissued equity available to the buyback
--   scheme     buyback_pool positive = fee income earmarked for equity purchase
--   external   negative     = value that has left the system for a bank rail
CREATE TYPE account_kind AS ENUM (
    -- cardholder
    'available', 'hold', 'stock_wallet', 'buyback_residual',
    -- merchant
    'merchant_receivable', 'merchant_settled', 'merchant_reserve',
    -- scheme participant (issuer / acquirer member banks)
    'settlement', 'collateral', 'interchange_income',
    -- listed company
    'treasury', 'treasury_cash',
    -- the scheme itself
    'revenue', 'buyback_pool', 'promo_budget', 'stip_liability',
    'loss_reserve', 'float', 'suspense',
    -- the boundary of the system
    'external'
);

CREATE TABLE accounts (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_type  text NOT NULL CHECK (owner_type IN
                    ('cardholder', 'merchant', 'participant', 'company', 'scheme')),
    owner_id    uuid,                                -- NULL only for scheme-owned accounts
    kind        account_kind NOT NULL,
    asset_id    text NOT NULL REFERENCES assets(id),
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT scheme_accounts_are_unowned CHECK (
        (owner_type = 'scheme' AND owner_id IS NULL)
        OR (owner_type <> 'scheme' AND owner_id IS NOT NULL)),

    -- One account per (owner, kind, asset). A cardholder holds exactly one
    -- stock_wallet per instrument, which is what makes the asset part of the key.
    UNIQUE NULLS NOT DISTINCT (owner_type, owner_id, kind, asset_id),

    -- Redundant on its own, but it gives ledger_entries a composite foreign key
    -- to point at, which makes "naira posted into an equity account" not merely
    -- forbidden but unrepresentable. Cheaper and stronger than a Go check.
    UNIQUE (id, asset_id)
);

CREATE INDEX accounts_owner ON accounts (owner_type, owner_id);

-- ---------------------------------------------------------------- ledger

-- One row per economic event. The header exists so that the balance check fires
-- once per event rather than once per entry: a T+1 clearing run posts six
-- figures of entries in one database transaction, and a FOR EACH ROW trigger
-- that aggregates over its own table turns that into a quadratic scan.
CREATE TABLE ledger_tx (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type      text NOT NULL,

    -- The scheme's own business date, assigned at the switch from the cutover
    -- clock. Never derived from created_at: a tap at 20:01 Lagos time belongs to
    -- tomorrow's clearing batch, and a batch replayed next week must still land
    -- on the date it originally cleared.
    business_date   date NOT NULL,

    -- Exactly-once, enforced by the database rather than by the caller
    -- remembering. Every switch-driven post has a natural key available:
    -- acquirer|business_date|terminal|stan.
    idempotency_key text NOT NULL UNIQUE,

    correlation_id  uuid,
    created_at      timestamptz NOT NULL DEFAULT now(),

    -- The transaction id that wrote this header, captured at insert. Used below
    -- to make a committed transaction permanently unamendable.
    xid             bigint NOT NULL DEFAULT pg_current_xact_id()::text::bigint
);

CREATE INDEX ledger_tx_business_date ON ledger_tx (business_date);
CREATE INDEX ledger_tx_correlation   ON ledger_tx (correlation_id) WHERE correlation_id IS NOT NULL;

CREATE TABLE ledger_entries (
    id          bigserial PRIMARY KEY,
    tx_id       uuid   NOT NULL REFERENCES ledger_tx(id),
    account_id  uuid   NOT NULL,
    asset_id    text   NOT NULL,
    amount      bigint NOT NULL CHECK (amount <> 0),
    reason      text   NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),

    FOREIGN KEY (account_id, asset_id) REFERENCES accounts (id, asset_id)
);

CREATE INDEX ledger_entries_tx      ON ledger_entries (tx_id);
CREATE INDEX ledger_entries_account ON ledger_entries (account_id, id);

-- Settlement finality, enforced in the schema.
--
-- Entries may only be added by the same database transaction that created their
-- header. Once that commits the set is closed forever, so a correction is a new
-- transaction with its own header and its own audit trail — never an edit to a
-- batch that has already been reported to a participant. Without this, the
-- deferred balance trigger below could be bypassed entirely by appending a
-- single unbalanced entry to yesterday's transaction.
CREATE OR REPLACE FUNCTION assert_ledger_tx_open() RETURNS trigger AS $fn$
DECLARE
    owner_xid bigint;
BEGIN
    SELECT xid INTO owner_xid FROM ledger_tx WHERE id = NEW.tx_id;
    IF owner_xid IS NULL THEN
        RAISE EXCEPTION 'ledger entry references unknown transaction %', NEW.tx_id;
    END IF;
    IF owner_xid <> pg_current_xact_id()::text::bigint THEN
        RAISE EXCEPTION
            'ledger transaction % is closed: entries may only be added by the transaction that opened it',
            NEW.tx_id;
    END IF;
    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_entries_append_only
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION assert_ledger_tx_open();

-- The core invariant. Deferred to commit so a multi-statement transaction can
-- build up both legs, and attached to the header so it runs once per event.
CREATE OR REPLACE FUNCTION assert_ledger_balanced() RETURNS trigger AS $fn$
DECLARE
    bad_asset text;
    delta     bigint;
    n         int;
BEGIN
    SELECT count(*) INTO n FROM ledger_entries WHERE tx_id = NEW.id;
    IF n < 2 THEN
        RAISE EXCEPTION
            'ledger transaction % has % entries: a movement needs at least two', NEW.id, n;
    END IF;

    SELECT asset_id, SUM(amount) INTO bad_asset, delta
      FROM ledger_entries
     WHERE tx_id = NEW.id
     GROUP BY asset_id
    HAVING SUM(amount) <> 0
     LIMIT 1;

    IF FOUND THEN
        RAISE EXCEPTION
            'unbalanced ledger transaction %: % entries sum to % (must be 0)',
            NEW.id, bad_asset, delta;
    END IF;
    RETURN NULL;
END;
$fn$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER ledger_must_balance
    AFTER INSERT ON ledger_tx
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_ledger_balanced();

-- ---------------------------------------------------------------- balances

-- Correct but O(history). Fine for tests and for the first months of the
-- pilot; account_balance_snapshots below is what keeps it correct later.
CREATE VIEW account_balances AS
    SELECT a.id AS account_id, a.owner_type, a.owner_id, a.kind, a.asset_id,
           COALESCE(SUM(e.amount), 0)::bigint AS balance
      FROM accounts a
      LEFT JOIN ledger_entries e ON e.account_id = a.id
     GROUP BY a.id;

-- Written after each clearing cycle. Balance reads then become
-- "snapshot + entries since", which keeps the ledger append-only rather than
-- introducing a hot mutable balance row per account — the scheme fee pool and
-- each treasury would otherwise be a single-row write lock that serialises the
-- entire clearing run.
CREATE TABLE account_balance_snapshots (
    account_id   uuid NOT NULL REFERENCES accounts(id),
    as_of_entry  bigint NOT NULL,          -- max(ledger_entries.id) included
    balance      bigint NOT NULL,
    taken_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, as_of_entry)
);

COMMIT;
