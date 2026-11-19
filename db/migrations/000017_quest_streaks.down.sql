-- 000017_quest_streaks.down.sql
-- Reverse quest streaks: drop member_quest_streaks and streak_bonus columns.

DROP TABLE IF EXISTS member_quest_streaks;

ALTER TABLE quest_completions
    DROP COLUMN IF EXISTS streak_count;

ALTER TABLE household_quests
    DROP COLUMN IF EXISTS streak_bonus;

ALTER TABLE quest_library
    DROP COLUMN IF EXISTS streak_bonus;

DROP FUNCTION IF EXISTS quest_streak_expires_at(TIMESTAMPTZ, TEXT, INT);
