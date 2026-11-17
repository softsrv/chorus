-- Household quest queries are scoped by household_id. Membership is checked
-- by the service layer first; the household_id predicate guarantees a quest ID
-- from another household can never be read or modified through this one.
-- The quest library is global and read-only: there are no write queries on it.

-- name: ListQuestLibrary :many
SELECT * FROM quest_library ORDER BY name ASC, id ASC;

-- name: GetQuestLibraryItem :one
SELECT * FROM quest_library WHERE id = $1;

-- name: ListHouseholdQuests :many
-- awaiting_approval: the quest has a completion waiting for the administrator.
SELECT sqlc.embed(q), EXISTS (
    SELECT 1 FROM quest_completions c
    WHERE c.household_id = q.household_id AND c.quest_id = q.id AND c.status = 'pending'
) AS awaiting_approval
FROM household_quests q
WHERE q.household_id = sqlc.arg(household_id)
ORDER BY q.created_at ASC, q.id ASC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: GetHouseholdQuest :one
SELECT * FROM household_quests WHERE household_id = $1 AND id = $2;

-- name: GetHouseholdQuestView :one
-- One quest with awaiting_approval, as ListHouseholdQuests reads it.
SELECT sqlc.embed(q), EXISTS (
    SELECT 1 FROM quest_completions c
    WHERE c.household_id = q.household_id AND c.quest_id = q.id AND c.status = 'pending'
) AS awaiting_approval
FROM household_quests q
WHERE q.household_id = sqlc.arg(household_id) AND q.id = sqlc.arg(id);

-- name: QuestHasPendingCompletion :one
-- Whether the quest has a completion waiting for the administrator.
SELECT EXISTS (
    SELECT 1 FROM quest_completions c
    WHERE c.household_id = sqlc.arg(household_id) AND c.quest_id = sqlc.arg(quest_id)
      AND c.status = 'pending'
) AS awaiting_approval;

-- name: CreateHouseholdQuest :one
INSERT INTO household_quests (id, household_id, library_id, name, points, repeat_days, notes, streak_bonus, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), NOW())
RETURNING *;

-- name: UpdateHouseholdQuest :one
UPDATE household_quests
SET name = $3, points = $4, repeat_days = $5, notes = $6, streak_bonus = $7, updated_at = NOW()
WHERE household_id = $1 AND id = $2
RETURNING *;

-- name: DeleteHouseholdQuest :execrows
DELETE FROM household_quests WHERE household_id = $1 AND id = $2;

-- name: ClaimQuest :one
-- Assigns an available quest to a family member. The availability check and
-- the write are one statement, so of two concurrent claims exactly one
-- matches; the other gets no row.
UPDATE household_quests
SET claimed_by = sqlc.arg(member_id), claimed_at = NOW(), updated_at = NOW()
WHERE household_id = sqlc.arg(household_id) AND id = sqlc.arg(id)
  AND claimed_by IS NULL
  AND (available_at IS NULL OR available_at <= NOW())
RETURNING *;

-- name: AbandonQuest :one
-- Puts a quest back in the pool. Only its current holder can abandon it.
UPDATE household_quests
SET claimed_by = NULL, claimed_at = NULL, updated_at = NOW()
WHERE household_id = sqlc.arg(household_id) AND id = sqlc.arg(id)
  AND claimed_by = sqlc.arg(member_id)
RETURNING *;

-- name: ReleaseMemberQuests :exec
-- Returns every quest the member holds to the pool. done_at is cleared with
-- the holder, so the done_at-needs-a-holder CHECK holds. No points move.
UPDATE household_quests
SET claimed_by = NULL, claimed_at = NULL, done_at = NULL, updated_at = NOW()
WHERE household_id = sqlc.arg(household_id) AND claimed_by = sqlc.arg(member_id);

-- name: CancelMemberPendingCompletions :exec
-- Cancels every completion of the member still waiting for approval. No
-- points move. As on reject, a quest whose running cooldown one of those
-- completions started (the quest's completed_at is its done_at, available_at
-- still ahead) is available again right away: the chore was never approved,
-- so it does not sit resting. Cooldowns started by anyone else stay.
WITH cancelled AS (
    UPDATE quest_completions cc
    SET status = 'cancelled'
    WHERE cc.household_id = sqlc.arg(household_id) AND cc.member_id = sqlc.arg(member_id)
      AND cc.status = 'pending'
    RETURNING cc.household_id, cc.quest_id, cc.done_at
)
UPDATE household_quests q
SET available_at = NOW(), updated_at = NOW()
FROM cancelled c
WHERE q.household_id = c.household_id AND q.id = c.quest_id
  AND q.completed_at = c.done_at AND q.available_at > NOW();

