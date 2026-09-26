# BDT Token — Setup Guide

Fixed-supply BEP-20 token on BNB Chain, with an off-chain Postgres ledger for
free/instant internal transfers. On-chain activity only happens for
deposits (user wallet → your platform) and withdrawals (your platform →
user wallet).

## ✅ Progress so far (testnet)

- **Token deployed**: `0x656416676C34d1b55EC216b1714AAfa953C1aa63` (BSC Testnet)
- **Verified on BscScan**: https://testnet.bscscan.com/address/0x656416676C34d1b55EC216b1714AAfa953C1aa63#code
- **Initial supply**: 18,000,000 BDT (fixed, no mint function exists)
- **Tested end to end**: deposit → sweep → internal transfer → withdraw, with the
  ledger audit balancing afterwards

> **Going to mainnet?** Read [MAINNET.md](MAINNET.md) instead of this file. It is
> the step-by-step runbook: deploy, wallet layout, Heroku, verification, and what
> to do when a withdrawal gets stuck. This README explains how the pieces work;
> that one explains the order to do them in.

## Architecture recap

```
User's external wallet
      │ deposit (on-chain, user pays gas)
      ▼
Per-user deposit address (HD-derived)  ──sweep──▶  Hot wallet
      │
      ▼ (watcher credits Postgres)
┌─────────────────────────────┐
│  Postgres ledger (ACID)     │◀── internal transfers (free, instant)
│  wallets, ledger_entries    │
└─────────────────────────────┘
      │ withdraw request
      ▼
Withdraw worker (hot wallet signs & sends, user pays no gas
                 upfront — fee deducted from their BDT balance)
      │
      ▼
User's external wallet
```

## 1. Prerequisites

- Node.js 18+ (for contract deploy) and Go 1.22+ (for the relayer)
- A Postgres 14+ instance
- A funded deployer wallet (small amount of BNB for deploy gas)
- A **separate** hot wallet for withdrawals — do not reuse the deployer key
- A single unified Etherscan API key (get free at etherscan.io — this now
  covers BSC too via Etherscan V2, no separate BscScan key needed)

## 2. Deploy the token contract — ✅ DONE (testnet)

```bash
cd bdt-token
npm install --save-dev hardhat@^2.22.0 @nomicfoundation/hardhat-toolbox@^5.0.0
npm install @openzeppelin/contracts

# Testnet first — get free test BNB from https://testnet.bnbchain.org/faucet-smart
export DEPLOYER_PRIVATE_KEY=0xyour_deployer_key
export INITIAL_SUPPLY=18000000   # whole tokens, fixed forever after this

npx hardhat run scripts/deploy.js --network bscTestnet
```

> **Note:** if you install the newest Hardhat and get `"Hardhat only supports
> ESM projects"`, that's Hardhat v3 (ESM-first) conflicting with this repo's
> CommonJS config. Pin to `hardhat@^2.22.0` as shown above.

Verify on the block explorer (unified Etherscan V2 API now covers BSC):

```bash
export ETHERSCAN_API_KEY=your_etherscan_key
npx hardhat verify --network bscTestnet <TOKEN_ADDRESS> 18000000
```

Once you're confident everything works, repeat deploy+verify with
`--network bscMainnet` and a fresh `INITIAL_SUPPLY` decision (this is
permanent, decide carefully).


## 3. Set up Postgres

```bash
createdb bdt_platform
psql bdt_platform -f ledger/schema.sql
```

This creates `users`, `wallets`, `ledger_entries`, `deposit_addresses`,
`withdrawals`, `processed_chain_events`, and `watcher_state`. It also seeds
`user_id = 1` as your treasury account — never let a real signup take this id.

If you already have a `users` table (e.g. from Branotix), drop the
`CREATE TABLE IF NOT EXISTS users` block and make sure your existing table
has a `BIGINT` primary key.

**Upgrading an existing database?** `schema.sql` only creates missing tables, so
it will not add columns to tables you already have. Run the migration too — it is
idempotent and non-destructive, safe on a database holding real balances:

