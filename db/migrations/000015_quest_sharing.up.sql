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
--    is the member who marked it done; if that member is removed while the
--    completion is pending, it moves to the earliest remaining recorded
--    member (it is nulled only when no recorded member is left, and the
--    completion is then cancelled). points_awarded is set when the
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

-- 4. A shared completion counts toward each of its members' combos, so a
--    completion is counted once per combo and member, not once per combo.
--    member_id is the award's member; existing rows take it from their award.
ALTER TABLE combo_award_completions ADD COLUMN member_id UUID;
UPDATE combo_award_completions cac
SET member_id = ca.member_id
FROM combo_awards ca
WHERE ca.id = cac.combo_award_id;
ALTER TABLE combo_award_completions ALTER COLUMN member_id SET NOT NULL;
ALTER TABLE combo_award_completions
    DROP CONSTRAINT uq_combo_award_completions_combo_completion,
    ADD CONSTRAINT uq_combo_award_completions_combo_completion_member
        UNIQUE (combo_id, quest_completion_id, member_id);

-- 5. A pending completion whose marker was removed under 000014 (member_id
--    nulled by ON DELETE SET NULL) records no member at all: nobody could be
--    paid for it, and from here on the Approvals list, approval and member
--    removal reach a completion only through its marker or its recorded
--    members. Each one is cancelled, as member removal does: no points move,
--    and a quest resting on the cooldown that completion started (its
--    completed_at is the completion's done_at, available_at still ahead) is
--    available again right away. Every completion that still has its marker
--    is untouched.
WITH cancelled AS (
    UPDATE quest_completions c
    SET status = 'cancelled'
    WHERE c.status = 'pending' AND c.member_id IS NULL
    RETURNING c.household_id, c.quest_id, c.done_at
)
UPDATE household_quests q
SET available_at = NOW(), updated_at = NOW()
FROM cancelled c
WHERE q.household_id = c.household_id AND q.id = c.quest_id
  AND q.completed_at = c.done_at AND q.available_at > NOW();
