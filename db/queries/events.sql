-- Every event query is scoped by household_id; see chores.sql.

-- name: CreateEvent :one
INSERT INTO events (id, household_id, title, description, location, starts_at, ends_at, all_day, created_by, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW())
RETURNING *;

-- name: GetEvent :one
SELECT * FROM events WHERE household_id = $1 AND id = $2;

-- name: ListEventsInRange :many
-- Events overlapping [range_start, range_end).
SELECT * FROM events
WHERE household_id = sqlc.arg(household_id)
  AND starts_at < sqlc.arg(range_end)
  AND ends_at >= sqlc.arg(range_start)
ORDER BY starts_at ASC, id ASC
LIMIT sqlc.arg(row_limit);

-- name: UpdateEvent :one
UPDATE events
SET title = $3, description = $4, location = $5, starts_at = $6, ends_at = $7, all_day = $8, updated_at = NOW()
WHERE household_id = $1 AND id = $2
RETURNING *;

-- name: DeleteEvent :execrows
DELETE FROM events WHERE household_id = $1 AND id = $2;
