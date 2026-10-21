CREATE TABLE households (
    id         UUID        PRIMARY KEY,
    name       TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    -- IANA zone name; anchors date-only values (chore due dates, all-day events).
    timezone   TEXT        NOT NULL DEFAULT 'UTC',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE household_members (
    household_id UUID        NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role         TEXT        NOT NULL CHECK (role IN ('admin', 'member')),
    joined_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (household_id, user_id)
);

CREATE INDEX idx_household_members_user_id ON household_members (user_id);

-- Single-use invite codes. Only the SHA-256 hash is stored.
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
