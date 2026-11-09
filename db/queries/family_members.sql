-- Every family member query is scoped by household_id. Membership / admin
-- authorization is checked by the service layer first; the household_id
-- predicate guarantees a member ID from another household can never be
-- read or modified through this one.

-- name: ListFamilyMembers :many
SELECT * FROM family_members
WHERE household_id = $1
ORDER BY name, id;

-- name: GetFamilyMember :one
SELECT * FROM family_members
WHERE household_id = $1 AND id = $2;

-- name: CreateFamilyMember :one
INSERT INTO family_members (id, household_id, name, pin_hash, points, created_at, updated_at)
VALUES ($1, $2, $3, $4, 0, NOW(), NOW())
RETURNING *;

-- name: RenameFamilyMember :one
UPDATE family_members
SET name = $3, updated_at = NOW()
WHERE household_id = $1 AND id = $2
RETURNING *;

-- name: SetFamilyMemberPin :one
UPDATE family_members
SET pin_hash = $3, updated_at = NOW()
WHERE household_id = $1 AND id = $2
RETURNING *;

-- name: DeleteFamilyMember :execrows
-- Releases the member's quests in the same statement, so a quest waiting for
-- approval (done_at set) returns to the pool instead of tripping the
-- done_at-needs-a-holder CHECK when the foreign key clears claimed_by.
WITH released AS (
    UPDATE household_quests
    SET claimed_by = NULL, claimed_at = NULL, done_at = NULL, updated_at = NOW()
    WHERE household_id = $1 AND claimed_by = $2
)
DELETE FROM family_members fm
WHERE fm.household_id = $1 AND fm.id = $2;

-- name: AddMemberPoints :one
UPDATE family_members
SET points = points + sqlc.arg(delta), updated_at = NOW()
WHERE household_id = $1 AND id = $2
RETURNING *;

-- name: GetAdminPin :one
SELECT * FROM household_admin_pins
WHERE household_id = $1;

-- name: UpsertAdminPin :exec
INSERT INTO household_admin_pins (household_id, pin_hash, created_at, updated_at)
VALUES ($1, $2, NOW(), NOW())
ON CONFLICT (household_id) DO UPDATE
SET pin_hash = EXCLUDED.pin_hash, updated_at = NOW();

-- name: DeleteAdminPin :execrows
DELETE FROM household_admin_pins
WHERE household_id = $1;
