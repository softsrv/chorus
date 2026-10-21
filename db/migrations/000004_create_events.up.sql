CREATE TABLE events (
    id           UUID        PRIMARY KEY,
    household_id UUID        NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    title        TEXT        NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    description  TEXT        NOT NULL DEFAULT '' CHECK (char_length(description) <= 2000),
    location     TEXT        NOT NULL DEFAULT '' CHECK (char_length(location) <= 200),
    starts_at    TIMESTAMPTZ NOT NULL,
    ends_at      TIMESTAMPTZ NOT NULL,
    all_day      BOOLEAN     NOT NULL DEFAULT false,
    created_by   UUID        REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (ends_at >= starts_at)
);

CREATE INDEX idx_events_household_range ON events (household_id, starts_at, ends_at);
