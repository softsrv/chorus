-- 000023_quest_schedule_repeat.up.sql
-- Expand only: a schedule can be marked as repeating. Existing rows are
-- one-off schedules, as before.
ALTER TABLE quest_schedules
    ADD COLUMN repeat BOOLEAN NOT NULL DEFAULT FALSE;
