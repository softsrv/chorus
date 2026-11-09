-- Household reward queries are scoped by household_id. Membership is checked
-- by the service layer first; the household_id predicate guarantees a reward
-- ID from another household can never be read or modified through this one.
-- The reward library is global and read-only: there are no write queries on
-- it. No query here touches family_members, so no reward operation can change
-- anyone's points.

-- name: ListRewardLibrary :many
SELECT * FROM reward_library ORDER BY cost ASC, description ASC, id ASC;

-- name: GetRewardLibraryItem :one
SELECT * FROM reward_library WHERE id = $1;

-- name: ListHouseholdRewards :many
SELECT * FROM household_rewards
WHERE household_id = $1
ORDER BY created_at ASC, id ASC;

-- name: GetHouseholdReward :one
SELECT * FROM household_rewards WHERE household_id = $1 AND id = $2;

-- name: CreateHouseholdReward :one
INSERT INTO household_rewards (id, household_id, library_id, description, cost, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
RETURNING *;

-- name: UpdateHouseholdReward :one
UPDATE household_rewards
SET description = $3, cost = $4, updated_at = NOW()
WHERE household_id = $1 AND id = $2
RETURNING *;

-- name: DeleteHouseholdReward :execrows
DELETE FROM household_rewards WHERE household_id = $1 AND id = $2;
