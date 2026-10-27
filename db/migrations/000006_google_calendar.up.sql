-- Google Calendar connection: at most one per user. Tokens are stored only as
-- AES-256-GCM ciphertext (app.TokenCipher); the key never reaches the DB.
-- Deleting the user deletes the connection (and oauth_states already cascade).
CREATE TABLE google_calendar_connections (
    id                UUID        PRIMARY KEY,
    user_id           UUID        NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    access_token_enc  BYTEA       NOT NULL,
    refresh_token_enc BYTEA       NOT NULL,
    token_expiry      TIMESTAMPTZ,
    access_level      TEXT        NOT NULL CHECK (access_level IN ('read_write', 'read_only')),
    broken            BOOLEAN     NOT NULL DEFAULT false,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
