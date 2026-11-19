-- 000023_quest_schedules.up.sql
-- Scheduled quests. Expand only: household_quests is not altered and no
-- existing row changes.

-- 1. A one-off schedule: the admin asks that quest_id be assigned to
--    member_id at the start of start_date, a local date in the household's
--    timezone. Both references are composite, so a schedule can only name a
--    quest and a member of its own household, and deleting either removes
--    its schedules.
CREATE TABLE quest_schedules (
    id           UUID        PRIMARY KEY,
    household_id UUID        NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    quest_id     UUID        NOT NULL,
    member_id    UUID        NOT NULL,
    start_date   DATE        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (household_id, quest_id)
        REFERENCES household_quests (household_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (household_id, member_id)
        REFERENCES family_members (household_id, id)
        ON DELETE CASCADE
);

CREATE INDEX quest_schedules_household_start_date_idx
    ON quest_schedules (household_id, start_date);

-- 2. The last household-local date whose schedules the rollover has
--    assigned. A household with no row has never rolled over. Schedules
--    dated after it and up to today are the ones still due, so a rollover
--    missed while the server was down runs once, and a second run of the
--    same day finds nothing to do.
CREATE TABLE household_quest_rollovers (
    household_id       UUID PRIMARY KEY REFERENCES households(id) ON DELETE CASCADE,
    last_rollover_date DATE NOT NULL
);
