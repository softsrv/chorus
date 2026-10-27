-- Google sign-in: an account may exist without a password (Google-only), and
-- may be linked to one Google account by its stable subject id.
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;
ALTER TABLE users ADD COLUMN google_subject TEXT UNIQUE;

-- One row per OAuth authorization request started by Chorus. Only the SHA-256
-- hash of the state value is stored. Shared by every Google flow; purpose
-- tells them apart so a state minted for one flow can't complete another.
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

-- Short-lived, single-use codes that hand a completed Google sign-in from the
-- callback redirect to the web client, which redeems one for a token pair.
-- Only the SHA-256 hash of the code is stored.
CREATE TABLE google_sign_in_handoffs (
    id         UUID        PRIMARY KEY,
    code_hash  TEXT        NOT NULL UNIQUE,
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_gsih_expires_at ON google_sign_in_handoffs (expires_at);
