-- 000018_quest_age_range.down.sql
-- Reverse quest age ranges: drop the limits, the birthday and the age rule.
-- Every quest is all ages again and no member has a birthday.

ALTER TABLE household_quests
    DROP CONSTRAINT IF EXISTS household_quests_age_range_check,
    DROP CONSTRAINT IF EXISTS household_quests_max_age_check,
    DROP CONSTRAINT IF EXISTS household_quests_min_age_check,
    DROP COLUMN IF EXISTS max_age,
    DROP COLUMN IF EXISTS min_age;

ALTER TABLE family_members
    DROP COLUMN IF EXISTS birthday;

DROP FUNCTION IF EXISTS quest_age_eligible(DATE, TEXT, INT, INT);
DROP FUNCTION IF EXISTS quest_member_age(DATE, TEXT);
