# Going live on BSC mainnet

Read this once from top to bottom before you run anything. Every step in it
exists because skipping it costs real money.

Testnet is finished. What follows is the whole path from "it works on chapel" to
"the relayer is running on Heroku against mainnet".

---

## 0. The three wallets, once and for all

This is the part that has been confusing, so here it is plainly. There are
**three** keys, and only two of them are used after launch.

| # | Wallet | Where the key lives | What it does | Reused? |
|---|---|---|---|---|
| **A** | **Deployer** | Your machine only. Never on Heroku. | Signs the one transaction that creates the token. Receives the entire premined supply. | No — used once, then put away |
| **B** | **Hot wallet** | Heroku config var `HOT_WALLET_PRIVATE_KEY` | Holds the BDT float and the BNB gas. Signs every withdrawal and every sweep. | Yes, constantly |
| **C** | **Your payout wallet** | MetaMask, on your phone/laptop. Never in any config. | Where you send collected fees to sell them. | Yes, manually |

**A and B must be different keys.** The deployer holds the whole supply the
moment the contract is created; the hot wallet sits online 24/7 with its key in a
Heroku config var. Making them the same wallet means one leaked config var loses
the entire 18,000,000 BDT.

### Where do the fees actually collect?

There is no third wallet in the code, and you do not need to create one.

Fees are **off-chain numbers**, not on-chain transfers. Every 3 BDT internal fee
and every 10 BDT withdrawal fee is credited to `user_id = 1` — the treasury row
in Postgres, seeded by `ledger/schema.sql`. You can see it growing in the console
as the account tagged *treasury*.

To turn that into BNB:

1. Open the console, select **user 1**.
2. Withdraw its balance to **wallet C** (your MetaMask address).
3. The withdraw worker sends real BDT from the hot wallet to your MetaMask.
4. Sell it there, and send BNB back to the **hot wallet** to keep gas topped up.

So: wallet C is your "second wallet". You already have it. Nothing to generate.
The `generate wallet` work you were dreading belongs to Part 2 (the user-facing
web/mobile wallet) and is not needed for any of this.

> One honest warning about step 3: selling BDT requires somebody willing to buy
> it. A brand-new token has no market. Until you create a liquidity pool
> (PancakeSwap) or find an OTC buyer, the fees are BDT sitting in your MetaMask —
> real, but not yet BNB. The gas for withdrawals comes out of your own pocket
> until then. Plan for that.

---

## 1. Pre-flight — do not skip

Run each of these and confirm the result before going further.

```bash
cd relayer
go build ./...
go vet ./...
```

Both must be silent. Then, against your **testnet** database:

```bash
psql "$DATABASE_URL" -f ../ledger/audit.sql
```

Query 1 is `total_owed`. It must be **less than or equal to** the BDT actually
held by your hot wallet plus all deposit addresses. If it is larger, the ledger
is claiming money that does not exist — stop, and do not carry that database into
mainnet. `ledger/reset.sql` wipes it clean (testnet only).

Then do one last full testnet loop with a **second MetaMask account** as the
outside user:

- [ ] Deposit from MetaMask account 2 → credited after confirmations
- [ ] Internal transfer user→user → 3 BDT lands on user 1
- [ ] Withdraw to MetaMask account 2 → arrives on-chain, 10 BDT lands on user 1
- [ ] `audit.sql` still balances afterwards
- [ ] Relayer log shows `solvency ok:` lines, not `!!! INSOLVENT`

Two rules that come from real damage done during testing:

- **Never deposit from the hot wallet.** It is not a deposit, it is your own
  float moving. The watcher refuses to credit it (correctly), and the sweeper
  pulls it straight back.
- **Never send tokens to user 1's deposit address.** User 1 is the treasury, not
  a customer.

---

## 2. Deploy the token on mainnet

You need real BNB in the deployer wallet (**A**) — about 0.005 BNB is plenty.

```bash
cd bdt-token

export DEPLOYER_PRIVATE_KEY=0x…            # wallet A
export INITIAL_SUPPLY=18000000             # PERMANENT. See below.

npx hardhat run scripts/deploy.js --network bscMainnet
```

`INITIAL_SUPPLY` is fixed forever. There is no `mint()` in the contract — that
is the point of it, but it means this number cannot be corrected later. Decide it
now, deliberately. 18,000,000 matches your testnet run; if you want a different
number on mainnet, change it before you press enter, not after.

