DROP TABLE IF EXISTS google_sign_in_handoffs;
DROP TABLE IF EXISTS oauth_states;

-- Passwordless (Google-only) accounts cannot exist once password_hash is
-- NOT NULL again, so they are removed before the constraint is restored.
DELETE FROM users WHERE password_hash IS NULL OR password_hash = '';

ALTER TABLE users DROP COLUMN IF EXISTS google_subject;
ALTER TABLE users ALTER COLUMN password_hash SET NOT NULL;
