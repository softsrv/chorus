-- 000021_quest_claim_expiry.down.sql
-- Back to claims that never expire: the deadline column goes, held quests stay
-- held.
DROP TRIGGER IF EXISTS household_quests_keep_claim_expiry ON household_quests;
DROP FUNCTION IF EXISTS household_quests_keep_claim_expiry();
ALTER TABLE household_quests DROP CONSTRAINT IF EXISTS household_quests_claim_expiry_with_holder;
DROP INDEX IF EXISTS household_quests_claim_expires_at_idx;
ALTER TABLE household_quests DROP COLUMN IF EXISTS claim_expires_at;
DROP FUNCTION IF EXISTS quest_claim_expires_at(TIMESTAMPTZ, TEXT);
