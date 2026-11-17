-- 000014_quest_completions_pending.up.sql
-- The cooldown starts when a member marks a chore done, not when the
-- administrator approves it. Each mark-done is its own quest_completions row,
-- pending until the administrator approves, rejects it or the member is
-- removed (cancelled). Existing rows are approved history and stay as they are.

-- 1. The one definition of when a chore comes back: local midnight in the
--    household's timezone, repeat_days days after the local day it was marked
--    done. MarkQuestDone and the backfill below both call it.
CREATE FUNCTION quest_next_available_at(done_at TIMESTAMPTZ, tz TEXT, repeat_days INT)
RETURNS TIMESTAMPTZ
LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
    SELECT (date_trunc('day', done_at AT TIME ZONE tz) + make_interval(days => repeat_days)) AT TIME ZONE tz
$$;

-- 2. A completion's status. Every existing row was recorded at approval.
ALTER TABLE quest_completions ADD COLUMN status TEXT NOT NULL DEFAULT 'approved';
ALTER TABLE quest_completions ALTER COLUMN status DROP DEFAULT;
ALTER TABLE quest_completions
    ADD CONSTRAINT quest_completions_status_check
        CHECK (status IN ('pending', 'approved', 'rejected', 'cancelled'));

-- 3. approved_at is set exactly when the completion is approved.
ALTER TABLE quest_completions ALTER COLUMN approved_at DROP DEFAULT;
ALTER TABLE quest_completions ALTER COLUMN approved_at DROP NOT NULL;
ALTER TABLE quest_completions
    ADD CONSTRAINT quest_completions_approved_at_check
        CHECK ((status = 'approved') = (approved_at IS NOT NULL));

-- 4. Approved history outlives the member: removing a member nulls the member
--    reference and keeps household_id.
ALTER TABLE quest_completions ALTER COLUMN member_id DROP NOT NULL;
ALTER TABLE quest_completions DROP CONSTRAINT quest_completions_household_id_member_id_fkey;
ALTER TABLE quest_completions
    ADD CONSTRAINT quest_completions_household_id_member_id_fkey
        FOREIGN KEY (household_id, member_id)
        REFERENCES family_members (household_id, id)
        ON DELETE SET NULL (member_id);

-- 5. The Approvals list reads pending completions oldest first.
CREATE INDEX idx_quest_completions_pending
    ON quest_completions (household_id, done_at, id)
    WHERE status = 'pending';

-- 6. Backfill: each quest waiting for approval becomes a pending completion
--    (same member, same mark-done time), and the quest is freed with its
--    cooldown counted from that mark-done time. No points move.
INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at, status, approved_at)
SELECT gen_random_uuid(), hq.household_id, hq.id, hq.claimed_by, hq.done_at, 'pending', NULL
FROM household_quests hq
WHERE hq.done_at IS NOT NULL;

-- done_at is cleared with the holder in one UPDATE: the 000008 CHECK forbids a
-- done_at without a holder. The SET expressions read the row's old values.
UPDATE household_quests hq
SET claimed_by = NULL, claimed_at = NULL, done_at = NULL,
    completed_at = hq.done_at,
    available_at = quest_next_available_at(hq.done_at, h.timezone, hq.repeat_days),
    updated_at = NOW()
FROM households h
WHERE h.id = hq.household_id AND hq.done_at IS NOT NULL;
