-- Fixes a bug: ConfirmPayment (P2P trade release) inserts ledger_entries
-- with entry_type='p2p_trade', but the original CHECK constraint only
-- allowed ('transfer', 'deposit', 'withdraw', 'fee') — so every confirm
-- attempt failed with a generic DB error ("could not confirm payment").

BEGIN;

ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_entry_type_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_entry_type_check
    CHECK (entry_type IN ('transfer', 'deposit', 'withdraw', 'fee', 'p2p_trade'));

COMMIT;
