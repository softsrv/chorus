-- name: UpsertGoogleCalendarConnection :one
-- One connection per user: a reconnect replaces the tokens and access level
-- and clears the broken flag.
INSERT INTO google_calendar_connections
    (id, user_id, access_token_enc, refresh_token_enc, token_expiry, access_level, broken, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, false, NOW(), NOW())
ON CONFLICT (user_id) DO UPDATE SET
    access_token_enc  = EXCLUDED.access_token_enc,
    refresh_token_enc = EXCLUDED.refresh_token_enc,
    token_expiry      = EXCLUDED.token_expiry,
    access_level      = EXCLUDED.access_level,
    broken            = false,
    updated_at        = NOW()
RETURNING *;

-- name: GetGoogleCalendarConnectionByUser :one
SELECT * FROM google_calendar_connections WHERE user_id = $1;

-- name: UpdateGoogleCalendarAccessToken :execrows
UPDATE google_calendar_connections
SET access_token_enc = $2, token_expiry = $3, updated_at = NOW()
WHERE user_id = $1;

-- name: MarkGoogleCalendarConnectionBroken :execrows
UPDATE google_calendar_connections
SET broken = true, updated_at = NOW()
WHERE user_id = $1;

-- name: DeleteGoogleCalendarConnectionByUser :one
-- Returns the deleted row so its token can be revoked at Google afterwards.
DELETE FROM google_calendar_connections WHERE user_id = $1
RETURNING *;