-- name: GetPendingCompletionMember :one
-- The member of a pending completion, read without a lock so Approve can
-- lock that member's row before it locks the completion (the order
-- FamilyService.Remove uses).
SELECT member_id FROM quest_completions
WHERE household_id = sqlc.arg(household_id) AND id = sqlc.arg(id)
  AND status = 'pending' AND member_id IS NOT NULL;

-- name: MarkQuestDone :one
-- The holder marks the quest done. In one statement the quest is released
-- and starts its cooldown from now (quest_next_available_at: local midnight
-- in the household's timezone, repeat_days days after today), and a pending
-- completion recording the member and the mark-done time is inserted for the
-- administrator. NOW() is one instant per transaction, so completed_at equals
-- the completion's done_at. A quest the member does not hold matches no row
-- and inserts nothing. No points move here.
--
-- The same statement moves the member's streak for the quest (streaked):
-- count+1 when marked done by its expires_at, else 1, with a new expires_at
-- of local midnight starting the day after 2 x repeat_days days from today.
-- The step it replaces is kept in prev_* so a reject can undo it. The
-- completion stores the resulting count (streak_count), so its approval,
-- however late, pays the streak as of this mark-done. The streak write
-- touches only member_quest_streaks: never available_at or the cooldown.
WITH done AS (
    UPDATE household_quests q
    SET claimed_by = NULL, claimed_at = NULL,
        completed_at = NOW(),
        available_at = quest_next_available_at(NOW(), h.timezone, q.repeat_days),
        updated_at = NOW()
    FROM households h
    WHERE h.id = q.household_id
      AND q.household_id = sqlc.arg(household_id) AND q.id = sqlc.arg(id)
      AND q.claimed_by = sqlc.arg(member_id)::uuid
    RETURNING q.*, h.timezone
), streaked AS (
    INSERT INTO member_quest_streaks (
        household_id, member_id, quest_id,
        count, last_done_at, expires_at,
        created_at, updated_at
    )
    SELECT
        d.household_id,
        sqlc.arg(member_id)::uuid,
        d.id,
        1,
        NOW(),
        quest_streak_expires_at(NOW(), d.timezone, d.repeat_days),
        NOW(),
        NOW()
    FROM done d
    ON CONFLICT (member_id, quest_id) DO UPDATE
    SET count = CASE
            WHEN NOW() <= member_quest_streaks.expires_at THEN member_quest_streaks.count + 1
            ELSE 1
        END,
        prev_count = member_quest_streaks.count,
        prev_last_done_at = member_quest_streaks.last_done_at,
        prev_expires_at = member_quest_streaks.expires_at,
        last_done_at = NOW(),
        expires_at = EXCLUDED.expires_at,
        updated_at = NOW()
    RETURNING quest_id, count
), recorded AS (
    INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at, status, streak_count)
    SELECT sqlc.arg(completion_id)::uuid, d.household_id, d.id, sqlc.arg(member_id)::uuid, NOW(), 'pending', s.count
    FROM done d
    JOIN streaked s ON s.quest_id = d.id
    RETURNING id
)
SELECT d.id, d.household_id, d.library_id, d.name, d.points, d.repeat_days, d.notes,
       d.claimed_by, d.claimed_at, d.done_at, d.completed_at, d.available_at,
       d.created_at, d.updated_at, d.streak_bonus
FROM done d;

-- name: GetMemberQuestStreak :one
SELECT * FROM member_quest_streaks
WHERE household_id = sqlc.arg(household_id)
  AND member_id = sqlc.arg(member_id)
  AND quest_id = sqlc.arg(quest_id);

-- name: ListPendingCompletions :many
-- Completions waiting for approval, one row each, oldest mark-done first,
-- with the chore's name and points and the name of the member who did it.
SELECT c.id, c.quest_id, q.name AS quest_name, q.points, c.member_id,
       m.name AS member_name, c.done_at
