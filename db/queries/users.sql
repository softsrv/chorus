-- name: CreateUser :one
INSERT INTO users (id, email, display_name, password_hash, email_verified, created_at, updated_at)
VALUES ($1, $2, $3, $4, false, NOW(), NOW())
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1 LIMIT 1;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1 LIMIT 1;

-- name: SetEmailVerified :exec
UPDATE users SET email_verified = true, updated_at = NOW() WHERE id = $1;

-- name: IncrementFailedLoginAttempts :exec
UPDATE users SET failed_login_attempts = failed_login_attempts + 1, updated_at = NOW() WHERE id = $1;

-- name: LockAccount :exec
UPDATE users SET locked_until = $2, updated_at = NOW() WHERE id = $1;

-- name: ResetLoginAttempts :exec
UPDATE users SET failed_login_attempts = 0, locked_until = NULL, updated_at = NOW() WHERE id = $1;

-- name: UpdatePasswordHash :exec
UPDATE users SET password_hash = $2, failed_login_attempts = 0, locked_until = NULL, updated_at = NOW() WHERE id = $1;

-- name: DeleteUserByID :exec
DELETE FROM users WHERE id = $1;

-- name: UpdateDisplayName :one
UPDATE users SET display_name = $2, updated_at = NOW() WHERE id = $1
RETURNING *;

-- name: GetUserForSession :one
-- Loads the user behind an access token, but only while the session (refresh
-- token) it was issued under is still live. This makes logout and session
-- revocation take effect immediately instead of when the access token expires.
SELECT u.* FROM users u
JOIN refresh_tokens rt ON rt.user_id = u.id
WHERE rt.id = sqlc.arg(session_id)
  AND u.id = sqlc.arg(user_id)
  AND rt.revoked_at IS NULL
  AND rt.expires_at > NOW();
