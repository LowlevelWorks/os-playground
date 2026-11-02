-- Login with Google (OIDC). The API is the web app's
-- backend for login: after the OIDC callback it creates a login session and
-- sets a cookie holding a random token. Only the token's SHA-256 is stored,
-- so a copy of this table cannot be used to sign in.
--
-- playground_sessions.owner_id holds users.id from now on. It stays text with
-- no foreign key: the store treats owners as opaque, and an account is
-- erased by anonymising its users row, which keeps every reference valid.
-- Sessions made by anonymous browsers before this migration simply expire.

-- +goose Up

CREATE TABLE login_sessions (
    token_hash bytea PRIMARY KEY CHECK (length(token_hash) = 32),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
-- Sign-out everywhere, and the retention purge of expired rows.
CREATE INDEX login_sessions_by_user ON login_sessions (user_id);
CREATE INDEX login_sessions_by_expiry ON login_sessions (expires_at);

GRANT SELECT, INSERT, DELETE ON login_sessions TO playground_app;

-- +goose Down
-- For resetting a local database only.

DROP TABLE login_sessions;
