-- STEP 1 of 2: Run this FIRST and read the output carefully before running
-- repair-apply.sql. This only SELECTs — it changes nothing.

SELECT
    le.reference_id,
    le.user_id,
    COUNT(*) AS duplicate_count,
    MIN(le.id) AS keep_id,
    SUM(le.amount) - (SELECT amount FROM ledger_entries WHERE id = MIN(le.id)) AS amount_to_remove
FROM ledger_entries le
WHERE le.entry_type = 'deposit'
GROUP BY le.reference_id, le.user_id
HAVING COUNT(*) > 1;
