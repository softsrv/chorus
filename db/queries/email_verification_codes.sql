-- name: InsertEmailVerificationCode :one
INSERT INTO email_verification_codes (id, user_id, token_hash, expires_at, created_at)
VALUES ($1, $2, $3, $4, NOW())
RETURNING *;

-- name: GetVerificationCodeByTokenHash :one
SELECT * FROM email_verification_codes
WHERE token_hash = $1
LIMIT 1;

-- name: MarkVerificationCodeUsed :execrows
-- Conditional on used_at IS NULL so two concurrent redemptions can't both succeed.
UPDATE email_verification_codes SET used_at = NOW() WHERE id = $1 AND used_at IS NULL;

-- name: CountRecentVerificationCodesByUserID :one
SELECT COUNT(*) FROM email_verification_codes
WHERE user_id = $1 AND created_at > NOW() - INTERVAL '1 hour';

-- name: GetOldestRecentVerificationCode :one
SELECT created_at FROM email_verification_codes
WHERE user_id = $1 AND created_at > NOW() - INTERVAL '1 hour'
ORDER BY created_at ASC
LIMIT 1;

-- name: DeleteStaleVerificationCodes :execrows
DELETE FROM email_verification_codes
WHERE (used_at IS NOT NULL AND used_at < NOW() - INTERVAL '7 days')
   OR (expires_at < NOW() - INTERVAL '1 day');
