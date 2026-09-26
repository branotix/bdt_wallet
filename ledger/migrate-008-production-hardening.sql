-- Migration 008 — production hardening.
-- Run after migrations 002..007.
BEGIN;

ALTER TABLE withdrawals ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS ux_withdrawals_user_idempotency
    ON withdrawals(user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
ALTER TABLE withdrawals DROP CONSTRAINT IF EXISTS withdrawals_fee_nonnegative;
ALTER TABLE withdrawals ADD CONSTRAINT withdrawals_fee_nonnegative CHECK (fee >= 0);
ALTER TABLE withdrawals DROP CONSTRAINT IF EXISTS withdrawals_address_format;
ALTER TABLE withdrawals ADD CONSTRAINT withdrawals_address_format
    CHECK (to_address ~ '^0x[0-9a-fA-F]{40}$');
CREATE UNIQUE INDEX IF NOT EXISTS ux_withdrawals_tx_hash
    ON withdrawals(tx_hash) WHERE tx_hash IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_p2p_orders_status_deadline
    ON p2p_orders(status, payment_deadline);
CREATE INDEX IF NOT EXISTS idx_p2p_orders_parties
    ON p2p_orders(token_provider_id, fiat_payer_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_user_active
    ON sessions(user_id, expires_at);

COMMIT;
