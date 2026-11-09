ALTER TABLE users ADD COLUMN household_provisioned_at TIMESTAMPTZ NULL;

-- Backfill existing users who already belong to a household so they are not auto-provisioned.
UPDATE users SET household_provisioned_at = NOW()
WHERE id IN (SELECT user_id FROM household_members);
