-- Freedom 0016: the issuer's share count.
--
-- The cap table reported "in issue" as the units on Freedom's own register,
-- which is the founders' lots and what taps have bought — not the company's
-- shares in issue, most of which are held off-exchange. A company's value is
-- its shares in issue times the price, so the count the applicant declared at
-- admission is kept on the instrument rather than left inside a finding's text.

BEGIN;

ALTER TABLE instruments
    ADD COLUMN shares_in_issue_units bigint NOT NULL DEFAULT 0
        CHECK (shares_in_issue_units >= 0);

COMMIT;
