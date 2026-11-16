-- 000012_quests_reset_at_midnight.down.sql
-- Restore available_at to completed_at + repeat_days for resting quests.

UPDATE household_quests
SET available_at = completed_at + make_interval(days => repeat_days)
WHERE available_at > NOW()
  AND completed_at IS NOT NULL;
