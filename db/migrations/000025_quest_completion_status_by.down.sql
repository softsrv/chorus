-- 000025_quest_completion_status_by.down.sql
-- Drop the status_by actor column from quest_completions.

ALTER TABLE quest_completions
    DROP COLUMN status_by;
