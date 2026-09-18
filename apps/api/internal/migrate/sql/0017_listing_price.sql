-- Freedom 0017: the listing price is the exchange's decision, on the record.
--
-- The applicant does not name a price. The exchange sets it from audited
-- net assets and revenue over the shares in issue (docs/LISTING-RULES.md
-- §2.4). The figures that decision was made from are kept on the application
-- beside the findings, so a merchant re-reading its record — listed or
-- refused — sees the fair value and the price it lists at without parsing
-- a finding's text.

BEGIN;

ALTER TABLE listing_applications
    ADD COLUMN fair_value_kobo    bigint NOT NULL DEFAULT 0 CHECK (fair_value_kobo >= 0),
    ADD COLUMN listing_price_kobo bigint NOT NULL DEFAULT 0 CHECK (listing_price_kobo >= 0);

COMMIT;
