-- Freedom 0015: one live instrument per company.
--
-- A company has one listed line of shares. Every path from a company to its
-- instrument — clearing's buyback funding, the rail's business view, the
-- index — joins instruments by company_id and a live status, and each of
-- those joins silently picks an arbitrary row the moment a second live
-- instrument exists. That happened: a second merchant of an already-listed
-- company was onboarded under a new symbol, and its customers' buybacks were
-- filled in the first symbol. The invariant belongs in the schema, where a
-- second listing fails loudly instead of being joined at random.
--
-- Delisted and draft rows are excluded: a company may be relisted after a
-- delisting, and a draft is an application that has not admitted anything.

BEGIN;

CREATE UNIQUE INDEX instruments_one_live_per_company
    ON instruments (company_id)
    WHERE status IN ('listed','halted','suspended');

COMMENT ON INDEX instruments_one_live_per_company IS
    'One live line of shares per company: a second would make every company-to-instrument join ambiguous.';

COMMIT;
