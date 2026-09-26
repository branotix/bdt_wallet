-- BDT Off-chain Ledger Schema
-- Design goals: ACID correctness, double-entry accounting, deadlock avoidance,
-- idempotent processing of on-chain events.

BEGIN;

-- Minimal users table — replace/merge with your existing auth users table
-- if you already have one from another project. Just needs a stable BIGINT id.
CREATE TABLE IF NOT EXISTS users (
    id          BIGSERIAL PRIMARY KEY,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Reserve user_id = 1 as your treasury/fee-collection account. Every
-- transfer's fee lands here (see Ledger.Transfer's treasuryUserID param).
-- Insert it once, and never let a real signup take this id.
INSERT INTO users (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
SELECT setval('users_id_seq', GREATEST((SELECT MAX(id) FROM users), 1));

-- Tracks the last block the deposit watcher fully processed, so it resumes
-- correctly after a restart instead of re-scanning or skipping blocks.
CREATE TABLE watcher_state (
    id          INT PRIMARY KEY DEFAULT 1,
    last_block  BIGINT NOT NULL,
    CONSTRAINT single_row CHECK (id = 1)
);

-- Users' internal wallet balances. One row per user.
-- balance is DERIVED from ledger_entries but cached here for fast reads —
-- it must ALWAYS be updated inside the same transaction as the ledger_entries
-- that justify the change (never update this table alone).
CREATE TABLE wallets (
    user_id      BIGINT PRIMARY KEY REFERENCES users(id),
    balance      NUMERIC(20, 2) NOT NULL DEFAULT 0 CHECK (balance >= 0),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Double-entry ledger: every movement of value is TWO rows (a debit and a credit)
-- that must sum to zero within a transaction_group. This gives you a full,
-- immutable audit trail independent of the `wallets.balance` cache above.
CREATE TABLE ledger_entries (
    id                 BIGSERIAL PRIMARY KEY,
    transaction_group  UUID NOT NULL,          -- groups the debit+credit pair (or larger set) of one logical transfer
    user_id            BIGINT NOT NULL REFERENCES users(id),
    amount             NUMERIC(20, 2) NOT NULL, -- positive = credit, negative = debit
    entry_type         TEXT NOT NULL CHECK (entry_type IN ('transfer', 'deposit', 'withdraw', 'fee', 'p2p_trade')),
    reference_id       TEXT,                    -- external tx hash for deposit/withdraw, or internal transfer id
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_ledger_user ON ledger_entries(user_id);
CREATE INDEX idx_ledger_group ON ledger_entries(transaction_group);
CREATE INDEX idx_ledger_reference ON ledger_entries(reference_id);

-- Tracks on-chain events we've already processed, so a retried/duplicate
-- blockchain-watcher event can NEVER be credited twice. This is your
-- idempotency guard for deposits.
--
-- The key is (tx_hash, log_index), NOT tx_hash alone: one transaction can
-- contain several Transfer logs, and with tx_hash as the sole primary key only
-- the first of them would ever be credited.
CREATE TABLE processed_chain_events (
    tx_hash       TEXT NOT NULL,
    log_index     INT NOT NULL,
    processed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tx_hash, log_index)
);

-- Withdrawal requests. Separate table so you can track pending -> broadcasting
-- -> confirmed -> failed states for on-chain sends.
CREATE TABLE withdrawals (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT NOT NULL REFERENCES users(id),
    amount        NUMERIC(20, 2) NOT NULL CHECK (amount > 0),
    fee           NUMERIC(20, 2) NOT NULL DEFAULT 0,
    to_address    TEXT NOT NULL,
    -- 'stuck' means broadcast but never mined, and we could NOT prove the
    -- nonce is dead — so refunding might pay the user twice. Needs a human.
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'broadcasting', 'confirmed', 'failed', 'stuck')),
    tx_hash       TEXT,
    -- Nonce of the signed transaction, recorded BEFORE it is broadcast. This is
    -- what lets the confirmation poller tell "this tx can never be mined"
    -- (the hot wallet's confirmed nonce moved past it, so another tx took the
    -- slot) apart from "still pending in a mempool, could confirm any minute".
    -- Refunding the second case pays the user twice.
    nonce         BIGINT,
    -- Hex of the fully signed transaction. Lets the poller re-broadcast the
    -- EXACT same transaction if the first send was lost to an RPC error.
    -- Re-sending identical bytes is safe: same nonce, so the chain can include
    -- it at most once. Without this, a transient network blip needs a human.
    raw_tx        TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_withdrawals_status ON withdrawals(status);

-- Deterministic HD-derived deposit address per user (see relayer/internal/chain/hdwallet.go)
CREATE TABLE deposit_addresses (
    user_id           BIGINT PRIMARY KEY REFERENCES users(id),
    address           TEXT NOT NULL UNIQUE,
    derivation_index  BIGINT NOT NULL,
    swept_at          TIMESTAMPTZ, -- last time this address's funds were swept to the hot wallet
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_deposit_addresses_address ON deposit_addresses(address);

-- Seed the treasury wallet row now that `wallets` exists.
INSERT INTO wallets (user_id, balance) VALUES (1, 0) ON CONFLICT (user_id) DO NOTHING;

COMMIT;

-- ============================================================
-- DEADLOCK-SAFE TRANSFER PATTERN (do this in your Go code, in
-- a single DB transaction, at isolation level READ COMMITTED):
--
-- 1. ALWAYS lock the two wallet rows in a FIXED, consistent order
--    (e.g. ascending user_id), never in "sender then receiver" order.
--    This is the #1 cause of deadlocks in ledger systems — two
--    concurrent transfers A->B and B->A locking in opposite order.
--
--    SELECT balance FROM wallets WHERE user_id IN (LEAST($1,$2), GREATEST($1,$2))
--    ORDER BY user_id FOR UPDATE;
--
-- 2. Check sender balance >= amount in application code (the CHECK
--    constraint is your last line of defense, not your primary check —
--    it turns a bug into a hard error instead of a silent bad state).
--
-- 3. Insert the two ledger_entries (debit + credit) with the SAME
--    transaction_group UUID.
--
-- 4. Update both wallets.balance in the same DB transaction.
--
-- 5. COMMIT. If anything fails, the whole transfer rolls back atomically.
-- ============================================================
