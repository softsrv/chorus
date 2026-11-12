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