```bash
psql "$DATABASE_URL" -f ledger/migrate-002-withdrawal-safety.sql
```

It adds `withdrawals.nonce`, `withdrawals.raw_tx`, the `'stuck'` status,
`deposit_addresses.swept_at`, and widens the `processed_chain_events` primary key
to `(tx_hash, log_index)` so two token transfers in one transaction are both
credited. Without it the relayer fails on startup queries.

There is also `ledger/reset.sql`, which wipes every balance and ledger entry.
It is **testnet only** and there is no undo.

## 4. Generate a deposit wallet mnemonic

This is the master seed for **every** user's deposit address. Generate it
once, offline, and never regenerate it (you'd lose access to old deposit
addresses).

```bash
# Any BIP39 mnemonic generator works, e.g.:
npx -y bip39-cli generate
```

Store this mnemonic and your hot wallet's private key in a secrets manager
(not a `.env` file committed anywhere, not in your shell history long-term).

## 5. Configure and run the relayer

```bash
cd relayer
go mod tidy
```

Copy the example env file and fill in real values:

```bash
cp .env.example .env
nano .env
```

| Variable | Description |
|---|---|
| `BSC_RPC_URL` | Your BSC node or provider endpoint (Ankr, QuickNode, etc — avoid free public RPCs in production) |
| `BDT_TOKEN_ADDRESS` | Contract address from step 2 |
| `HOT_WALLET_PRIVATE_KEY` | Hot wallet's private key (pays gas for withdrawals + sweeps) |
| `HOT_WALLET_ADDRESS` | Hot wallet's public address — must match the key above, checked at startup |
| `DEPOSIT_WALLET_MNEMONIC` | The mnemonic from step 4 |
| `DATABASE_URL` | `postgres://user:pass@host:5432/bdt_platform` |
| `REQUIRED_CONFIRMATIONS` | Optional, default `15`. Use **15 or more on mainnet** — a reorg that drops a credited deposit means you handed out balance backed by nothing. |

The relayer refuses to start if `HOT_WALLET_ADDRESS` is not the address that
`HOT_WALLET_PRIVATE_KEY` derives to. A mismatch is silent and expensive: the
watcher compares incoming transfers against `HOT_WALLET_ADDRESS` to decide what
is an internal movement rather than a real deposit, so a wrong address makes it
credit your own outgoing float as customer deposits.

`.env` is already in `.gitignore` — it will load automatically when you run
the relayer, no need to `export` anything manually. **Never commit `.env`** —
check with `git status` before any `git add .` that it doesn't show up.

For production (a real server, not local dev), skip `.env` entirely and
inject these same variables via systemd's `EnvironmentFile=` or Docker
secrets instead — more auditable and harder to accidentally leak.

Run it:

```bash
go run ./cmd
```

This starts five concurrent processes in one binary:
1. **Watcher** — scans new blocks for deposits to known addresses, credits Postgres
2. **Withdraw worker** — signs, records, then broadcasts pending withdrawals
3. **Confirmation poller** — confirms broadcasted txs, re-broadcasts lost ones,
   and refunds only when it can *prove* the transaction can never be mined
4. **Sweeper** — every 15 min, consolidates deposit-address funds into the hot wallet
5. **Solvency monitor** — every 5 min, checks that `sum(wallets.balance)` is still
   covered by the BDT actually held in custody, and warns when BNB gas runs low

For production, run this under systemd or as a container with automatic restart,
and monitor the logs for `refunding`, `stuck`, and `!!! INSOLVENT` — those need
human attention. A `Dockerfile` and `heroku.yml` are in the repo root;
[MAINNET.md](MAINNET.md) §6 covers the deploy.

**Only ever run one instance against a given hot wallet.** Two relayers read the
same next nonce, sign two different withdrawals with it, and one gets silently
dropped — after the user's balance was already debited.

## 6. Graphical test console (GUI)

A browser UI for testing the whole flow by clicking, instead of hand-writing
SQL or curl calls. It's a second binary in the same Go module, so there are no
new dependencies to install:

```bash
cd relayer
go run ./cmd/api
```

Then open <http://127.0.0.1:8080>.

