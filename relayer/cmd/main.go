package main

import (
	"context"
	"log"
	"math/big"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"bdt-relayer/internal/chain"
	"bdt-relayer/internal/config"
	"bdt-relayer/internal/fees"
	"bdt-relayer/internal/ledger"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := ethclient.DialContext(ctx, cfg.RPCUrl)
	if err != nil {
		log.Fatalf("connect RPC: %v", err)
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()

	// Refuse to run a second relayer against the same database. Two instances
	// sharing one hot wallet both read the same "next nonce" and each signs a
	// different withdrawal with it — only one is ever mined, and the other
	// user has already been debited for a payout that never arrives. The
	// MAINNET.md checklist says "worker=1, never worker=2"; this makes that a
	// hard failure instead of an operator discipline problem.
	//
	// pg_advisory_lock is SESSION-scoped, so it must be held on one dedicated
	// connection for the process lifetime — acquiring it through the pool
	// (which hands out different connections per query) would not hold it.
	// The lock is released automatically if this process dies or the
	// connection drops, so a crashed instance never permanently blocks a
	// restart.
	const relayerLockKey = 727100727 // arbitrary constant, just needs to be unique to this app
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		log.Fatalf("acquire db connection for startup lock: %v", err)
	}
	defer lockConn.Release()
	var gotLock bool
	if err := lockConn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, relayerLockKey).Scan(&gotLock); err != nil {
		log.Fatalf("acquire relayer lock: %v", err)
	}
	if !gotLock {
		log.Fatal("another relayer instance already holds the lock on this database — refusing to start a second one against the same hot wallet (see MAINNET.md, worker=1 never worker=2)")
	}
	log.Println("acquired single-instance lock — safe to run the withdraw worker")

	l := ledger.New(pool)
	tokenAddr := common.HexToAddress(cfg.TokenAddress)

	// Read the chain ID from the node instead of hardcoding it. BSC mainnet is
	// 56 and testnet (chapel) is 97; signing with the wrong one makes every
	// withdrawal fail with "invalid chain id for signer".
	chainID, err := client.ChainID(ctx)
	if err != nil {
		log.Fatalf("get chain id: %v", err)
	}

	mnemonic := mustEnv("DEPOSIT_WALLET_MNEMONIC")
	addrMgr, err := chain.NewDepositAddressManager(mnemonic, pool)
	if err != nil {
		log.Fatalf("deposit address manager: %v", err)
	}
	lookup := chain.NewDBDepositLookup(pool)

	hotAddr := common.HexToAddress(cfg.HotWalletAddress)

	// Mainnet reorgs are rare but not impossible, and a reorged-out deposit is a
	// credit against tokens that no longer exist. Testnet defaults are too thin
	// for real money.
	if chainID.Cmp(big.NewInt(56)) == 0 && cfg.RequiredConfirmations < 15 {
		log.Fatalf("refusing to run on BSC MAINNET with only %d confirmations — set REQUIRED_CONFIRMATIONS=15 or higher", cfg.RequiredConfirmations)
	}

	// One signer, shared by the withdraw worker and the sweeper's gas-funding
	// step, so the two can never sign two different transactions with the
	// same hot-wallet nonce (see hotwallet_signer.go).
	signer, err := chain.NewHotWalletSigner(client, cfg.HotWalletPrivateKey, chainID)
	if err != nil {
		log.Fatalf("hot wallet signer: %v", err)
	}
	if signer.Address != hotAddr {
		log.Fatalf("HOT_WALLET_PRIVATE_KEY does not match HOT_WALLET_ADDRESS (got %s, configured %s) — check your .env",
			signer.Address.Hex(), hotAddr.Hex())
	}

	watcher := chain.NewWatcher(client, tokenAddr, hotAddr, cfg.RequiredConfirmations, lookup, l)
	withdrawWorker, err := chain.NewWithdrawWorker(client, tokenAddr, signer, l)
	if err != nil {
		log.Fatalf("withdraw worker: %v", err)
	}
	confirmPoller := chain.NewConfirmationPoller(client, l, hotAddr, fees.TreasuryUserID, cfg.RequiredConfirmations, 30*time.Minute)
	sweeper, err := chain.NewSweeper(client, tokenAddr, signer, chainID, addrMgr)
	if err != nil {
		log.Fatalf("sweeper: %v", err)
	}
	solvency, err := chain.NewSolvencyMonitor(client, tokenAddr, hotAddr)
	if err != nil {
		log.Fatalf("solvency monitor: %v", err)
	}

	// --- Determine starting block for the watcher ---
	startBlock, err := getLastScannedBlock(ctx, pool, client)
	if err != nil {
		log.Fatalf("determine start block: %v", err)
	}

	// --- Run all four processes concurrently ---
	go must("watcher", func() error {
		return watcher.Run(ctx, startBlock, 5*time.Second, func(ctx context.Context, lastBlock uint64) error {
			return saveWatcherProgress(ctx, pool, lastBlock)
		})
	})

	go must("withdraw worker", func() error {
		return withdrawWorker.Run(ctx, 5*time.Second, func(ctx context.Context) ([]chain.PendingWithdrawal, error) {
			return fetchPendingWithdrawals(ctx, pool)
		})
	})

	go must("confirmation poller", func() error {
		return confirmPoller.Run(ctx, 15*time.Second, func(ctx context.Context) ([]chain.BroadcastedWithdrawal, error) {
			return fetchBroadcastingWithdrawals(ctx, pool)
		})
	})

	go must("sweeper", func() error {
		return sweeper.Run(ctx, 15*time.Minute, func(ctx context.Context) ([]chain.SweepAddress, error) {
			return fetchSweepCandidates(ctx, pool)
		}, func(ctx context.Context, userID int64) error {
			return markSwept(ctx, pool, userID)
		})
	})

	go must("solvency monitor", func() error {
		return solvency.Run(ctx, 5*time.Minute,
			func(ctx context.Context) (decimal.Decimal, error) { return totalOwed(ctx, pool) },
			func(ctx context.Context) ([]common.Address, error) { return custodyAddresses(ctx, pool, hotAddr) })
	})

	// P2P escrow expiry sweep: cancels orders still awaiting a payment claim
	// past their 15-minute window, unlocking the token provider's escrowed
	// balance. Orders already marked 'paid' are never touched by this (see
	// ledger.MarkPaid) — only a human confirming or disputing resolves those.
	go must("p2p order expiry", func() error {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				n, err := l.ExpireStalePendingOrders(ctx)
				if err != nil {
					log.Printf("p2p order expiry: %v", err)
					continue
				}
				if n > 0 {
					log.Printf("p2p order expiry: cancelled %d stale order(s), escrow released", n)
				}
			}
		}
	})

	log.Printf("bdt-relayer running on chain %s with %d confirmations: watcher (from block %d), withdraw worker, confirmation poller, sweeper, solvency monitor, p2p expiry",
		chainID, cfg.RequiredConfirmations, startBlock)
	<-ctx.Done()
	log.Println("shutting down")
}

