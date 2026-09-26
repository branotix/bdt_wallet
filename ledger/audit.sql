-- BDT ledger audit queries — run these after any test session.
--
-- The invariant that matters: the platform can never owe users more BDT than
-- it actually holds on-chain. If query 1 reports more than the token's total
-- supply, the ledger has minted money and something upstream is broken.
--
--   psql "$DATABASE_URL" -f ledger/audit.sql

\echo '=== 1. Total BDT the ledger says we owe (must be <= on-chain supply) ==='
SELECT sum(balance) AS total_owed FROM wallets;

\echo ''
\echo '=== 2. Cached balances vs the ledger_entries that justify them ==='
\echo '(any row here means wallets.balance drifted from the audit trail)'
SELECT w.user_id,
       w.balance                        AS cached_balance,
       COALESCE(sum(le.amount), 0)      AS ledger_sum,
       w.balance - COALESCE(sum(le.amount), 0) AS drift
FROM wallets w
LEFT JOIN ledger_entries le ON le.user_id = w.user_id
GROUP BY w.user_id, w.balance
HAVING w.balance <> COALESCE(sum(le.amount), 0)
ORDER BY w.user_id;

\echo ''
\echo '=== 3. Transaction groups that do not sum to zero ==='
\echo '(deposits and withdrawals are expected here — they cross the system'
\echo ' boundary. Internal transfers must always sum to exactly zero.)'
SELECT transaction_group,
       string_agg(DISTINCT entry_type, ',') AS types,
       sum(amount)                          AS net
FROM ledger_entries
GROUP BY transaction_group
HAVING sum(amount) <> 0
ORDER BY min(created_at);

\echo ''
\echo '=== 4. Deposits credited, newest first ==='
SELECT user_id, amount, reference_id AS tx_hash, created_at
FROM ledger_entries
WHERE entry_type = 'deposit'
ORDER BY created_at DESC
LIMIT 20;

\echo ''
\echo '=== 5. Withdrawals by status ==='
SELECT status, count(*), sum(amount) AS amount, sum(fee) AS fees
FROM withdrawals
GROUP BY status
ORDER BY status;

\echo ''
\echo '=== 6. Duplicate deposit credits for the same tx hash ==='
\echo '(should always be empty — a hit here means the idempotency guard failed)'
SELECT reference_id AS tx_hash, count(*) AS times_credited, sum(amount) AS total
FROM ledger_entries
WHERE entry_type = 'deposit'
GROUP BY reference_id
HAVING count(*) > 1;
