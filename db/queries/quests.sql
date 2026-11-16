-- Household quest queries are scoped by household_id. Membership is checked
-- by the service layer first; the household_id predicate guarantees a quest ID
-- from another household can never be read or modified through this one.
-- The quest library is global and read-only: there are no write queries on it.

-- name: ListQuestLibrary :many
SELECT * FROM quest_library ORDER BY name ASC, id ASC;

-- name: GetQuestLibraryItem :one
SELECT * FROM quest_library WHERE id = $1;

-- name: ListHouseholdQuests :many
SELECT * FROM household_quests
WHERE household_id = sqlc.arg(household_id)
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: GetHouseholdQuest :one
SELECT * FROM household_quests WHERE household_id = $1 AND id = $2;

-- name: CreateHouseholdQuest :one
INSERT INTO household_quests (id, household_id, library_id, name, points, repeat_days, notes, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
RETURNING *;

-- name: UpdateHouseholdQuest :one
UPDATE household_quests
SET name = $3, points = $4, repeat_days = $5, notes = $6, updated_at = NOW()
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
-- Puts a quest back in the pool. Only its current holder can abandon it, and
-- not while it waits for approval.
UPDATE household_quests
SET claimed_by = NULL, claimed_at = NULL, updated_at = NOW()
WHERE household_id = sqlc.arg(household_id) AND id = sqlc.arg(id)
  AND claimed_by = sqlc.arg(member_id)
  AND done_at IS NULL
RETURNING *;

-- name: ReleaseMemberQuests :exec
-- Returns every quest the member holds to the pool, a quest awaiting approval
-- included. done_at is cleared with the holder, so the done_at-needs-a-holder
-- CHECK holds. No points move.
UPDATE household_quests
SET claimed_by = NULL, claimed_at = NULL, done_at = NULL, updated_at = NOW()
WHERE household_id = sqlc.arg(household_id) AND claimed_by = sqlc.arg(member_id);

-- name: MarkQuestDone :one
-- The holder marks the quest done; it then waits for the administrator.
-- No points move here.
UPDATE household_quests
SET done_at = NOW(), updated_at = NOW()
WHERE household_id = sqlc.arg(household_id) AND id = sqlc.arg(id)
  AND claimed_by = sqlc.arg(member_id)
  AND done_at IS NULL
RETURNING *;

-- name: ListPendingQuests :many
-- Quests waiting for approval, oldest first, with the name of the member who
-- holds each one.
SELECT sqlc.embed(q), m.name AS member_name
FROM household_quests q
JOIN family_members m ON m.household_id = q.household_id AND m.id = q.claimed_by
WHERE q.household_id = sqlc.arg(household_id) AND q.done_at IS NOT NULL
ORDER BY q.done_at ASC, q.id ASC;

-- name: ApproveQuest :one
-- Completes a pending quest. The pending row is locked first, and the
-- done_at check is re-read under the lock, so of two concurrent approvals
-- exactly one gets a row; the households row is read, not locked. The quest
-- comes back at local midnight in the household's timezone, repeat_days days
-- after the local day it was marked done (done_at), however late the approval.
-- credited_member_id is the holder to credit, read before claimed_by is cleared.
WITH pending AS (
    SELECT hq.id, hq.claimed_by, hq.done_at, h.timezone
    FROM household_quests hq
    JOIN households h ON h.id = hq.household_id
    WHERE hq.household_id = sqlc.arg(household_id) AND hq.id = sqlc.arg(id)
      AND hq.done_at IS NOT NULL
    FOR UPDATE OF hq
)
UPDATE household_quests q
SET claimed_by = NULL, claimed_at = NULL, done_at = NULL,
    completed_at = NOW(),
    available_at = (date_trunc('day', p.done_at AT TIME ZONE p.timezone)
        + make_interval(days => q.repeat_days)) AT TIME ZONE p.timezone,
    updated_at = NOW()
FROM pending p
WHERE q.household_id = sqlc.arg(household_id) AND q.id = p.id
RETURNING q.*, p.claimed_by AS credited_member_id;

-- name: RejectQuest :one
-- Sends a pending quest back to its holder: it stays theirs, unfinished.
UPDATE household_quests
SET done_at = NULL, updated_at = NOW()
WHERE household_id = sqlc.arg(household_id) AND id = sqlc.arg(id)
  AND done_at IS NOT NULL
RETURNING *;
