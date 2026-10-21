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

-- name: ListHouseholdMembers :many
SELECT m.user_id, m.role, m.joined_at, u.display_name
FROM household_members m
JOIN users u ON u.id = m.user_id
WHERE m.household_id = $1
ORDER BY u.display_name, m.user_id;

-- name: LockHouseholdAdmins :many
SELECT user_id FROM household_members
WHERE household_id = $1 AND role = 'admin'
FOR UPDATE;

-- name: UpdateHouseholdMemberRole :execrows
UPDATE household_members SET role = $3 WHERE household_id = $1 AND user_id = $2;

-- name: RemoveHouseholdMember :execrows
DELETE FROM household_members WHERE household_id = $1 AND user_id = $2;

-- name: ListAdminHouseholdsWithOtherMembers :many
-- Households where the user is the only admin but other members remain.
-- Used to block account deletion that would orphan a household.
SELECT h.id, h.name
FROM households h
JOIN household_members m ON m.household_id = h.id AND m.user_id = $1 AND m.role = 'admin'
WHERE NOT EXISTS (
    SELECT 1 FROM household_members a
    WHERE a.household_id = h.id AND a.role = 'admin' AND a.user_id <> $1
)
AND EXISTS (
    SELECT 1 FROM household_members o
    WHERE o.household_id = h.id AND o.user_id <> $1
);

-- name: DeleteSoleMemberHouseholds :exec
-- Deletes every household where the user is the only member.
DELETE FROM households h
WHERE EXISTS (
    SELECT 1 FROM household_members m WHERE m.household_id = h.id AND m.user_id = $1
)
AND NOT EXISTS (
    SELECT 1 FROM household_members o WHERE o.household_id = h.id AND o.user_id <> $1
);

-- name: InsertHouseholdInvite :one
INSERT INTO household_invites (id, household_id, token_hash, created_by, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, NOW())
RETURNING *;

-- name: GetHouseholdInviteByHashForUpdate :one
SELECT * FROM household_invites WHERE token_hash = $1 FOR UPDATE;

-- name: MarkHouseholdInviteUsed :exec
UPDATE household_invites SET used_by = $2, used_at = NOW() WHERE id = $1;

-- name: DeleteStaleHouseholdInvites :execrows
DELETE FROM household_invites
WHERE (used_at IS NOT NULL AND used_at < NOW() - INTERVAL '7 days')
   OR (expires_at < NOW() - INTERVAL '7 days');
