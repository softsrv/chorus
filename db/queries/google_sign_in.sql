-- name: InsertOAuthState :one
INSERT INTO oauth_states (id, state_hash, purpose, user_id, pkce_verifier, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
RETURNING *;

-- name: ConsumeOAuthState :one
-- Single use: conditional on used_at IS NULL, unexpired, and the flow's
-- purpose, so a state can't be replayed or used to complete another flow.
UPDATE oauth_states SET used_at = NOW()
WHERE state_hash = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > NOW()
RETURNING *;

-- name: DeleteStaleOAuthStates :execrows
DELETE FROM oauth_states
WHERE (used_at IS NOT NULL AND used_at < NOW() - INTERVAL '7 days')
   OR (expires_at < NOW() - INTERVAL '7 days');

-- name: DeleteStaleSignInHandoffs :execrows
DELETE FROM google_sign_in_handoffs
WHERE (used_at IS NOT NULL AND used_at < NOW() - INTERVAL '7 days')
   OR (expires_at < NOW() - INTERVAL '7 days');
