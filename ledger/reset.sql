-- ⚠️  DESTRUCTIVE — TESTNET ONLY. This erases every balance, ledger entry,
-- deposit address and withdrawal record. There is no undo. Never run this
-- against a database holding real user funds.
--
--   psql "$DATABASE_URL" -f ledger/reset.sql
--
-- Use it when a test session has produced a ledger you no longer trust (e.g.
-- balances that exceed the on-chain supply). Your token contract and the BDT
-- actually sitting in the hot wallet are untouched — only the off-chain
-- bookkeeping is wiped.
--
-- After running this, restart the relayer. It will seed watcher_state to the
-- current block, so any deposit made BEFORE the reset is not re-credited.

BEGIN;

-- Order matters only for readability; TRUNCATE ... CASCADE handles the FKs.
TRUNCATE ledger_entries,
         processed_chain_events,
         withdrawals,
         deposit_addresses,
         wallets,
         watcher_state,
         users
    RESTART IDENTITY CASCADE;

-- Recreate the reserved treasury account (user_id = 1) exactly as schema.sql does.
INSERT INTO users (id) VALUES (1);
SELECT setval('users_id_seq', 1);
INSERT INTO wallets (user_id, balance) VALUES (1, 0);

COMMIT;

\echo 'Ledger reset. Treasury is user_id = 1 with balance 0.'
\echo 'If this database was created before the withdrawal-safety changes, also run:'
\echo '  psql "$DATABASE_URL" -f ledger/migrate-002-withdrawal-safety.sql'
\echo 'Restart the relayer before creating accounts, so watcher_state is seeded'
\echo 'to the current block and old deposits are not re-scanned.'
