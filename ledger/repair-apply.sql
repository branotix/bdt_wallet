-- STEP 2 of 2: Only run this AFTER reading repair-preview.sql's output and
-- confirming the amounts look right. This actually changes balances.

BEGIN;

WITH duplicates AS (
    SELECT id, user_id, amount, reference_id,
           ROW_NUMBER() OVER (PARTITION BY reference_id ORDER BY id) AS rn
    FROM ledger_entries
    WHERE entry_type = 'deposit'
),
to_remove AS (
    SELECT id, user_id, amount FROM duplicates WHERE rn > 1
),
per_user_excess AS (
    SELECT user_id, SUM(amount) AS excess
    FROM to_remove
    GROUP BY user_id
)
UPDATE wallets w
SET balance = w.balance - pue.excess,
    updated_at = now()
FROM per_user_excess pue
WHERE w.user_id = pue.user_id;

DELETE FROM ledger_entries
WHERE id IN (
    SELECT id FROM (
        SELECT id, ROW_NUMBER() OVER (PARTITION BY reference_id ORDER BY id) AS rn
        FROM ledger_entries
        WHERE entry_type = 'deposit'
    ) x WHERE x.rn > 1
);

-- Sanity check: this must return ZERO rows. If it returns any, something is
-- wrong beyond the known duplicate-deposit bug — run ROLLBACK below instead
-- of COMMIT, and share the output before doing anything else.
SELECT user_id, balance FROM wallets WHERE balance < 0;

COMMIT;
