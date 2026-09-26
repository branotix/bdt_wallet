-- Inbound SMS phone verification. Cost-free: instead of the backend sending
-- an outbound OTP (which costs money per SMS), the user sends a short code
-- FROM their phone TO a fixed number, and an Android device holding that
-- SIM forwards the incoming SMS to our webhook.

BEGIN;

ALTER TABLE users ADD COLUMN phone_verified BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE phone_verifications (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT NOT NULL REFERENCES users(id),
    phone         TEXT NOT NULL,          -- normalized, e.g. "8801XXXXXXXXX"
    code          TEXT NOT NULL,          -- 6 digits
    attempts      INT NOT NULL DEFAULT 0, -- webhook match attempts against this code
    expires_at    TIMESTAMPTZ NOT NULL,
    verified_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_phone_verifications_user ON phone_verifications(user_id);
CREATE INDEX idx_phone_verifications_phone_code ON phone_verifications(phone, code);

-- Rate limit how often a user can request a new code — otherwise nothing
-- stops someone from spamming /verify-phone/start in a loop.
CREATE INDEX idx_phone_verifications_user_created ON phone_verifications(user_id, created_at);

COMMIT;
