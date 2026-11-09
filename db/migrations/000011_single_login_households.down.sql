-- 000011_single_login_households.down.sql
-- Reverse conversion of single-login households.
-- Non-destructive: leaves users.household_provisioned_at untouched.

-- 1. Drop the unique single-login index.
DROP INDEX IF EXISTS household_members_one_login;

-- 2. Restore retired memberships back into household_members.
INSERT INTO household_members (household_id, user_id, role, joined_at)
SELECT household_id, user_id, role, joined_at
FROM household_members_retired
ON CONFLICT (household_id, user_id) DO NOTHING;

-- 3. Delete family members that were created during the up migration.
DELETE FROM family_members
WHERE id IN (
    SELECT md5(m.household_id::text || ':' || m.user_id::text)::uuid
    FROM household_members m
);

-- 4. Re-add chores FK NOT VALID with ON DELETE SET NULL.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'chores_household_id_assignee_id_fkey'
    ) THEN
        ALTER TABLE chores
            ADD CONSTRAINT chores_household_id_assignee_id_fkey
            FOREIGN KEY (household_id, assignee_id)
            REFERENCES household_members (household_id, user_id)
            ON DELETE SET NULL (assignee_id)
            NOT VALID;
    END IF;
END $$;

-- 5. Drop the retired members archive table.
DROP TABLE IF EXISTS household_members_retired;
