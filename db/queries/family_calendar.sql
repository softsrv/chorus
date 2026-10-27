-- Every family-calendar query is scoped by household_id; callers check
-- membership with getHousehold first.

-- name: GetHouseholdCalendar :one
SELECT hc.id, hc.household_id, hc.set_by, hc.calendar_id, hc.calendar_name,
       hc.created_at, hc.updated_at, u.display_name AS set_by_name
FROM household_calendars hc
JOIN users u ON u.id = hc.set_by
WHERE hc.household_id = $1;

-- name: UpsertHouseholdCalendar :one
-- Sets the household's family calendar. An existing one is only replaced when
-- it was set by the same member: for anyone else the guarded DO UPDATE
-- matches nothing and no row is returned.
INSERT INTO household_calendars (id, household_id, set_by, calendar_id, calendar_name, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
ON CONFLICT (household_id) DO UPDATE SET
    calendar_id   = EXCLUDED.calendar_id,
    calendar_name = EXCLUDED.calendar_name,
    updated_at    = NOW()
WHERE household_calendars.set_by = EXCLUDED.set_by
RETURNING *;

-- name: DeleteHouseholdCalendar :execrows
-- Only the member who set it can clear it.
DELETE FROM household_calendars WHERE household_id = $1 AND set_by = $2;
