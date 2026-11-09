-- 000011_single_login_households.up.sql
-- Convert multi-login households to single-login (admin-only) households.
-- Expand-before-contract: extra logins become family members, and their
-- household_members rows are archived to household_members_retired before deletion.

-- 1. Drop chores composite FK to household_members so assignee_id is preserved
-- in the chores archive rather than being set to NULL on member deletion.
ALTER TABLE chores DROP CONSTRAINT IF EXISTS chores_household_id_assignee_id_fkey;

-- 2. Convert every login member into a family_members row with 0 points.
-- Deterministic md5 id makes this idempotent with ON CONFLICT DO NOTHING.
INSERT INTO family_members (id, household_id, name, points)
SELECT
    md5(m.household_id::text || ':' || m.user_id::text)::uuid,
    m.household_id,
    u.display_name,
    0
FROM household_members m
JOIN users u ON u.id = m.user_id
ON CONFLICT (id) DO NOTHING;

-- 3. Archive table for retired household memberships.
CREATE TABLE IF NOT EXISTS household_members_retired (
    household_id UUID        NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role         TEXT        NOT NULL,
    joined_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (household_id, user_id)
);

-- 4. Copy all memberships except the kept admin (earliest joined_at, lowest user_id)
-- into household_members_retired.
WITH ranked_members AS (
    SELECT
        household_id,
        user_id,
        role,
        joined_at,
        ROW_NUMBER() OVER (
            PARTITION BY household_id
            ORDER BY
                CASE WHEN role = 'admin' THEN 0 ELSE 1 END,
                joined_at ASC,
                user_id ASC
        ) AS rk
    FROM household_members
)
INSERT INTO household_members_retired (household_id, user_id, role, joined_at)
SELECT household_id, user_id, role, joined_at
FROM ranked_members
WHERE rk > 1
ON CONFLICT (household_id, user_id) DO NOTHING;

-- 5. Delete the retired memberships from household_members.
-- Note: Any family calendar set by a retired member is cleared by the existing
-- household_calendars (household_id, set_by) -> household_members CASCADE foreign key.
DELETE FROM household_members
WHERE (household_id, user_id) IN (
    SELECT household_id, user_id
    FROM (
        SELECT
            household_id,
            user_id,
            ROW_NUMBER() OVER (
                PARTITION BY household_id
                ORDER BY
                    CASE WHEN role = 'admin' THEN 0 ELSE 1 END,
                    joined_at ASC,
                    user_id ASC
            ) AS rk
        FROM household_members
    ) ranked
    WHERE rk > 1
);

-- Ensure the kept single login has admin role.
UPDATE household_members SET role = 'admin' WHERE role <> 'admin';

-- 6. Reset household_provisioned_at to NULL for any users left with no household_members row,
-- so their next verified sign-in automatically provisions their own household.
UPDATE users
SET household_provisioned_at = NULL
WHERE id NOT IN (SELECT user_id FROM household_members);

-- 7. Enforce exactly one login account per household.
CREATE UNIQUE INDEX IF NOT EXISTS household_members_one_login ON household_members (household_id);
