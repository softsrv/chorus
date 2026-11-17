-- 000015_quest_sharing.up.sql
-- Quest sharing. Expand only: every existing quest becomes not shareable with
-- a teamwork bonus of 0, and no existing quest, completion or point balance
-- changes. A quest that is never shared writes no row to either new table.

-- 1. Whether members may share a quest, and the bonus each member on a shared
--    completion earns on top of their split. A bonus needs shareable.
ALTER TABLE household_quests
    ADD COLUMN shareable      BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN teamwork_bonus INT     NOT NULL DEFAULT 0;
ALTER TABLE household_quests
    ADD CONSTRAINT household_quests_teamwork_bonus_check CHECK (teamwork_bonus >= 0),
    ADD CONSTRAINT household_quests_teamwork_bonus_shareable_check CHECK (shareable OR teamwork_bonus = 0);

-- 2. Every member on a held, shared quest, the holder included once it is
--    shared. household_quests.claimed_by stays the holder of record.
--    joined_at, ties broken by member_id, orders "earliest remaining member".
CREATE TABLE household_quest_participants (
    household_id UUID        NOT NULL,
    quest_id     UUID        NOT NULL,
    member_id    UUID        NOT NULL,
    joined_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (quest_id, member_id),
    FOREIGN KEY (household_id, quest_id)
        REFERENCES household_quests (household_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (household_id, member_id)
        REFERENCES family_members (household_id, id)
        ON DELETE CASCADE
);

CREATE INDEX idx_household_quest_participants_member
    ON household_quest_participants (household_id, member_id);

-- 3. Every member a shared completion covers. quest_completions.member_id
--    stays the member who marked it done. points_awarded is set when the
--    completion is approved. added_at, ties broken by member_id, orders
--    "earliest remaining member".
ALTER TABLE quest_completions
    ADD CONSTRAINT quest_completions_household_id_id_key UNIQUE (household_id, id);

CREATE TABLE quest_completion_members (
    household_id   UUID        NOT NULL,
    completion_id  UUID        NOT NULL,
    member_id      UUID        NOT NULL,
    points_awarded INT         NULL CHECK (points_awarded IS NULL OR points_awarded >= 0),
    added_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (completion_id, member_id),
    FOREIGN KEY (household_id, completion_id)
        REFERENCES quest_completions (household_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (household_id, member_id)
        REFERENCES family_members (household_id, id)
        ON DELETE CASCADE
);

CREATE INDEX idx_quest_completion_members_member
    ON quest_completion_members (household_id, member_id);