Verify the source so holders can read the contract:

```bash
export ETHERSCAN_API_KEY=your_etherscan_key
npx hardhat verify --network bscMainnet <TOKEN_ADDRESS> 18000000
```

The second argument must be **exactly** the `INITIAL_SUPPLY` you deployed with,
or verification fails with a constructor-argument mismatch.

Write the mainnet address down. It is not the testnet one, and mixing them up
means the relayer watches a contract nobody is using.

### Then split the supply

After deploy, all 18,000,000 BDT is in wallet **A**. Do **not** send all of it to
the hot wallet.

Send the hot wallet only what it needs to honour withdrawals — start with
something like 1–2% of supply, and top it up as needed. The rest stays in wallet
A, offline. A hot wallet is by definition a wallet whose key is on a server; keep
the amount that a server compromise could take small enough that it does not end
the project.

---

## 3. Create the hot wallet and the deposit mnemonic

**Hot wallet (B).** A fresh account. In MetaMask: *Add account* → export its
private key. Never used for anything else, never used as the deployer.

**Deposit mnemonic.** Generate once, offline:

```bash
npx -y bip39-cli generate
```

This single seed derives **every** user's deposit address
(`m/44'/60'/0'/0/{user_id}`). Consequences:

- Lose it and you lose access to every deposit address, including tokens sitting
  in them that have not been swept yet.
- Leak it and whoever has it can take every incoming deposit before your sweeper
  does.
- Never regenerate it. Existing users' addresses would change and deposits sent
  to the old ones would be unreachable.

Do **not** import this mnemonic into MetaMask. It is not your wallet; it is every
user's wallet.

Store both in a password manager, plus one offline copy on paper. Losing them is
unrecoverable — there is no support desk for this.

---

## 4. Heroku: app and Postgres

```bash
cd bdt-token
git init && git add . && git commit -m "bdt relayer"      # if not already a repo
heroku create your-bdt-relayer
heroku stack:set container -a your-bdt-relayer
heroku addons:create heroku-postgresql:essential-0 -a your-bdt-relayer
```

`essential-0` is the cheapest paid tier. Do not use a free/hobby tier for a
ledger — the row limits will silently start rejecting writes, and a rejected
`ledger_entries` insert is a rejected transfer.

Load the schema. Heroku Postgres requires TLS, so append `sslmode=require`:

```bash
DBURL=$(heroku config:get DATABASE_URL -a your-bdt-relayer)
psql "$DBURL?sslmode=require" -f ledger/schema.sql
```

Then turn on backups immediately, before there is anything to lose:

```bash
heroku pg:backups:schedule DATABASE_URL --at "03:00 Asia/Dhaka" -a your-bdt-relayer
heroku pg:backups:schedules -a your-bdt-relayer
```

`ledger_entries` is the only record of who owns what. The chain does not know
your users exist. If this database is lost and there is no backup, every internal
balance is gone, permanently, with no way to reconstruct it.

---

## 5. Configure

```bash
heroku config:set -a your-bdt-relayer \
  BSC_RPC_URL="https://your-provider-endpoint" \
  BDT_TOKEN_ADDRESS="0x…mainnet token…" \
  HOT_WALLET_ADDRESS="0x…wallet B…" \
  HOT_WALLET_PRIVATE_KEY="0x…wallet B key…" \
  DEPOSIT_WALLET_MNEMONIC="twelve words …" \
  REQUIRED_CONFIRMATIONS=15
```

`DATABASE_URL` is set for you by the Postgres add-on.

Notes on two of these:

- **`BSC_RPC_URL`** — do not use `https://bsc-dataseed.binance.org` or any other
  free public endpoint. They rate-limit, and a rate-limited watcher is a watcher
  that misses deposits. Use QuickNode, Ankr, NodeReal, or your own node.
- **`REQUIRED_CONFIRMATIONS=15`** — the default of 15 is a testnet number. On
  mainnet a reorg that drops a deposit you already credited means you handed out
  balance backed by nothing. The relayer logs a warning at startup if you run
  mainnet with fewer than 12.