func must(name string, fn func() error) {
	if err := fn(); err != nil {
		log.Printf("%s stopped: %v", name, err)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required env var: %s", key)
	}
	return v
}

// getLastScannedBlock resumes from where the watcher left off (persisted in
// a simple key-value table), or falls back to current block on first run.
func getLastScannedBlock(ctx context.Context, pool *pgxpool.Pool, client *ethclient.Client) (uint64, error) {
	var block uint64
	err := pool.QueryRow(ctx, `SELECT last_block FROM watcher_state WHERE id = 1`).Scan(&block)
	if err == nil {
		return block, nil
	}
	// No state yet — start from current head so we don't re-scan all history.
	current, err := client.BlockNumber(ctx)
	if err != nil {
		return 0, err
	}
	_, err = pool.Exec(ctx, `INSERT INTO watcher_state (id, last_block) VALUES (1, $1)`, current)
	return current, err
}

// saveWatcherProgress persists the watcher's progress after every
// successfully-scanned block range, so a restart resumes from here instead
// of re-scanning (or worse, skipping) blocks.
func saveWatcherProgress(ctx context.Context, pool *pgxpool.Pool, lastBlock uint64) error {
	_, err := pool.Exec(ctx, `UPDATE watcher_state SET last_block = $1 WHERE id = 1`, lastBlock)
	return err
}