It reads the same `.env` as the relayer, but needs far less of it — no RPC
endpoint and no hot wallet key, because this process never touches the chain:

| Variable | Required | Purpose |
|---|---|---|
| `DATABASE_URL` | yes | The ledger it reads and writes |
| `DEPOSIT_WALLET_MNEMONIC` | for deposits | Derives each account's deposit address |
| `HOT_WALLET_ADDRESS` | no | Shown in the UI, and used to reject withdrawals aimed at your own hot wallet |
| `API_ADDR` | no | Listen address. Defaults to `127.0.0.1:8080`, or `:$PORT` if `PORT` is set |
| `API_AUTH_TOKEN` | if not loopback | Shared bearer token. **Required** to bind any non-loopback address — the process exits rather than serving money-moving endpoints openly |
| `BDT_NETWORK` | no | `mainnet` switches the UI label and the explorer links to bscscan.com. Anything else means testnet |

What you can do from it:

- **Create account** — inserts a `users` row and its `wallets` row in one
  transaction, then derives the deposit address
- **Deposit** — shows that account's deposit address. Send BDT to it from an
  external wallet and the balance appears once the watcher has seen
  `REQUIRED_CONFIRMATIONS` blocks. **This needs `go run ./cmd` running too** —
  the console only reads the ledger, the relayer is what watches the chain.
- **Internal transfer** — user-to-user, instant, flat **3 BDT** fee
- **Withdraw** — queues a `pending` withdrawal, flat **10 BDT** fee. The
  relayer's withdraw worker broadcasts it, and the table tracks
  `pending → broadcasting → confirmed` / `failed` / `stuck`.
- **Ledger history** — the raw double-entry rows behind every balance change

A `stuck` withdrawal means the transaction was broadcast but never mined, *and*
the relayer could not prove it will never be mined. It is deliberately **not**
refunded, because refunding a transaction that later confirms pays the user
twice. [MAINNET.md](MAINNET.md) §9 explains how to resolve one.

Fees are flat (not percentage-based), defined once in
`relayer/internal/fees/fees.go`, and always debited **on top of** the amount:
withdrawing 100 BDT costs 110 BDT total. Deposits are free — the user already
paid their own gas to send them. Every fee is credited to `user_id = 1`, the
treasury.

### Endpoints

| Method | Path | Body / query |
|---|---|---|
| `POST` | `/api/accounts` | — creates an account |
| `GET` | `/api/accounts` | — lists accounts with balances |
| `GET` | `/api/account` | `?user_id=2` — balance, deposit address, history, withdrawals |
| `POST` | `/api/transfer` | `{"from_user_id":2,"to_user_id":3,"amount":"50.00"}` |
| `POST` | `/api/withdraw` | `{"user_id":2,"to_address":"0x…","amount":"50.00"}` |
| `GET` | `/api/config` | — fee schedule, network label, explorer base, treasury id |

Every `/api/*` endpoint requires `Authorization: Bearer $API_AUTH_TOKEN` when
that variable is set. `GET /` is exempt so the page can load and ask you for the
token.

Amounts may be JSON strings or numbers, with at most 2 decimal places. The
ledger columns are `NUMERIC(20,2)`, so the API rejects anything it would have
to round rather than silently truncating a user's money.

> ⚠️ **No per-user authentication.** The account is whatever `user_id` the
> request body claims, so any caller can move any user's balance. That's fine for
> a localhost console and unacceptable for anything a customer touches.
> `API_AUTH_TOKEN` keeps strangers out but makes every holder an admin — it is not
> a substitute. When you wire this into a real backend, take `user_id` from the
> authenticated session and never from the request body.

## 7. Fund the hot wallet

The hot wallet needs:
- **BNB** for gas — every withdrawal (~$0.05-0.15) and every sweep-gas-funding
  transaction (~$0.02) draws from this. The solvency monitor warns below
  0.02 BNB; at zero, withdrawals stay `pending` and sweeps stop. Top it up.
- **BDT** — it accumulates from sweeps as users deposit, so on a busy platform it
  is self-sustaining. But it must be able to cover withdrawals *now*, and on day
  one it holds nothing, so seed it with a float from the deployer wallet.