FROM quest_completions c
JOIN household_quests q ON q.household_id = c.household_id AND q.id = c.quest_id
JOIN family_members m ON m.household_id = c.household_id AND m.id = c.member_id
WHERE c.household_id = sqlc.arg(household_id) AND c.status = 'pending'
ORDER BY c.done_at ASC, c.id ASC;

-- name: ApproveCompletion :one
-- Approves one pending completion. The completion is locked first and its
-- status re-read under the lock, so of two concurrent approvals exactly one
-- gets a row. The quest is not touched: its cooldown already started at
-- mark-done. points is the chore's current points, to credit to member_id;
-- quest_name is the chore's name, the reason in the points history.
-- streak_bonus is the chore's current streak bonus and streak_count the
-- member's streak as fixed at this completion's mark-done (NULL for one
-- marked done before streaks existed); neither is recomputed from NOW().
-- member_id must match the member the caller already locked.
WITH pending AS (
    SELECT pc.id, q.points, q.name, q.streak_bonus
    FROM quest_completions pc
    JOIN household_quests q ON q.household_id = pc.household_id AND q.id = pc.quest_id
    WHERE pc.household_id = sqlc.arg(household_id) AND pc.id = sqlc.arg(id)
      AND pc.status = 'pending' AND pc.member_id = sqlc.arg(member_id)::uuid
    FOR UPDATE OF pc
)
UPDATE quest_completions c
SET status = 'approved', approved_at = NOW()
FROM pending p
WHERE c.id = p.id AND c.status = 'pending'
RETURNING c.id, c.quest_id, c.member_id, c.done_at, c.approved_at, p.points, p.name AS quest_name,
          p.streak_bonus, c.streak_count;

-- name: RejectCompletion :one
-- Rejects one pending completion: no points. If it started the quest's
-- running cooldown (the quest's completed_at is its done_at) and that
-- cooldown has not ended, the cooldown is cancelled so the chore is available
-- again right away. Other completions and the quest's holder are untouched.
WITH rejected AS (
    UPDATE quest_completions rc
    SET status = 'rejected'
    WHERE rc.household_id = sqlc.arg(household_id) AND rc.id = sqlc.arg(id)
      AND rc.status = 'pending'
    RETURNING rc.id, rc.household_id, rc.quest_id, rc.member_id, rc.done_at, rc.status
), reopened AS (
    UPDATE household_quests q
    SET available_at = NOW(), updated_at = NOW()
    FROM rejected r
    WHERE q.household_id = r.household_id AND q.id = r.quest_id
      AND q.completed_at = r.done_at AND q.available_at > NOW()
    RETURNING q.id
)
SELECT r.id, r.quest_id, r.member_id, r.done_at, r.status
FROM rejected r;

-- name: RestoreMemberQuestStreak :exec
-- Undoes the streak step a rejected completion made at its mark-done. Only
-- when that completion is the member's latest mark-done of the quest
-- (last_done_at = its done_at): the row goes back to prev_* when there was
-- an earlier streak, and is deleted when that mark-done started it (count 1,
-- nothing before). One step of history is kept: a completion whose step a
-- later mark-done already superseded, or whose earlier step was already
-- undone, leaves the streak alone. Writes only member_quest_streaks.
WITH restored AS (
    UPDATE member_quest_streaks s
    SET count = s.prev_count, last_done_at = s.prev_last_done_at, expires_at = s.prev_expires_at,
        prev_count = NULL, prev_last_done_at = NULL, prev_expires_at = NULL,
        updated_at = NOW()
    WHERE s.household_id = sqlc.arg(household_id) AND s.member_id = sqlc.arg(member_id)
      AND s.quest_id = sqlc.arg(quest_id) AND s.last_done_at = sqlc.arg(done_at)
      AND s.prev_count IS NOT NULL
    RETURNING s.quest_id
)
DELETE FROM member_quest_streaks s
WHERE s.household_id = sqlc.arg(household_id) AND s.member_id = sqlc.arg(member_id)
  AND s.quest_id = sqlc.arg(quest_id) AND s.last_done_at = sqlc.arg(done_at)
  AND s.prev_count IS NULL AND s.count = 1
  AND NOT EXISTS (SELECT 1 FROM restored);
