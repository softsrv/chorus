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

-- name: SetFamilyMemberBirthday :one
-- Sets a member's birthday, or clears it with NULL. Returns it with the
-- member's age today in the household's timezone, from quest_member_age (the
-- one age rule). age is only meaningful when birthday is set: it is 0 for a
-- member with none, and the caller reports no age then. A member id of
-- another household matches no row.
UPDATE family_members m
SET birthday = sqlc.narg(birthday)::date, updated_at = NOW()
FROM households h
WHERE m.household_id = sqlc.arg(household_id) AND m.id = sqlc.arg(id)
  AND h.id = m.household_id
RETURNING m.id, m.birthday, COALESCE(quest_member_age(m.birthday, h.timezone), 0)::int AS age;

-- name: ListFamilyMemberBirthdays :many
-- Every member of the household with their birthday and their age today in
-- the household's timezone (quest_member_age). As above, age is 0 and not
-- meaningful for a member with no birthday.
SELECT m.id, m.birthday, COALESCE(quest_member_age(m.birthday, h.timezone), 0)::int AS age
FROM family_members m
JOIN households h ON h.id = m.household_id
WHERE m.household_id = sqlc.arg(household_id)
ORDER BY m.name, m.id;

-- name: LockFamilyMember :one
-- Locks the member row for the rest of the transaction. The lock conflicts
-- with the key-share lock a foreign-key check takes, so rows that would point
-- at this member wait until the transaction ends.
SELECT id FROM family_members
WHERE household_id = $1 AND id = $2
FOR UPDATE;

-- name: DeleteFamilyMember :execrows
DELETE FROM family_members
WHERE household_id = $1 AND id = $2;

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