Do **not** put the whole premined supply in the hot wallet. Its key lives on a
server; keep the amount a server compromise could take small enough that it does
not end the project. See [MAINNET.md](MAINNET.md) §2.

## 8. Security checklist before going live

- [ ] `go build ./...` and `go vet ./...` are clean in `relayer/`
- [ ] `HOT_WALLET_PRIVATE_KEY` and `DEPOSIT_WALLET_MNEMONIC` are in a secrets
      manager, never in git history (check with `git log -p` before first push)
- [ ] `.env` is excluded from Docker builds — that is what `.dockerignore` is
      for; a key baked into an image layer stays there forever
- [ ] Postgres has regular backups — `ledger_entries` is your source of truth,
      losing it means losing the ability to prove who owns what
- [ ] `REQUIRED_CONFIRMATIONS` is 15 or more on mainnet
- [ ] `ledger/audit.sql` shows `total_owed` ≤ on-chain custody, and no
      unbalanced transaction groups
- [ ] The hot wallet holds a working float, not the entire supply
- [ ] Only ever run **one** instance of the withdraw worker against a given
      hot wallet (nonce collisions otherwise)
- [ ] The console is not reachable from the internet, or at minimum has
      `API_AUTH_TOKEN` set — and you understand that is admin access, not
      per-user auth
- [ ] Tested the full deposit → internal transfer → withdraw flow on testnet
      with a wallet that is **not** the hot wallet
- [ ] Rate-limit withdrawal requests per user (still not built — see §9)
- [ ] Consider a manual-approval threshold for large withdrawals initially
      (still not built — see §9)

## 9. Known follow-ups (not yet built)

- **No rate limiting** on `POST /api/withdraw`. The checklist calls for it; the
  code does not have it.
- **No approval step** for large withdrawals — every request is picked up
  automatically by the worker.
- **No admin dashboard** for stuck/failed withdrawals. Monitoring is reading the
  log for `stuck`, `refunding`, and `!!! INSOLVENT`.
- **No session auth.** `relayer/internal/api` is a test console that trusts the
  `user_id` in the request body — see the warning in section 6. For production,
  either put real auth middleware in front of it and derive `user_id` from the
  session, or call `internal/ledger` directly from your own backend and keep this
  console for local use only.
- **No liquidity.** BDT cannot be sold for BNB until a pool exists or you find a
  buyer, so early gas costs come out of your own pocket.

Two rules that are not follow-ups but permanent constraints, both learned the
hard way on testnet:

- Money is `shopspring/decimal` everywhere. **Never** reintroduce `float64` in
  code that touches balances.
- A deposit is only real when tokens arrive from **outside** the platform.
  Crediting an internal movement (hot wallet, or one of your own deposit
  addresses) mints ledger balance from nothing, because the sweeper returns those
  tokens to where they started. That bug inflated the testnet ledger to
  215,986,819 BDT against an 18,000,000 supply. The watcher rejects those
  transfers now, and the solvency monitor exists to catch it if anything ever
  slips through again.

## Production-hardening profile

This fork is intended to be a much safer deployment baseline, not a substitute
for an independent security audit. Before real funds are enabled:

- use BSC mainnet with `REQUIRED_CONFIRMATIONS=15` or higher;
- keep the hot-wallet private key out of Git, Docker images, and source files;
- use a dedicated secrets manager/runtime secret injection;
- set `CORS_ALLOWED_ORIGINS` to exact HTTPS origins (never `*`);
- expose the admin console only behind an authenticated private network or a
  strong admin token/reverse proxy;
- run exactly one relayer worker per hot wallet/database;
- keep PostgreSQL backups and test restore procedures;
- monitor solvency, stuck withdrawals, watcher lag and failed broadcasts;
- load-test the API before claiming a 100k-user capacity target;
- perform a smart-contract and application security audit before mainnet funds.

The default confirmation setting is now 15. The mainnet worker also refuses to
start if fewer than 15 confirmations are configured.
# bdt_wallet
