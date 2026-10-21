-- Every chore query is scoped by household_id. Membership is checked by the
-- service layer first; the household_id predicate guarantees a chore ID from
-- another household can never be read or modified through this one.

-- name: CreateChore :one
INSERT INTO chores (id, household_id, title, description, assignee_id, due_on, created_by, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
RETURNING *;

-- name: GetChore :one
SELECT * FROM chores WHERE household_id = $1 AND id = $2;

-- name: ListChores :many
SELECT * FROM chores
WHERE household_id = sqlc.arg(household_id)
  AND (sqlc.narg(assignee_id)::uuid IS NULL OR assignee_id = sqlc.narg(assignee_id))
  AND (sqlc.arg(status)::text = 'all'
       OR (sqlc.arg(status) = 'open' AND completed_at IS NULL)
       OR (sqlc.arg(status) = 'done' AND completed_at IS NOT NULL))
ORDER BY due_on ASC NULLS LAST, created_at ASC, id ASC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: UpdateChore :one
UPDATE chores
SET title = $3, description = $4, assignee_id = $5, due_on = $6, updated_at = NOW()
WHERE household_id = $1 AND id = $2
RETURNING *;

-- name: SetChoreCompleted :one
UPDATE chores
SET completed_at = NOW(), completed_by = $3, updated_at = NOW()
WHERE household_id = $1 AND id = $2
RETURNING *;

-- name: SetChoreOpen :one
UPDATE chores
SET completed_at = NULL, completed_by = NULL, updated_at = NOW()
WHERE household_id = $1 AND id = $2
RETURNING *;

-- name: DeleteChore :execrows
DELETE FROM chores WHERE household_id = $1 AND id = $2;
