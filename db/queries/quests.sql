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
-- participants: the members on the quest when it is shared, earliest first.
SELECT sqlc.embed(q), EXISTS (
    SELECT 1 FROM quest_completions c
    WHERE c.household_id = q.household_id AND c.quest_id = q.id AND c.status = 'pending'
) AS awaiting_approval,
ARRAY(
    SELECT p.member_id FROM household_quest_participants p
    WHERE p.household_id = q.household_id AND p.quest_id = q.id
    ORDER BY p.joined_at, p.member_id
)::uuid[] AS participants
FROM household_quests q
WHERE q.household_id = sqlc.arg(household_id)
ORDER BY q.created_at ASC, q.id ASC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: GetHouseholdQuest :one
SELECT * FROM household_quests WHERE household_id = $1 AND id = $2;

-- name: GetHouseholdQuestView :one
-- One quest with awaiting_approval and participants, as ListHouseholdQuests
-- reads it.
SELECT sqlc.embed(q), EXISTS (
    SELECT 1 FROM quest_completions c
    WHERE c.household_id = q.household_id AND c.quest_id = q.id AND c.status = 'pending'
) AS awaiting_approval,
ARRAY(
    SELECT p.member_id FROM household_quest_participants p
    WHERE p.household_id = q.household_id AND p.quest_id = q.id
    ORDER BY p.joined_at, p.member_id
)::uuid[] AS participants
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
INSERT INTO household_quests (id, household_id, library_id, name, points, repeat_days, notes, shareable, teamwork_bonus, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW())
RETURNING *;

-- name: UpdateHouseholdQuest :one
-- Full replacement of the editable fields. A new points, shareable or
-- teamwork_bonus applies from the next approval: a pending completion is
-- paid from the quest as it stands when it is approved.
UPDATE household_quests
SET name = $3, points = $4, repeat_days = $5, notes = $6,
    shareable = $7, teamwork_bonus = $8, updated_at = NOW()
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

-- name: ShareQuest :many
-- The holder of a shareable quest adds another member of the household to
-- it. One statement: the quest must be in this household, shareable and held
-- by the acting member, and the target must be a household member other than
-- the actor who is not on the quest yet; otherwise nothing is written. The
-- first share also records the holder, joined at the time they accepted, so
-- the holder stays the earliest member. Returns the member ids it added.
WITH quest AS (
    SELECT q.household_id, q.id, q.claimed_at
    FROM household_quests q
    WHERE q.household_id = sqlc.arg(household_id) AND q.id = sqlc.arg(id)
      AND q.shareable AND q.claimed_by = sqlc.arg(member_id)::uuid
    -- Locked and re-checked, so a share racing the holder's mark-done or
    -- abandon either sees the quest still held or writes nothing.
    FOR UPDATE OF q
), target AS (
    SELECT m.id
    FROM family_members m
    WHERE m.household_id = sqlc.arg(household_id) AND m.id = sqlc.arg(target_member_id)::uuid
      AND m.id <> sqlc.arg(member_id)::uuid
      AND NOT EXISTS (
          SELECT 1 FROM household_quest_participants p
          WHERE p.household_id = sqlc.arg(household_id) AND p.quest_id = sqlc.arg(id)
            AND p.member_id = m.id
      )
)
INSERT INTO household_quest_participants (household_id, quest_id, member_id, joined_at)
SELECT q.household_id, q.id, v.member_id, v.joined_at
FROM quest q
CROSS JOIN target t
CROSS JOIN LATERAL (VALUES
    (sqlc.arg(member_id)::uuid, COALESCE(q.claimed_at, NOW())),
    (t.id, NOW())
) AS v(member_id, joined_at)
ON CONFLICT (quest_id, member_id) DO NOTHING
RETURNING member_id;

-- name: ListQuestParticipants :many
-- The members on one quest, earliest first; none when it is not shared.
SELECT member_id FROM household_quest_participants
WHERE household_id = sqlc.arg(household_id) AND quest_id = sqlc.arg(quest_id)
ORDER BY joined_at ASC, member_id ASC;

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
    RETURNING q.*
), recorded AS (
    INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at, status)
    SELECT sqlc.arg(completion_id)::uuid, d.household_id, d.id, sqlc.arg(member_id)::uuid, NOW(), 'pending'
    FROM done d
    RETURNING id
)
SELECT d.id, d.household_id, d.library_id, d.name, d.points, d.repeat_days, d.notes,
       d.claimed_by, d.claimed_at, d.done_at, d.completed_at, d.available_at,
       d.created_at, d.updated_at, d.shareable, d.teamwork_bonus
FROM done d;

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
-- mark-done. points is the chore's current points, to credit to member_id.
-- member_id must match the member the caller already locked.
WITH pending AS (
    SELECT pc.id, q.points
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
RETURNING c.id, c.quest_id, c.member_id, c.done_at, c.approved_at, p.points;

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
