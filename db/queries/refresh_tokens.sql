-- name: InsertRefreshToken :one
INSERT INTO refresh_tokens (
    id, user_id, token_hash,
    device_name, ip_address, user_agent, expires_at, created_at, last_used_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
RETURNING *;

-- name: GetRefreshTokenByHash :one
SELECT * FROM refresh_tokens WHERE token_hash = $1 LIMIT 1;

-- name: GetRefreshTokenByID :one
SELECT * FROM refresh_tokens WHERE id = $1 LIMIT 1;

-- name: RevokeRefreshToken :exec
UPDATE refresh_tokens SET revoked_at = NOW() WHERE id = $1;

-- name: RevokeAllUserRefreshTokens :exec
UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL;

-- name: UpdateRefreshTokenLastUsed :exec
UPDATE refresh_tokens SET last_used_at = NOW(), device_name = $2, ip_address = $3, user_agent = $4 WHERE id = $1;

-- name: ListActiveRefreshTokensByUserID :many
SELECT * FROM refresh_tokens
WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > NOW()
ORDER BY last_used_at DESC;

-- name: DeleteExpiredRefreshTokens :execrows
DELETE FROM refresh_tokens
WHERE (expires_at < NOW() - INTERVAL '90 days')
   OR (revoked_at IS NOT NULL AND revoked_at < NOW() - INTERVAL '90 days');

-- name: RevokeOtherUserRefreshTokens :exec
UPDATE refresh_tokens SET revoked_at = NOW()
WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL;
