-- Ọja 0004: close the two-token window against concurrent replay.
--
-- The previous rolling token stays live so that a write-back which fails in the
-- field does not lock out a real cardholder. But "the tag still holds the old
-- token" and "someone copied the old token and is using it at the same moment"
-- present identically — unless the tag's read counter is consulted.
--
-- NTAG21x increments a one-way counter on the first valid READ after entering
-- a field, so a genuine re-tap always reports a HIGHER counter than the tap
-- that rotated the token. A duplicate of the same tap reports the SAME counter.
-- Recording the counter at rotation makes the two distinguishable.

BEGIN;

ALTER TABLE card_credentials
    ADD COLUMN token_prev_from_counter bigint NOT NULL DEFAULT 0;

COMMENT ON COLUMN card_credentials.token_prev_from_counter IS
    'Counter value at which token_prev was superseded. The previous token is '
    'accepted only above this, so a replay of the same tap is refused while a '
    'genuine later tap after a failed write-back still works.';

COMMIT;