The relayer refuses to start if `HOT_WALLET_PRIVATE_KEY` and
`HOT_WALLET_ADDRESS` are different wallets. That check is there because a
mismatch is silent and catastrophic: the watcher compares incoming transfers
against `HOT_WALLET_ADDRESS` to decide what is an internal movement rather than a
deposit, so a wrong address makes it start crediting your own outgoing float as
customer deposits — which is exactly the bug that inflated the testnet ledger to
215 million BDT.

---

## 6. Deploy and scale to exactly one

```bash
git push heroku main
heroku ps:scale worker=1 -a your-bdt-relayer
heroku logs --tail -a your-bdt-relayer
```

**`worker=1`. Never `worker=2`.** Two relayers sharing one hot wallet both read
the same "next nonce", both sign a different withdrawal with it, and only one can
be mined. The other is dropped — after the user's balance was already debited.

As of this version this is also enforced in code, not just discipline: the
relayer takes a Postgres advisory lock on startup and a second instance
against the same database refuses to boot (`another relayer instance already
holds the lock`). It is not a substitute for `worker=1` — Heroku can still
briefly run two instances during a deploy — but it turns the failure into a
loud crash instead of a silent double-payout.

Use a **Basic** dyno or higher. Eco dynos sleep, and a sleeping relayer credits
no deposits and broadcasts no withdrawals.

There is no `web` process on purpose. The relayer binds no port, and the test
console has no per-user authentication — it trusts whatever `user_id` the request
body claims. On a public `herokuapp.com` URL that is a self-service drain on
every balance. Run the console locally against the Heroku database instead:

```bash
cd relayer
DATABASE_URL="$(heroku config:get DATABASE_URL -a your-bdt-relayer)?sslmode=require" \
DEPOSIT_WALLET_MNEMONIC="…" \
HOT_WALLET_ADDRESS="0x…" \
BDT_NETWORK=mainnet \
go run ./cmd/api
```

If you genuinely must expose it, set `API_AUTH_TOKEN` to a long random string —
the console will then require `Authorization: Bearer <token>` and will prompt you
for it in the browser. It refuses to bind a non-loopback address without one.
Understand what that buys you: it keeps strangers out, it does **not** separate
one user from another. Everyone with the token is an admin.

---

## 7. First-run verification, in order

Do these with tiny amounts. 5 BDT, not 5,000.

1. **Startup line.** Expect
   `bdt-relayer running on chain 56 with 15 confirmations: …`. Chain 56 is
   mainnet; 97 means you are still pointed at testnet.
2. **Solvency line.** Within a minute:
   `solvency ok: ledger owes 0.00 BDT, custody holds … BDT`. If you see
   `!!! INSOLVENT`, stop and run `ledger/audit.sql`.
3. **Gas.** Send ~0.05 BNB to the hot wallet. Below 0.02 BNB the relayer starts
   warning; at zero, withdrawals and sweeps both stop.
4. **Deposit.** Create an account in the console, send it 5 BDT from your
   MetaMask (wallet C, or any outside wallet). It should credit after ~45 seconds
   (15 confirmations × ~3s). Within 15 minutes the sweeper moves it to the hot
   wallet — `swept user=… balance=…`.
5. **Internal transfer.** Move 1 BDT between two accounts. 3 BDT fee appears on
   user 1.
6. **Withdraw.** Send 1 BDT back out to MetaMask. Watch it go
   `pending → broadcasting → confirmed`, and check the tokens actually arrive.

   > If MetaMask shows nothing, it is almost certainly not a failure: MetaMask
   > only displays custom tokens you have imported, **per account**. Import the
   > mainnet BDT address on that account. Check BscScan before assuming anything
   > broke.
7. **Audit.** `psql "$DBURL?sslmode=require" -f ledger/audit.sql`. `total_owed`
   must still be ≤ on-chain custody, and query 3 (transaction groups that do not
   sum to zero) must return nothing.

Only after all seven pass should another human be allowed to deposit.

---

## 8. Running it day to day

**Watch the log for these strings.** Everything else is noise.

