-- Household combo queries are scoped by household_id. Membership is checked
-- by the service layer first; the household_id predicate guarantees a combo
-- or chore ID from another household can never be read or modified through this one.

-- name: ListHouseholdCombos :many
SELECT * FROM combos
WHERE household_id = $1
ORDER BY created_at ASC, id ASC;

-- name: GetHouseholdCombo :one
SELECT * FROM combos
WHERE household_id = $1 AND id = $2;

-- name: CreateHouseholdCombo :one
INSERT INTO combos (id, household_id, name, bonus_points, created_at, updated_at)
VALUES ($1, $2, $3, $4, NOW(), NOW())
RETURNING *;

-- name: UpdateHouseholdCombo :one
UPDATE combos
SET name = $3, bonus_points = $4, updated_at = NOW()
WHERE household_id = $1 AND id = $2
RETURNING *;

-- name: DeleteHouseholdCombo :execrows
DELETE FROM combos
WHERE household_id = $1 AND id = $2;

-- name: ListComboQuests :many
SELECT quest_id FROM combo_quests
WHERE household_id = $1 AND combo_id = $2
ORDER BY quest_id ASC;

-- name: ListHouseholdComboQuests :many
SELECT combo_id, quest_id FROM combo_quests
WHERE household_id = $1
ORDER BY combo_id ASC, quest_id ASC;

-- name: DeleteComboQuests :exec
DELETE FROM combo_quests
WHERE household_id = $1 AND combo_id = $2;

-- name: InsertComboQuest :exec
INSERT INTO combo_quests (household_id, combo_id, quest_id)
VALUES ($1, $2, $3);

-- name: CountHouseholdQuestsByIDs :one
SELECT count(*) FROM household_quests
WHERE household_id = $1 AND id = ANY(sqlc.arg(quest_ids)::uuid[]);

-- name: InsertQuestCompletion :one
-- Records one approved completion: who did the chore and when they marked it
-- done. Combos are judged from these rows.
INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: LockCombosForQuest :many
-- Locks, in one statement and in id order, every combo of the household that
-- contains the quest. Concurrent approvals touching the same combo serialize
-- here, so the achievement check that follows sees the other's completion.
SELECT c.id FROM combos c
WHERE c.household_id = sqlc.arg(household_id)
  AND EXISTS (
      SELECT 1 FROM combo_quests cq
      WHERE cq.household_id = c.household_id AND cq.combo_id = c.id
        AND cq.quest_id = sqlc.arg(quest_id)
  )
ORDER BY c.id
FOR UPDATE OF c;

-- name: ListCombosCompletedByMemberOnDay :many
-- The combos containing the quest that the member has just finished: the
-- combo still has two or more chores, and every one of them has a completion
-- by the member, not yet counted toward this combo, whose done_at falls on the
-- same household-local day (households.timezone) as done_at. award_day is
-- that day; chore_count is how many completions paying it will claim.
WITH day AS (
    SELECT h.timezone,
           (sqlc.arg(done_at)::timestamptz AT TIME ZONE h.timezone)::date AS local_day
    FROM households h
    WHERE h.id = sqlc.arg(household_id)
)
SELECT c.id, c.name, c.bonus_points, d.local_day::date AS award_day,
       (SELECT count(*) FROM combo_quests n
        WHERE n.household_id = c.household_id AND n.combo_id = c.id)::int AS chore_count
FROM combos c
CROSS JOIN day d
WHERE c.household_id = sqlc.arg(household_id)
  AND EXISTS (
      SELECT 1 FROM combo_quests cq
      WHERE cq.household_id = c.household_id AND cq.combo_id = c.id
        AND cq.quest_id = sqlc.arg(quest_id)
  )
  AND (SELECT count(*) FROM combo_quests n
       WHERE n.household_id = c.household_id AND n.combo_id = c.id) >= 2
  AND NOT EXISTS (
      SELECT 1 FROM combo_quests cq
      WHERE cq.household_id = c.household_id AND cq.combo_id = c.id
        AND NOT EXISTS (
            SELECT 1 FROM quest_completions qc
            WHERE qc.household_id = c.household_id
              AND qc.quest_id = cq.quest_id
              AND qc.member_id = sqlc.arg(member_id)
              AND (qc.done_at AT TIME ZONE d.timezone)::date = d.local_day
              AND NOT EXISTS (
                  SELECT 1 FROM combo_award_completions cac
                  WHERE cac.combo_id = c.id AND cac.quest_completion_id = qc.id
              )
        )
  )
ORDER BY c.id;

-- name: InsertComboAward :one
-- One paid bonus. combo_name and points are snapshots, so a later edit or
-- delete of the combo leaves the award as it was paid.
INSERT INTO combo_awards (id, household_id, combo_id, member_id, combo_name, points, award_day)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ClaimComboCompletions :execrows
-- Counts one completion per chore of the combo toward the award: for each
-- chore, the member's earliest completion on award_day (household-local) not
-- yet counted toward this combo. No ON CONFLICT: a completion counted twice
-- toward one combo is a unique violation that rolls the approval back.
INSERT INTO combo_award_completions (combo_award_id, combo_id, quest_completion_id)
SELECT sqlc.arg(combo_award_id)::uuid, cq.combo_id, pick.id
FROM combo_quests cq
JOIN households h ON h.id = cq.household_id
CROSS JOIN LATERAL (
    SELECT qc.id FROM quest_completions qc
    WHERE qc.household_id = cq.household_id
      AND qc.quest_id = cq.quest_id
      AND qc.member_id = sqlc.arg(member_id)
      AND (qc.done_at AT TIME ZONE h.timezone)::date = sqlc.arg(award_day)::date
      AND NOT EXISTS (
          SELECT 1 FROM combo_award_completions cac
          WHERE cac.combo_id = cq.combo_id AND cac.quest_completion_id = qc.id
      )
    ORDER BY qc.done_at ASC, qc.id ASC
    LIMIT 1
) pick
WHERE cq.household_id = sqlc.arg(household_id) AND cq.combo_id = sqlc.arg(combo_id);
