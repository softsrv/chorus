-- A household's family calendar: one Google calendar, chosen by the member who
-- connected it, whose entries every member sees. At most one per household.
--
-- Clearing is done by the database alone, never with a Google call:
--   * the (household_id, set_by) FK to household_members clears it when the
--     setter leaves or is removed, or deletes their account (users cascade to
--     household_members);
--   * the set_by FK to google_calendar_connections clears it when the setter
--     disconnects Google (the connection row is deleted).
-- The events table is left untouched.
CREATE TABLE household_calendars (
    id            UUID        PRIMARY KEY,
    household_id  UUID        NOT NULL UNIQUE REFERENCES households(id) ON DELETE CASCADE,
    set_by        UUID        NOT NULL,
    calendar_id   TEXT        NOT NULL CHECK (char_length(calendar_id) BETWEEN 1 AND 1024),
    calendar_name TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (household_id, set_by) REFERENCES household_members(household_id, user_id) ON DELETE CASCADE,
    FOREIGN KEY (set_by) REFERENCES google_calendar_connections(user_id) ON DELETE CASCADE
);

CREATE INDEX idx_household_calendars_set_by ON household_calendars (set_by);
