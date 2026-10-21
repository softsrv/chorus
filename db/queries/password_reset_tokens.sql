-- name: InsertPasswordResetToken :one
INSERT INTO password_reset_tokens (id, user_id, token_hash, expires_at, created_at)
VALUES ($1, $2, $3, $4, NOW())
RETURNING *;

-- name: GetPasswordResetTokenByHash :one
SELECT * FROM password_reset_tokens WHERE token_hash = $1 LIMIT 1;

-- name: MarkPasswordResetTokenUsed :execrows
-- Conditional on used_at IS NULL so two concurrent redemptions can't both succeed.
UPDATE password_reset_tokens SET used_at = NOW() WHERE id = $1 AND used_at IS NULL;

-- name: CountRecentPasswordResetsByEmail :one
SELECT COUNT(*) FROM password_reset_tokens prt
JOIN users u ON u.id = prt.user_id
WHERE u.email = $1 AND prt.created_at > NOW() - INTERVAL '1 hour';

-- name: DeleteStalePasswordResetTokens :execrows
DELETE FROM password_reset_tokens
WHERE (used_at IS NOT NULL AND used_at < NOW() - INTERVAL '7 days')
   OR (expires_at < NOW() - INTERVAL '7 days');
