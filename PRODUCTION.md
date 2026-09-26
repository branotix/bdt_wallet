# BDT Wallet — production deployment checklist

## 1. Configure secrets
Copy `.env.example` to `.env` and set real values. Never commit `.env`.

Required: `BSC_RPC_URL`, `BDT_TOKEN_ADDRESS`, `HOT_WALLET_ADDRESS`, `HOT_WALLET_PRIVATE_KEY`, `DEPOSIT_WALLET_MNEMONIC`, `DATABASE_URL`, `API_AUTH_TOKEN`, and `CORS_ALLOWED_ORIGINS`.

Use a dedicated secrets manager where possible. The deposit mnemonic controls every derived deposit address; the hot-wallet key controls the withdrawal float.

## 2. Initialize database
Run `ledger/schema.sql`, then migrations `002` through `008` in order. Existing production data must be backed up before any migration.

## 3. Start exactly one relayer
The relayer takes a PostgreSQL advisory lock and refuses a second instance. Do not horizontally scale the relayer worker against the same hot wallet.

## 4. Run the console behind a reverse proxy
The console is admin-grade and must not be exposed directly to the public internet. Put TLS, IP allowlisting/VPN and a reverse proxy in front of it. Keep `API_AUTH_TOKEN` set even when bound locally.

## 5. Mainnet safeguards
Use BSC mainnet only after testnet validation. Keep at least 15 confirmations. Keep only a limited operational float in the hot wallet; sweep excess funds to cold/offline custody.

## 6. P2P
P2P fiat payment is external to the blockchain. A token provider must verify receipt before releasing escrow. Disputes require an authenticated operator decision. Never auto-release based only on the buyer's "paid" claim.

## 7. Operational requirements
Enable Postgres backups/PITR, monitor wallet solvency, withdrawal failures, stuck transactions, RPC errors, login abuse, and P2P disputes. Load-test the API before advertising a 100k-user capacity.
