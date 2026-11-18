-- 000018_quest_age_range.up.sql
-- Quest age ranges: an optional minimum and maximum age on each quest and an
-- optional birthday on each family member. Expand-only: every existing quest
-- gets NULL limits (all ages) and every existing member a NULL birthday (can
-- do every quest), so no existing accept or share changes.

-- 1. The age rule, defined once. Every query that needs a member's age or
--    whether they may take a quest calls these; nothing else restates the
--    arithmetic.
--
--    quest_member_age: whole years from birthday to today's date in tz, so the
--    age goes up on the birthday itself (local midnight in tz). NULL when the
--    birthday is NULL. STABLE, not IMMUTABLE: it reads today's date.
CREATE FUNCTION quest_member_age(birthday DATE, tz TEXT)
RETURNS INT
LANGUAGE sql STABLE STRICT PARALLEL SAFE
AS $$
    SELECT date_part('year', age((now() AT TIME ZONE tz)::date, birthday))::int
$$;

--    quest_age_eligible: true when the member has no birthday, or their age
--    is within the quest's limits, both inclusive; a NULL limit is no limit
--    on that side.
CREATE FUNCTION quest_age_eligible(birthday DATE, tz TEXT, min_age INT, max_age INT)
RETURNS BOOLEAN
LANGUAGE sql STABLE PARALLEL SAFE
AS $$
    SELECT birthday IS NULL
        OR ((min_age IS NULL OR quest_member_age(birthday, tz) >= min_age)
            AND (max_age IS NULL OR quest_member_age(birthday, tz) <= max_age))
$$;

-- 2. A member's birthday; NULL when not set.
ALTER TABLE family_members
    ADD COLUMN birthday DATE;

-- 3. A quest's age limits; NULL means no limit on that side.
ALTER TABLE household_quests
    ADD COLUMN min_age INT,
    ADD COLUMN max_age INT,
    ADD CONSTRAINT household_quests_min_age_check CHECK (min_age >= 0),
    ADD CONSTRAINT household_quests_max_age_check CHECK (max_age >= 0),
    ADD CONSTRAINT household_quests_age_range_check
        CHECK (min_age IS NULL OR max_age IS NULL OR min_age <= max_age);
