-- name: CreateHousehold :one
INSERT INTO households (id, name, timezone, created_at, updated_at)
VALUES ($1, $2, $3, NOW(), NOW())
RETURNING *;

-- name: GetHouseholdForMember :one
-- Returns the household together with the caller's role. No row means either
-- the household doesn't exist or the user isn't a member; callers treat both
-- as "not found" so household IDs can't be probed.
SELECT h.*, m.role
FROM households h
JOIN household_members m ON m.household_id = h.id
WHERE h.id = $1 AND m.user_id = $2;

-- name: ListHouseholdsForUser :many
SELECT h.*, m.role
FROM households h
JOIN household_members m ON m.household_id = h.id
WHERE m.user_id = $1
ORDER BY h.name, h.id;

-- name: UpdateHousehold :one
UPDATE households SET name = $2, timezone = $3, updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteHousehold :exec
DELETE FROM households WHERE id = $1;

-- name: AddHouseholdMember :exec
INSERT INTO household_members (household_id, user_id, role, joined_at)
VALUES ($1, $2, $3, NOW());

-- name: GetHouseholdMember :one
SELECT * FROM household_members WHERE household_id = $1 AND user_id = $2;

-- name: DeleteSoleMemberHouseholds :exec
-- Deletes every household where the user is the only member.
DELETE FROM households h
WHERE EXISTS (
    SELECT 1 FROM household_members m WHERE m.household_id = h.id AND m.user_id = $1
)
AND NOT EXISTS (
    SELECT 1 FROM household_members o WHERE o.household_id = h.id AND o.user_id <> $1
);
