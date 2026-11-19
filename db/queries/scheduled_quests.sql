-- Scheduled quests: the admin's one-off schedules and the per-household
-- rollover that assigns them at the household's local midnight. Every
-- statement that reads or writes a household's rows is keyed by its
-- household_id.

-- name: CreateQuestSchedule :one
-- Stores a schedule of one of the household's quests for one of its
-- members. The quest and the member are read only from this household: a
-- quest or member of another household (or none) matches no row and nothing
-- is written. Returns the schedule with the quest's and member's names.
WITH ins AS (
    INSERT INTO quest_schedules (id, household_id, quest_id, member_id, start_date)
    SELECT sqlc.arg(id)::uuid, q.household_id, q.id, m.id, sqlc.arg(start_date)::date
    FROM household_quests q
    JOIN family_members m ON m.household_id = q.household_id
    WHERE q.household_id = sqlc.arg(household_id)::uuid AND q.id = sqlc.arg(quest_id)::uuid
      AND m.id = sqlc.arg(member_id)::uuid
    RETURNING id, household_id, quest_id, member_id, start_date, created_at
)
SELECT ins.id, ins.household_id, ins.quest_id, ins.member_id, ins.start_date, ins.created_at,
       q.name AS quest_name, m.name AS member_name
FROM ins
JOIN household_quests q ON q.household_id = ins.household_id AND q.id = ins.quest_id
JOIN family_members m ON m.household_id = ins.household_id AND m.id = ins.member_id;

-- name: ListQuestSchedules :many
-- The household's schedules, soonest date first, with the quest's and
-- member's names.
SELECT s.id, s.household_id, s.quest_id, s.member_id, s.start_date, s.created_at,
       q.name AS quest_name, m.name AS member_name
FROM quest_schedules s
JOIN household_quests q ON q.household_id = s.household_id AND q.id = s.quest_id
JOIN family_members m ON m.household_id = s.household_id AND m.id = s.member_id
WHERE s.household_id = sqlc.arg(household_id)
ORDER BY s.start_date, s.created_at, s.id;

-- name: DeleteQuestSchedule :execrows
DELETE FROM quest_schedules
WHERE household_id = sqlc.arg(household_id) AND id = sqlc.arg(id);

-- name: ListHouseholdsDueForRollover :many
-- Households, after after_id in id order and at most max_rows, that have a
-- schedule still due at now: dated on or before the household's local date
-- at now (households.timezone) and after its last rollover date (any date,
-- when it has never rolled over). A household with nothing due is left to
-- the expiry sweep alone.
SELECT h.id
FROM households h
LEFT JOIN household_quest_rollovers r ON r.household_id = h.id
WHERE h.id > sqlc.arg(after_id)::uuid
  AND EXISTS (
      SELECT 1 FROM quest_schedules s
      WHERE s.household_id = h.id
        AND s.start_date <= (sqlc.arg(now)::timestamptz AT TIME ZONE h.timezone)::date
        AND (r.last_rollover_date IS NULL OR s.start_date > r.last_rollover_date)
  )
ORDER BY h.id
LIMIT sqlc.arg(max_rows);

-- name: LockHouseholdForRollover :one
-- Locks the household row for the rest of the transaction, so one rollover
-- (or one schedule created for today) of a household runs at a time. FOR NO
-- KEY UPDATE leaves foreign-key checks against the household unblocked.
SELECT id FROM households
WHERE id = sqlc.arg(household_id)
FOR NO KEY UPDATE;

-- name: GetHouseholdRolloverState :one
-- The household's local date at now and the last date it rolled over (NULL
-- when it never has). Run as a statement of its own after
-- LockHouseholdForRollover, so it reads a rollover that committed while the
-- lock waited.
SELECT (sqlc.arg(now)::timestamptz AT TIME ZONE h.timezone)::date AS local_date,
       r.last_rollover_date
FROM households h
LEFT JOIN household_quest_rollovers r ON r.household_id = h.id
WHERE h.id = sqlc.arg(household_id);

