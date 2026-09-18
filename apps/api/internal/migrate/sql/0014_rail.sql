-- Freedom 0014: the rail door.
--
-- Tapp is a working card rail with its own cardholders, merchants and
-- credentials. Freedom's ledger sees it as one scheme participant that is both
-- issuer and acquirer of every tap it delivers (docs/INTEGRATION.md). Nothing
-- here is a new concept: it is the columns that let an existing row be found
-- by the id the rail already uses for it.

BEGIN;

-- The rail's own ids, stored verbatim and never interpreted. UNIQUE because a
-- second row for the same rail id would split one person's holdings across two
-- wallets. The Tapp participant itself needs no column: participants.code is
-- already unique, and 'TAPP' is the marker.
ALTER TABLE cardholders ADD COLUMN external_ref text UNIQUE;
ALTER TABLE merchants   ADD COLUMN external_ref text UNIQUE;

-- cardholders.phone is NOT NULL UNIQUE and the rail does not send one: the
-- phone is Tapp's KYC record, not Freedom's. Relaxing the constraint would
-- silently permit phoneless cardholders on the card rail proper, where a phone
-- is the recovery channel. So a rail cardholder carries a deterministic
-- placeholder derived from the external ref ('tapp:' || external_ref), which
-- keeps the constraint and cannot collide with a real E.164 number.
COMMENT ON COLUMN cardholders.external_ref IS
    'The rail''s own id for this person (Tapp user id). Rail cardholders carry '
    'phone = ''tapp:'' || external_ref because the phone is held by the rail.';
COMMENT ON COLUMN merchants.external_ref IS
    'The rail''s own id for this merchant (Tapp sender profile id).';

-- A buyback unwind was only ever caused by a chargeback. A rail reversal is
-- the same economic event — the sale did not happen, the shares must come back
-- — without a dispute row, so the cause is recorded as one or the other, never
-- neither: an unwind with no traceable cause is a share movement nobody can
-- explain later.
ALTER TABLE buyback_unwinds
    ALTER COLUMN dispute_id DROP NOT NULL,
    ADD COLUMN reversal_presentment_id uuid REFERENCES presentments(id),
    ADD CONSTRAINT buyback_unwinds_have_a_cause
        CHECK (num_nonnulls(dispute_id, reversal_presentment_id) >= 1);

COMMIT;
