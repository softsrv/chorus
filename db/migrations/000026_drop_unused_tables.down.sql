-- 000026_drop_unused_tables.down.sql

CREATE TABLE household_invites (
    id           UUID        PRIMARY KEY,
    household_id UUID        NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    token_hash   TEXT        NOT NULL UNIQUE,
    created_by   UUID        REFERENCES users(id) ON DELETE SET NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    used_by      UUID        REFERENCES users(id) ON DELETE SET NULL,
    used_at      TIMESTAMPTZ
);

CREATE INDEX idx_household_invites_household_id ON household_invites (household_id);
CREATE INDEX idx_household_invites_expires_at   ON household_invites (expires_at);

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
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_chores_household_due ON chores (household_id, due_on);
CREATE INDEX idx_chores_assignee      ON chores (household_id, assignee_id);

CREATE TABLE household_members_retired (
    household_id UUID        NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role         TEXT        NOT NULL,
    joined_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (household_id, user_id)
);
