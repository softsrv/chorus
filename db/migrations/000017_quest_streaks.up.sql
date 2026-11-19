-- 000017_quest_streaks.up.sql
-- Quest streaks: member_quest_streaks tracking streak counts and expirations,
-- and streak_bonus column on household_quests and quest_library.

-- 1. Function calculating when a quest streak expires: local midnight
--    starting (local date of done_at + 2*repeat_days + 1 day) in households.timezone.
CREATE FUNCTION quest_streak_expires_at(done_at TIMESTAMPTZ, tz TEXT, repeat_days INT)
RETURNS TIMESTAMPTZ
LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
    SELECT (date_trunc('day', done_at AT TIME ZONE tz) + make_interval(days => 2 * repeat_days + 1)) AT TIME ZONE tz
$$;

-- 2. Create member_quest_streaks
CREATE TABLE member_quest_streaks (
    household_id      UUID        NOT NULL,
    member_id         UUID        NOT NULL,
    quest_id          UUID        NOT NULL,
    count             INT         NOT NULL DEFAULT 1 CHECK (count >= 1),
    last_done_at      TIMESTAMPTZ NOT NULL,
    expires_at        TIMESTAMPTZ NOT NULL,
    prev_count        INT,
    prev_last_done_at TIMESTAMPTZ,
    prev_expires_at   TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (member_id, quest_id),
    FOREIGN KEY (household_id)
        REFERENCES households (id)
        ON DELETE CASCADE,
    FOREIGN KEY (household_id, member_id)
        REFERENCES family_members (household_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (household_id, quest_id)
        REFERENCES household_quests (household_id, id)
        ON DELETE CASCADE
);

CREATE INDEX idx_member_quest_streaks_household ON member_quest_streaks (household_id);

-- 3. Add streak_bonus to household_quests and quest_library
ALTER TABLE household_quests
    ADD COLUMN streak_bonus INT NOT NULL DEFAULT 0 CHECK (streak_bonus >= 0);

ALTER TABLE quest_library
    ADD COLUMN streak_bonus INT NOT NULL DEFAULT 0 CHECK (streak_bonus >= 0);

-- 4. Each completion remembers the member's streak count as of its
--    mark-done, so approving it later (or out of order) pays the bonus the
--    streak had then. NULL for completions marked done before streaks.
ALTER TABLE quest_completions
    ADD COLUMN streak_count INT CHECK (streak_count >= 1);

-- Update seeded library rows: 5 where repeat_days = 1 else 10
UPDATE quest_library
SET streak_bonus = CASE WHEN repeat_days = 1 THEN 5 ELSE 10 END;
