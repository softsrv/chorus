-- 000022_quest_schedules.down.sql
-- Scheduled quests go: every schedule and the rollover marker are dropped.
-- Quests already assigned by a schedule stay held as any accepted quest.
DROP TABLE IF EXISTS household_quest_rollovers;
DROP TABLE IF EXISTS quest_schedules;
