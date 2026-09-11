-- Freedom 0005: account kinds the exchange needs.
--
-- Deliberately NOT wrapped in BEGIN/COMMIT, and deliberately alone in this file.
-- Postgres permits ALTER TYPE ... ADD VALUE inside a transaction block but
-- forbids USING the new value until that transaction has committed. A single
-- file that adds a kind and then references it in a constraint or an insert
-- fails halfway through, leaving a schema that is partly applied and a
-- migration that is unrecorded.
--
-- The reserve kinds are new rather than a reuse of the card 'hold' kind. Both
-- mean "set aside", but their lifecycles are driven by different jobs with
-- different retry semantics, and sharing one kind means an order release
-- eventually frees card authorisation value. They would cross exactly once, at
-- two in the morning, for real money.

ALTER TYPE account_kind ADD VALUE IF NOT EXISTS 'order_cash_reserve';
ALTER TYPE account_kind ADD VALUE IF NOT EXISTS 'order_share_reserve';
ALTER TYPE account_kind ADD VALUE IF NOT EXISTS 'exchange_fee_income';
ALTER TYPE account_kind ADD VALUE IF NOT EXISTS 'tax_withheld';
