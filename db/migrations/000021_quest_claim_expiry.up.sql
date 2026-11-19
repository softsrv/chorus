-- 000021_quest_claim_expiry.up.sql
-- A claimed quest is only held until the end of the local day it was accepted:
-- household-local midnight. claim_expires_at stores that deadline for a held
-- quest and is NULL for an unheld one.

-- 1. The one definition of when a claim ends: the local midnight that closes
--    the local day of claimed_at in the household's timezone. ClaimQuest and
--    the backfill below both call it.
CREATE FUNCTION quest_claim_expires_at(claimed_at TIMESTAMPTZ, tz TEXT)
RETURNS TIMESTAMPTZ
LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
    SELECT (date_trunc('day', claimed_at AT TIME ZONE tz) + INTERVAL '1 day') AT TIME ZONE tz
$$;

-- 2. The deadline of a held quest; NULL when nobody holds it.
ALTER TABLE household_quests ADD COLUMN claim_expires_at TIMESTAMPTZ;

-- 3. Quests held when this goes live end at the first local midnight after
--    release (they keep the rest of today).
UPDATE household_quests q
SET claim_expires_at = quest_claim_expires_at(NOW(), h.timezone)
FROM households h
WHERE h.id = q.household_id AND q.claimed_by IS NOT NULL;

-- 4. The expiry sweep finds overdue held quests by deadline.
CREATE INDEX household_quests_claim_expires_at_idx
    ON household_quests (claim_expires_at)
    WHERE claimed_by IS NOT NULL;

-- 5. The database keeps the deadline with the holder for every writer, not
--    only the new ClaimQuest. Migrations run by hand before the restart, so
--    the old binary keeps serving for a while: its accept sets claimed_by
--    and claimed_at alone, and its mark-done clears claimed_by alone. On
--    every insert or update, a held quest with no deadline gets the local
--    midnight ending the day of its claimed_at (now when that is missing),
--    and an unheld quest's deadline is cleared. It fires on every update,
--    the ON DELETE SET NULL of claimed_by included.
CREATE FUNCTION household_quests_keep_claim_expiry()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.claimed_by IS NULL THEN
        NEW.claim_expires_at := NULL;
    ELSIF NEW.claim_expires_at IS NULL THEN
        NEW.claim_expires_at := (
            SELECT quest_claim_expires_at(COALESCE(NEW.claimed_at, NOW()), h.timezone)
            FROM households h WHERE h.id = NEW.household_id
        );
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER household_quests_keep_claim_expiry
    BEFORE INSERT OR UPDATE ON household_quests
    FOR EACH ROW EXECUTE FUNCTION household_quests_keep_claim_expiry();

-- 6. And says so: a quest is held exactly when it has a deadline.
ALTER TABLE household_quests ADD CONSTRAINT household_quests_claim_expiry_with_holder
    CHECK ((claimed_by IS NULL) = (claim_expires_at IS NULL));
