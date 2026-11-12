-- 000012_combos.up.sql
-- Combo bonus feature: combos, combo_quests, quest_completions, combo_awards,
-- and combo_award_completions.
-- Expand only: no existing rows are modified, quest_completions starts empty.

-- 1. Add UNIQUE (household_id, id) on household_quests for composite FKs.
ALTER TABLE household_quests
    ADD CONSTRAINT household_quests_household_id_id_key UNIQUE (household_id, id);

-- 2. Combos table.
CREATE TABLE combos (
    id           UUID        PRIMARY KEY,
    household_id UUID        NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    bonus_points INT         NOT NULL DEFAULT 0 CHECK (bonus_points >= 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (household_id, id)
);

CREATE INDEX idx_combos_household_id ON combos (household_id);

-- 3. Combo quests join table (a combo has >= 2 quests; a quest may belong to multiple combos).
CREATE TABLE combo_quests (
    household_id UUID NOT NULL,
    combo_id     UUID NOT NULL,
    quest_id     UUID NOT NULL,
    PRIMARY KEY (combo_id, quest_id),
    FOREIGN KEY (household_id, combo_id)
        REFERENCES combos (household_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (household_id, quest_id)
        REFERENCES household_quests (household_id, id)
        ON DELETE CASCADE
);

CREATE INDEX idx_combo_quests_household_id ON combo_quests (household_id);

-- 4. Quest completions (log of completed quests for combo evaluation).
CREATE TABLE quest_completions (
    id          UUID        PRIMARY KEY,
    household_id UUID       NOT NULL,
    quest_id    UUID        NOT NULL,
    member_id   UUID        NOT NULL,
    done_at     TIMESTAMPTZ NOT NULL,
    approved_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (household_id, quest_id)
        REFERENCES household_quests (household_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (household_id, member_id)
        REFERENCES family_members (household_id, id)
        ON DELETE CASCADE
);

CREATE INDEX idx_quest_completions_member_day ON quest_completions (household_id, member_id, done_at);

-- 5. Combo awards (snapshot of bonus paid to member on award_day).
CREATE TABLE combo_awards (
    id           UUID        PRIMARY KEY,
    household_id UUID        NOT NULL,
    combo_id     UUID,
    member_id    UUID        NOT NULL,
    combo_name   TEXT        NOT NULL,
    points       INT         NOT NULL CHECK (points >= 0),
    award_day    DATE        NOT NULL,
    awarded_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (household_id, combo_id)
        REFERENCES combos (household_id, id)
        ON DELETE SET NULL (combo_id),
    FOREIGN KEY (household_id, member_id)
        REFERENCES family_members (household_id, id)
        ON DELETE CASCADE
);

CREATE INDEX idx_combo_awards_household_id ON combo_awards (household_id);

-- 6. Claimed completions per combo award.
CREATE TABLE combo_award_completions (
    combo_award_id      UUID NOT NULL REFERENCES combo_awards(id) ON DELETE CASCADE,
    combo_id            UUID NOT NULL,
    quest_completion_id UUID NOT NULL REFERENCES quest_completions(id) ON DELETE CASCADE,
    PRIMARY KEY (combo_award_id, quest_completion_id),
    CONSTRAINT uq_combo_award_completions_combo_completion UNIQUE (combo_id, quest_completion_id)
);
