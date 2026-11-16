-- 000012_quests_reset_at_midnight.up.sql
-- Recalculate available_at for quests currently resting so they reset at household-local midnight.
-- completed_at's local day stands in for the done day since done_at was cleared at approval.

UPDATE household_quests q
SET available_at = (date_trunc('day', q.completed_at AT TIME ZONE h.timezone) + make_interval(days => q.repeat_days)) AT TIME ZONE h.timezone
FROM households h
WHERE h.id = q.household_id
  AND q.available_at > NOW()
  AND q.completed_at IS NOT NULL;
