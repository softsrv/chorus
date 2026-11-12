-- Points awards are the history of every points credit. Each row is written
-- by the service layer's creditPoints in the same transaction as the
-- family_members.points update it explains. Every query is scoped by
-- household_id; the service checks membership first.

-- name: InsertPointsAward :one
INSERT INTO points_awards (id, household_id, member_id, kind, reason, points, earned_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
RETURNING *;

-- name: ListMemberPointsAwards :many
SELECT * FROM points_awards
WHERE household_id = $1 AND member_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3 OFFSET $4;
