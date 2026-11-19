-- Recreate tables dropped in 000021_drop_google_redirect_state.up.sql
-- Matching the definitions from 000005_google_sign_in.up.sql

CREATE TABLE oauth_states (
    id            UUID        PRIMARY KEY,
    state_hash    TEXT        NOT NULL UNIQUE,
    purpose       TEXT        NOT NULL CHECK (purpose IN ('sign_in', 'calendar')),
    user_id       UUID        REFERENCES users(id) ON DELETE CASCADE,
    pkce_verifier TEXT        NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    used_at       TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_oauth_states_expires_at ON oauth_states (expires_at);

CREATE TABLE google_sign_in_handoffs (
    id         UUID        PRIMARY KEY,
    code_hash  TEXT        NOT NULL UNIQUE,
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_gsih_expires_at ON google_sign_in_handoffs (expires_at);
