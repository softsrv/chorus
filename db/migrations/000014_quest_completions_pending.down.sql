-- 000014_quest_completions_pending.down.sql
-- Back to one pending approval per quest, held on household_quests.done_at.

-- 1. A quest nobody holds gets its oldest pending completion back: the member
--    holds it again and it waits for approval. A quest someone took since
--    keeps its holder and gains no done_at; its pending completions are lost.
UPDATE household_quests hq
SET claimed_by = p.member_id, claimed_at = NOW(), done_at = p.done_at, updated_at = NOW()
FROM (
    SELECT DISTINCT ON (c.quest_id) c.household_id, c.quest_id, c.member_id, c.done_at
    FROM quest_completions c
    WHERE c.status = 'pending' AND c.member_id IS NOT NULL
    ORDER BY c.quest_id, c.done_at ASC, c.id ASC
) p
WHERE hq.household_id = p.household_id AND hq.id = p.quest_id
  AND hq.claimed_by IS NULL;

-- 2. Only approved completions of current members fit the old table.
DELETE FROM quest_completions WHERE status <> 'approved' OR member_id IS NULL;

DROP INDEX idx_quest_completions_pending;

ALTER TABLE quest_completions DROP CONSTRAINT quest_completions_household_id_member_id_fkey;
ALTER TABLE quest_completions ALTER COLUMN member_id SET NOT NULL;
ALTER TABLE quest_completions
    ADD CONSTRAINT quest_completions_household_id_member_id_fkey
        FOREIGN KEY (household_id, member_id)
        REFERENCES family_members (household_id, id)
        ON DELETE CASCADE;

ALTER TABLE quest_completions DROP CONSTRAINT quest_completions_approved_at_check;
ALTER TABLE quest_completions ALTER COLUMN approved_at SET NOT NULL;
ALTER TABLE quest_completions ALTER COLUMN approved_at SET DEFAULT NOW();

ALTER TABLE quest_completions DROP CONSTRAINT quest_completions_status_check;
ALTER TABLE quest_completions DROP COLUMN status;

DROP FUNCTION quest_next_available_at(TIMESTAMPTZ, TEXT, INT);
