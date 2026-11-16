CREATE TABLE points_awards (
    id           UUID        PRIMARY KEY,
    household_id UUID        NOT NULL,
    member_id    UUID        NOT NULL,
    kind         TEXT        NOT NULL CHECK (kind IN ('quest', 'combo', 'streak')),
    reason       TEXT        NOT NULL,
    points       INT         NOT NULL CHECK (points >= 0),
    earned_at    TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (household_id, member_id)
        REFERENCES family_members (household_id, id)
        ON DELETE CASCADE
);

CREATE INDEX idx_points_awards_member_created
    ON points_awards (household_id, member_id, created_at DESC);