func fetchPendingWithdrawals(ctx context.Context, pool *pgxpool.Pool) ([]chain.PendingWithdrawal, error) {
	rows, err := pool.Query(ctx, `SELECT id, to_address, amount FROM withdrawals WHERE status = 'pending' ORDER BY created_at LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chain.PendingWithdrawal
	for rows.Next() {
		var p chain.PendingWithdrawal
		var amt decimal.Decimal
		if err := rows.Scan(&p.ID, &p.ToAddress, &amt); err != nil {
			return nil, err
		}
		p.Amount = amt
		out = append(out, p)
	}
	return out, nil
}

func fetchBroadcastingWithdrawals(ctx context.Context, pool *pgxpool.Pool) ([]chain.BroadcastedWithdrawal, error) {
	// nonce and raw_tx are what let the confirmation poller distinguish "this can
	// never be mined, refund it" from "this is still live, re-send it and wait".
	// COALESCE on raw_tx keeps rows written before the column existed scannable.
	rows, err := pool.Query(ctx, `
		SELECT id, user_id, tx_hash, amount, fee, nonce, COALESCE(raw_tx, ''), created_at
		  FROM withdrawals
		 WHERE status = 'broadcasting' AND tx_hash IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chain.BroadcastedWithdrawal
	for rows.Next() {
		var w chain.BroadcastedWithdrawal
		if err := rows.Scan(&w.ID, &w.UserID, &w.TxHash, &w.Amount, &w.Fee, &w.Nonce, &w.RawTx, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func fetchSweepCandidates(ctx context.Context, pool *pgxpool.Pool) ([]chain.SweepAddress, error) {
	// Re-check every deposit address on a rolling basis rather than only the
	// ones with a credited deposit. Tokens can land in a deposit address
	// WITHOUT the watcher crediting anything — someone sends from an address we
	// already control, a deposit arrives before the account row exists, or a
	// previous sweep reverted. Those balances used to be stranded forever,
	// because a "sweep only what we credited" query can never see them.
	//
	// swept_at keeps this cheap: an address drops out for an hour once it reads
	// zero, and ORDER BY swept_at NULLS FIRST makes the LIMIT round-robin so no
	// address is starved.
	rows, err := pool.Query(ctx, `
		SELECT user_id, address FROM deposit_addresses
		WHERE swept_at IS NULL OR swept_at < now() - interval '1 hour'
		ORDER BY swept_at NULLS FIRST
		LIMIT 50
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chain.SweepAddress
	for rows.Next() {
		var s chain.SweepAddress
		var addrHex string
		if err := rows.Scan(&s.UserID, &addrHex); err != nil {
			return nil, err
		}
		s.Address = common.HexToAddress(addrHex)
		out = append(out, s)
	}
	return out, nil
}

// markSwept stamps deposit_addresses.swept_at so fetchSweepCandidates stops
// returning this address until a NEW deposit arrives for it.
func markSwept(ctx context.Context, pool *pgxpool.Pool, userID int64) error {
	_, err := pool.Exec(ctx, `UPDATE deposit_addresses SET swept_at = now() WHERE user_id = $1`, userID)
	return err
}

// totalOwed is the sum of every user balance — what the platform would have to
// pay out if everyone withdrew at once. The solvency monitor compares it against
// the BDT actually held on-chain.
func totalOwed(ctx context.Context, pool *pgxpool.Pool) (decimal.Decimal, error) {
	var total decimal.Decimal
	err := pool.QueryRow(ctx, `SELECT COALESCE(sum(balance), 0) FROM wallets`).Scan(&total)
	return total, err
}

// custodyAddresses lists every on-chain address the platform's BDT can legally
// be sitting in: the hot wallet, plus each user's deposit address (tokens rest
// there between arriving and being swept).
//
// Anything held anywhere else is not backing user balances, which is exactly
// what the solvency check is meant to catch.
func custodyAddresses(ctx context.Context, pool *pgxpool.Pool, hotAddr common.Address) ([]common.Address, error) {
	rows, err := pool.Query(ctx, `SELECT address FROM deposit_addresses ORDER BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []common.Address{hotAddr}
	for rows.Next() {
		var addrHex string
		if err := rows.Scan(&addrHex); err != nil {
			return nil, err
		}
		out = append(out, common.HexToAddress(addrHex))
	}
	return out, rows.Err()
}
