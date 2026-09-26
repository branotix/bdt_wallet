-- Passkey (WebAuthn) support. Additive only — phone+PIN login (migrate-003)
-- keeps working exactly as before. A user may have zero, one, or several
-- registered passkeys (e.g. one per device); PIN remains valid regardless,
-- so a lost/broken passkey can never lock someone out of their own funds.

BEGIN;

CREATE TABLE webauthn_credentials (
    id                BIGSERIAL PRIMARY KEY,
    user_id           BIGINT NOT NULL REFERENCES users(id),
    credential_id     BYTEA NOT NULL UNIQUE,   -- opaque handle from the authenticator
    public_key        BYTEA NOT NULL,          -- COSE-encoded public key, never a secret
    sign_count        BIGINT NOT NULL DEFAULT 0, -- clone-detection counter (see auth/webauthn.go)
    transports        TEXT,                     -- e.g. "internal,hybrid", informational only
    device_label      TEXT,                     -- "iPhone", "Pixel 8" — whatever the client sends
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at      TIMESTAMPTZ
);
CREATE INDEX idx_webauthn_user ON webauthn_credentials(user_id);

COMMIT;