-- name: LockHouseholdMembers :many
-- Locks every member row of the household, in id order (the order
-- lockQuestMembers takes them), before any quest row: member first, as
-- Accept and FamilyService.Remove lock.
SELECT id FROM family_members
WHERE household_id = sqlc.arg(household_id)
ORDER BY id
FOR UPDATE;

-- name: AssignDueSchedules :many
-- Assigns the household's due schedules at now, under ClaimQuest's rules: the
-- quest is not held (claimed_by IS NULL), not resting (available_at at or
-- before now) and the member is within its age range (quest_age_eligible).
-- Due is dated on or before local_date and after after_date (any date when
-- NULL); schedule_id, when set, narrows it to that one schedule. Of several
-- schedules of one quest the earliest eligible one wins. The quest is left as
-- Accept leaves it: claimed_by and claimed_at set, claim_expires_at the local
-- midnight ending the day of now. Returns the quests assigned.
WITH due AS (
    SELECT DISTINCT ON (s.quest_id) s.quest_id, s.member_id
    FROM quest_schedules s
    JOIN household_quests q ON q.household_id = s.household_id AND q.id = s.quest_id
    JOIN family_members m ON m.household_id = s.household_id AND m.id = s.member_id
    JOIN households h ON h.id = s.household_id
    WHERE s.household_id = sqlc.arg(household_id)::uuid
      AND s.start_date <= sqlc.arg(local_date)::date
      AND (sqlc.narg(after_date)::date IS NULL OR s.start_date > sqlc.narg(after_date)::date)
      AND (sqlc.narg(schedule_id)::uuid IS NULL OR s.id = sqlc.narg(schedule_id)::uuid)
      AND q.claimed_by IS NULL
      AND (q.available_at IS NULL OR q.available_at <= sqlc.arg(now)::timestamptz)
      AND quest_age_eligible(m.birthday, h.timezone, q.min_age, q.max_age)
    ORDER BY s.quest_id, s.start_date, s.created_at, s.id
)
UPDATE household_quests q
SET claimed_by = d.member_id, claimed_at = sqlc.arg(now)::timestamptz, updated_at = NOW(),
    claim_expires_at = (
        SELECT quest_claim_expires_at(sqlc.arg(now)::timestamptz, h.timezone)
        FROM households h WHERE h.id = q.household_id
    )
FROM due d
WHERE q.household_id = sqlc.arg(household_id)::uuid AND q.id = d.quest_id
  AND q.claimed_by IS NULL
  AND (q.available_at IS NULL OR q.available_at <= sqlc.arg(now)::timestamptz)
RETURNING q.id, q.claimed_by;

-- name: DeleteFinishedSchedules :execrows
-- Ends the one-off schedules AssignDueSchedules just judged (same window and
-- schedule_id): those assigned and those skipped because the quest was held
-- or resting. A schedule skipped because the member is outside the quest's
-- age range is kept for the admin to see; it is never due again, since its
-- date is now at or before the household's last rollover date.
DELETE FROM quest_schedules s
USING household_quests q, family_members m, households h
WHERE s.household_id = sqlc.arg(household_id)::uuid
  AND q.household_id = s.household_id AND q.id = s.quest_id
  AND m.household_id = s.household_id AND m.id = s.member_id
  AND h.id = s.household_id
  AND s.start_date <= sqlc.arg(local_date)::date
  AND (sqlc.narg(after_date)::date IS NULL OR s.start_date > sqlc.narg(after_date)::date)
  AND (sqlc.narg(schedule_id)::uuid IS NULL OR s.id = sqlc.narg(schedule_id)::uuid)
  AND quest_age_eligible(m.birthday, h.timezone, q.min_age, q.max_age);

-- name: MarkHouseholdRolledOver :exec
-- Records that the household's schedules up to rollover_date are done. It
-- never moves the date back.
INSERT INTO household_quest_rollovers (household_id, last_rollover_date)
VALUES (sqlc.arg(household_id), sqlc.arg(rollover_date)::date)
ON CONFLICT (household_id) DO UPDATE
SET last_rollover_date = GREATEST(household_quest_rollovers.last_rollover_date, EXCLUDED.last_rollover_date);