| Log line | Meaning | Action |
|---|---|---|
| `!!! INSOLVENT` | Ledger owes more than you hold | Stop withdrawals. Run `audit.sql`. Find the credit that had no deposit behind it. |
| `WARNING: hot wallet … holds only … BNB` | Gas running out | Send BNB. Withdrawals are stalling. |
| `WARNING: withdrawal … unmined for …` | Broadcast but not mined, still live | Usually gas price. See §9. |
| `marking stuck` | Cannot prove dead, not refunded | Manual decision. See §9. |
| `reverted on-chain — refunding` | Transfer failed, user credited back | Investigate why (hot wallet short of BDT?) |
| `NOT credited as a deposit` | Internal movement correctly ignored | None. This is the fix working. |

**Weekly:** run `ledger/audit.sql`. It takes ten seconds and it is the only thing
that catches a slow leak before it becomes a large one.

**Monthly:** confirm backups exist (`heroku pg:backups -a …`) and that the hot
wallet's BDT float still covers a plausible day of withdrawals.

---

## 9. When something goes wrong

### A withdrawal is stuck in `broadcasting`

The relayer will not refund it while the transaction could still confirm — that
would pay the user twice. It re-broadcasts the same signed transaction on every
poll, which handles the common case (a node dropped it).

If it is genuinely never going to be mined, usually because gas was too cheap,
you kill it by consuming its nonce yourself. The log line tells you the nonce.
From the hot wallet, in MetaMask (advanced settings → custom nonce), send 0 BNB
to yourself at that exact nonce with a high gas price. Once that confirms, the
relayer sees the nonce has moved past the stuck transaction, proves it can never
be mined, and refunds the user automatically on the next poll.

### A withdrawal is `stuck` in the database

Same situation, but the relayer had no nonce recorded (a row from before this
version) and stopped touching it. Check BscScan for the `tx_hash`:

- **Confirmed?** The user was paid.
  `UPDATE withdrawals SET status='confirmed' WHERE id=…;`
- **Nowhere, and the hot wallet's nonce is well past it?** Never mined.
  Refund it, once, by hand — and check first that no ledger entry with
  `reference_id = 'refund:withdrawal:<id>'` already exists.

### `!!! INSOLVENT`

Something credited a balance without tokens arriving from outside. Stop the
withdraw worker (`heroku ps:scale worker=0`) so nothing else leaves, then use
`audit.sql` query 4 (recent deposits) and query 6 (duplicate credits per tx) to
find it. Every legitimate deposit has a real inbound transfer on BscScan from an
address that is not yours.

### The hot wallet key leaked

Assume everything in it is gone. Immediately:

1. `heroku ps:scale worker=0`
2. From wallet A, do **not** send it any more BDT.
3. Create a new hot wallet, update both config vars, `worker=1`.

The deposit mnemonic is unaffected — different key entirely. User balances in
Postgres are unaffected. What you lose is whatever float was sitting in the old
hot wallet, which is precisely why §2 says not to keep the whole supply there.

---

## 10. What is still not built

Say this out loud before you let strangers in. None of these are bugs; they are
things that do not exist yet.

- **No per-user authentication anywhere.** The API identifies the account by the
  `user_id` in the request body. This is fine for a console you run on your own
  laptop and unacceptable for anything a customer touches. Part 2 (the web/mobile
  app) has to own real sessions and pass the authenticated `user_id` into
  `internal/ledger` directly.
- **No rate limiting** on withdrawal requests.
- **No approval step** for large withdrawals. Every request is automatic.
- **No admin dashboard.** Monitoring is reading the log.
- **No liquidity.** BDT cannot be sold for BNB until you create a pool or find a
  buyer.

The custody engine — ledger, deposits, sweeps, withdrawals, refunds, solvency
checking — is what is finished. Part 2 is the product on top of it.

## Security configuration added in the hardened build

Set `CORS_ALLOWED_ORIGINS` to the exact HTTPS origin(s) that host the wallet.
The API no longer intentionally uses wildcard CORS. The HTTP server also adds
security headers, bounded request bodies, request timeouts and a conservative
per-IP abuse guard. For multiple API instances, put distributed rate limiting
at the reverse proxy/API gateway as well.

## Production hardening migration

Before enabling real users, run `ledger/migrate-008-production-hardening.sql` after migrations 002 through 007. It adds withdrawal idempotency, strict destination-address validation, a unique transaction-hash guard, and indexes used by the P2P/session paths.

Mobile withdrawal requests must send a unique `Idempotency-Key` (16-128 characters). Reusing the same key for the same user returns the original withdrawal instead of creating another debit.
