-- Adds real user authentication (phone + PIN) for the mobile wallet app.
-- Distinct from the admin console's shared token — this is per-user login.

BEGIN;

ALTER TABLE users ADD COLUMN phone TEXT UNIQUE;
ALTER TABLE users ADD COLUMN pin_hash TEXT;
ALTER TABLE users ADD COLUMN pin_salt TEXT;
ALTER TABLE users ADD COLUMN display_name TEXT;
ALTER TABLE users ADD COLUMN created_at_app TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE users ADD COLUMN failed_login_attempts INT NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN locked_until TIMESTAMPTZ;

CREATE INDEX idx_users_phone ON users(phone);

-- Session tokens issued at login. Storing a HASH of the token (never the
-- token itself), same principle as the admin console's auth token — if the
-- DB leaks, sessions can't be replayed directly.
CREATE TABLE sessions (
    token_hash   TEXT PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE INDEX idx_sessions_expiry ON sessions(expires_at);

COMMIT;
