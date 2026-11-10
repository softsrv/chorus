CREATE TABLE family_members (
    id           UUID        PRIMARY KEY,
    household_id UUID        NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 50),
    pin_hash     TEXT,
    points       INT         NOT NULL DEFAULT 0 CHECK (points >= 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (household_id, id)
);

CREATE INDEX idx_family_members_household_id ON family_members (household_id);

CREATE TABLE household_admin_pins (
    household_id UUID        PRIMARY KEY REFERENCES households(id) ON DELETE CASCADE,
    pin_hash     TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE quest_library (
    id          UUID        PRIMARY KEY,
    name        TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    points      INT         NOT NULL DEFAULT 0 CHECK (points >= 0),
    repeat_days INT         NOT NULL DEFAULT 1 CHECK (repeat_days >= 1),
    notes       TEXT        NOT NULL DEFAULT '' CHECK (char_length(notes) <= 2000),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE reward_library (
    id          UUID        PRIMARY KEY,
    description TEXT        NOT NULL CHECK (char_length(description) BETWEEN 1 AND 200),
    cost        INT         NOT NULL DEFAULT 0 CHECK (cost >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE household_quests (
    id           UUID        PRIMARY KEY,
    household_id UUID        NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    library_id   UUID        REFERENCES quest_library(id) ON DELETE SET NULL,
    name         TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    points       INT         NOT NULL DEFAULT 0 CHECK (points >= 0),
    repeat_days  INT         NOT NULL DEFAULT 1 CHECK (repeat_days >= 1),
    notes        TEXT        NOT NULL DEFAULT '' CHECK (char_length(notes) <= 2000),
    claimed_by   UUID,
    claimed_at   TIMESTAMPTZ,
    done_at      TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    available_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (done_at IS NULL OR claimed_by IS NOT NULL),
    FOREIGN KEY (household_id, claimed_by)
        REFERENCES family_members (household_id, id)
        ON DELETE SET NULL (claimed_by)
);

CREATE INDEX idx_household_quests_household_id ON household_quests (household_id);
CREATE INDEX idx_household_quests_pending      ON household_quests (household_id) WHERE done_at IS NOT NULL;
CREATE INDEX idx_household_quests_claimed      ON household_quests (household_id, claimed_by);

CREATE TABLE household_rewards (
    id           UUID        PRIMARY KEY,
    household_id UUID        NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    library_id   UUID        REFERENCES reward_library(id) ON DELETE SET NULL,
    description  TEXT        NOT NULL CHECK (char_length(description) BETWEEN 1 AND 200),
    cost         INT         NOT NULL DEFAULT 0 CHECK (cost >= 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_household_rewards_household_id ON household_rewards (household_id);

-- Seed library rows (global, fixed UUIDs, ON CONFLICT DO NOTHING)
INSERT INTO quest_library (id, name, points, repeat_days, notes) VALUES
    ('00000000-0000-4000-8000-000000000001', 'Make your bed', 5, 1, ''),
    ('00000000-0000-4000-8000-000000000002', 'Feed the pet', 5, 1, ''),
    ('00000000-0000-4000-8000-000000000003', 'Set the table', 5, 1, ''),
    ('00000000-0000-4000-8000-000000000004', 'Clear the table', 5, 1, ''),
    ('00000000-0000-4000-8000-000000000005', 'Load the dishwasher', 10, 1, ''),
    ('00000000-0000-4000-8000-000000000006', 'Unload the dishwasher', 10, 1, ''),
    ('00000000-0000-4000-8000-000000000007', 'Take out the trash', 10, 3, ''),
    ('00000000-0000-4000-8000-000000000008', 'Tidy your room', 15, 7, ''),
    ('00000000-0000-4000-8000-000000000009', 'Vacuum the living room', 20, 7, ''),
    ('00000000-0000-4000-8000-00000000000a', 'Clean the bathroom', 30, 7, ''),
    ('00000000-0000-4000-8000-00000000000b', 'Do a load of laundry', 20, 7, ''),
    ('00000000-0000-4000-8000-00000000000c', 'Water the plants', 5, 3, ''),
    ('00000000-0000-4000-8000-00000000000d', 'Mow the lawn', 50, 14, ''),
    ('00000000-0000-4000-8000-00000000000e', 'Wash the car', 40, 30, '')
ON CONFLICT (id) DO NOTHING;

INSERT INTO reward_library (id, description, cost) VALUES
    ('00000000-0000-4000-8000-000000000011', 'Choose the game on family game night', 200),
    ('00000000-0000-4000-8000-000000000012', 'Pick dinner for one night', 300),
    ('00000000-0000-4000-8000-000000000013', '30 minutes extra screen time', 150),
    ('00000000-0000-4000-8000-000000000014', 'Stay up 30 minutes late', 250),
    ('00000000-0000-4000-8000-000000000015', 'Movie night pick', 300),
    ('00000000-0000-4000-8000-000000000016', '$10', 1000)
ON CONFLICT (id) DO NOTHING;
