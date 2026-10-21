CREATE TABLE chores (
    id           UUID        PRIMARY KEY,
    household_id UUID        NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    title        TEXT        NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    description  TEXT        NOT NULL DEFAULT '' CHECK (char_length(description) <= 2000),
    assignee_id  UUID,
    due_on       DATE,
    completed_at TIMESTAMPTZ,
    completed_by UUID        REFERENCES users(id) ON DELETE SET NULL,
    created_by   UUID        REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- The assignee must be a member of the same household. When they leave,
    -- the chore becomes unassigned rather than pointing outside the household.
    FOREIGN KEY (household_id, assignee_id)
        REFERENCES household_members (household_id, user_id)
        ON DELETE SET NULL (assignee_id)
);

CREATE INDEX idx_chores_household_due ON chores (household_id, due_on);
CREATE INDEX idx_chores_assignee      ON chores (household_id, assignee_id);
