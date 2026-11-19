-- 000023_quest_schedule_repeat.down.sql
-- Repeating schedules become one-off schedules by losing the flag.
ALTER TABLE quest_schedules
    DROP COLUMN IF EXISTS repeat;
