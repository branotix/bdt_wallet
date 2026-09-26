-- Migration 002 — withdrawal safety and idempotency fixes.
--
--   psql "$DATABASE_URL" -f ledger/migrate-002-withdrawal-safety.sql
--
-- Safe to run on a database with real balances in it: this only adds columns and
-- widens constraints, it never touches a balance or a ledger entry. Safe to run
-- twice.
--
-- Run this INSTEAD of ledger/reset.sql if you have data you want to keep.
-- schema.sql already contains everything below, so a database created fresh from
-- schema.sql does not need this file.
--
-- What each change is for:
--
--  1. withdrawals.nonce — lets the confirmation poller prove an unmined
--     transaction can never be mined (the hot wallet's confirmed nonce moved
--     past it) before refunding. The old code refunded on a 30-minute timeout,
--     which pays the user twice if the transaction later confirms.
--
--  2. withdrawals.raw_tx — lets the poller re-broadcast the exact same signed
--     transaction after a lost send, instead of stranding it.
--
--  3. status 'stuck' — for a withdrawal that was broadcast, never mined, and
--     cannot be proven dead. Refusing to auto-refund is the safe choice, so it
--     is parked for a human instead.
--
--  4. processed_chain_events primary key — was tx_hash alone, so only the FIRST
--     Transfer log in a transaction could ever be credited. Must be
--     (tx_hash, log_index).
--
--  5. deposit_addresses.swept_at — lets the sweeper stop re-checking an address
--     it has already emptied.

BEGIN;

ALTER TABLE withdrawals ADD COLUMN IF NOT EXISTS nonce  BIGINT;
ALTER TABLE withdrawals ADD COLUMN IF NOT EXISTS raw_tx TEXT;

-- Postgres names an inline CHECK on `status` as <table>_<column>_check. Drop and
-- recreate rather than trying to alter it in place.
ALTER TABLE withdrawals DROP CONSTRAINT IF EXISTS withdrawals_status_check;
ALTER TABLE withdrawals ADD CONSTRAINT withdrawals_status_check
    CHECK (status IN ('pending', 'broadcasting', 'confirmed', 'failed', 'stuck'));

-- Existing rows predate per-log keying; 0 is the correct value for them because
-- they were recorded when only one log per transaction was ever credited.
ALTER TABLE processed_chain_events ADD COLUMN IF NOT EXISTS log_index INT NOT NULL DEFAULT 0;
ALTER TABLE processed_chain_events ALTER COLUMN log_index DROP DEFAULT;
ALTER TABLE processed_chain_events DROP CONSTRAINT IF EXISTS processed_chain_events_pkey;
ALTER TABLE processed_chain_events DROP CONSTRAINT IF EXISTS processed_chain_events_tx_hash_log_index_key;
ALTER TABLE processed_chain_events ADD PRIMARY KEY (tx_hash, log_index);

ALTER TABLE deposit_addresses ADD COLUMN IF NOT EXISTS swept_at TIMESTAMPTZ;

COMMIT;

\echo 'Migration 002 applied. Now run ledger/audit.sql and confirm total_owed does'
\echo 'not exceed the BDT actually held by the hot wallet + deposit addresses.'
