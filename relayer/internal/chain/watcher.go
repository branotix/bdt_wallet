package chain

import (
	"context"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/shopspring/decimal"

	"bdt-relayer/internal/ledger"
)

// Transfer(address indexed from, address indexed to, uint256 value)
// keccak256 of that signature — must be the full 32 bytes (64 hex chars).
// HexToHash left-pads anything shorter with zeros instead of failing, which
// silently produces a topic that matches no logs at all.
var transferEventSig = common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")

// DepositLookup resolves a deposit address back to your internal user_id.
// Backed by a DB table mapping user_id -> derived deposit address (see
// address derivation notes below the watcher).
type DepositLookup interface {
	UserIDForAddress(ctx context.Context, addr common.Address) (int64, bool, error)
}

type Watcher struct {
	client    *ethclient.Client
	tokenAddr common.Address
	// hotAddr is the platform's own custody wallet. Transfers OUT of it are
	// never deposits — see processLog for why crediting them mints money.
	hotAddr       common.Address
	confirmations uint64
	lookup        DepositLookup
	ledger        *ledger.Ledger
	// decimals is read from the token contract itself at startup (see
	// NewWatcher) instead of assumed to be 18. If this relayer is ever
	// pointed at a token whose decimals() differs from 18, hardcoding 18
	// here would silently mis-scale every single deposit — e.g. a token
	// with 6 decimals would have every deposit credited 10^12x too large,
	// or the reverse for a token with more than 18.
	decimals int32
}

var erc20DecimalsABI = mustParseABI(`[{"constant":true,"inputs":[],"name":"decimals","outputs":[{"name":"","type":"uint8"}],"type":"function"}]`)

func mustParseABI(j string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(j))
	if err != nil {
		panic(err) // this ABI literal is fixed and known-valid; a failure here is a code bug, not a runtime condition
	}
	return parsed
}

func NewWatcher(client *ethclient.Client, tokenAddr, hotAddr common.Address, confirmations uint64, lookup DepositLookup, l *ledger.Ledger) *Watcher {
	decimals := readTokenDecimals(client, tokenAddr)
	return &Watcher{client: client, tokenAddr: tokenAddr, hotAddr: hotAddr, confirmations: confirmations, lookup: lookup, ledger: l, decimals: decimals}
}

// readTokenDecimals calls decimals() on the token contract. Falls back to 18
// (the ERC20 convention, and what BDTToken.sol actually uses) only if the
// call itself fails — e.g. RPC hiccup at startup — logging loudly either way
// so a wrong value is never silent.
func readTokenDecimals(client *ethclient.Client, tokenAddr common.Address) int32 {
	data, err := erc20DecimalsABI.Pack("decimals")
	if err != nil {
		log.Printf("watcher: could not build decimals() call, defaulting to 18: %v", err)
		return 18
	}
	result, err := client.CallContract(context.Background(), ethereum.CallMsg{To: &tokenAddr, Data: data}, nil)
	if err != nil {
		log.Printf("watcher: decimals() call failed, defaulting to 18: %v", err)
		return 18
	}
	var d uint8
	if err := erc20DecimalsABI.UnpackIntoInterface(&d, "decimals", result); err != nil {
		log.Printf("watcher: could not decode decimals(), defaulting to 18: %v", err)
		return 18
	}
	log.Printf("watcher: token decimals() = %d", d)
	return int32(d)
}

// Run polls for new blocks and processes Transfer events. Polling (not
// subscription) is used deliberately — WebSocket subscriptions on public/shared
// RPC endpoints drop silently; polling with a persisted "last scanned block"
// is more resilient for a relayer that must never miss a deposit.
//
// saveProgress is called after each successfully-scanned range so progress
// survives a restart (persisted to watcher_state in Postgres).
func (w *Watcher) Run(ctx context.Context, fromBlock uint64, pollInterval time.Duration, saveProgress func(ctx context.Context, lastBlock uint64) error) error {
	current := fromBlock
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	tick := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			tick++
			latest, err := w.client.BlockNumber(ctx)
			if err != nil {
				log.Printf("watcher: get block number failed: %v", err)
				continue
			}
			// Heartbeat every ~60s so "is this even running / how far behind
			// is it" never requires a restart to answer — the exact question
			// that made this bug hard to diagnose without logs.
			if tick%12 == 0 {
				log.Printf("watcher: heartbeat — current=%d latest=%d (behind by %d blocks)", current, latest, latest-current)
			}
			// Only scan up to latest-confirmations, so we don't credit deposits
			// that could still be reorged out.
			if latest < w.confirmations {
				continue
			}
			safeHead := latest - w.confirmations
			if current > safeHead {
				continue // nothing new and confirmed yet
			}

			// Re-scan a small overlap behind `current` on every tick, not just
			// the exact new range. Public/load-balanced RPC endpoints can
			// serve BlockNumber() from a node that's ahead of the node that
			// answers FilterLogs() a moment later — the log for a real deposit
			// can simply not be indexed yet on whichever node answers, come
			// back empty (no error), and get silently skipped forever once
			// `current` moves past it. Re-checking the last ~20 blocks costs
			// nothing extra (CreditDeposit is idempotent, already-processed
			// logs are free no-ops) and catches exactly this class of miss
			// without needing to know which node lagged.
			const overlapBlocks = 20
			scanFrom := current
			if current > overlapBlocks {
				scanFrom = current - overlapBlocks
			}

			if err := w.scanRange(ctx, scanFrom, safeHead); err != nil {
				log.Printf("watcher: scan range %d-%d failed: %v", scanFrom, safeHead, err)
				continue // retry same range next tick, do NOT advance `current`
			}
			current = safeHead + 1

			if saveProgress != nil {
				if err := saveProgress(ctx, current); err != nil {
					log.Printf("watcher: failed to persist progress at block %d: %v", current, err)
					// Not fatal — worst case on restart we re-scan a small
					// range, which is safe because CreditDeposit is idempotent.
				}
			}
		}
	}
}

func (w *Watcher) scanRange(ctx context.Context, from, to uint64) error {
	// Public RPC endpoints (like BSC's free data-seed nodes) reject
	// eth_getLogs queries spanning too many blocks, AND rate-limit rapid
	// back-to-back requests. Chunk the scan into small windows, with a
	// small delay between chunks, so it works on any provider, free or paid.
	const maxChunk = 500
	const delayBetweenChunks = 300 * time.Millisecond

	first := true
	for chunkFrom := from; chunkFrom <= to; chunkFrom += maxChunk {
		if !first {
			time.Sleep(delayBetweenChunks)
		}
		first = false

		chunkTo := chunkFrom + maxChunk - 1
		if chunkTo > to {
			chunkTo = to
		}

		query := ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(chunkFrom),
			ToBlock:   new(big.Int).SetUint64(chunkTo),
			Addresses: []common.Address{w.tokenAddr},
			Topics:    [][]common.Hash{{transferEventSig}},
		}

		logs, err := w.client.FilterLogs(ctx, query)
		if err != nil {
			// Return here so the caller retries from `current` (unchanged)
			// on the next tick — we haven't advanced past this chunk yet.
			return err
		}

		for _, vLog := range logs {
			if err := w.processLog(ctx, vLog); err != nil {
				log.Printf("watcher: failed processing log tx=%s idx=%d: %v", vLog.TxHash.Hex(), vLog.Index, err)
			}
		}
	}
	return nil
}

func (w *Watcher) processLog(ctx context.Context, vLog types.Log) error {
	if len(vLog.Topics) < 3 {
		return nil // not a standard Transfer log, skip
	}
	fromAddr := common.HexToAddress(vLog.Topics[1].Hex())
	toAddr := common.HexToAddress(vLog.Topics[2].Hex())

	userID, ok, err := w.lookup.UserIDForAddress(ctx, toAddr)
	if err != nil {
		return err
	}
	if !ok {
		return nil // not sent to one of our deposit addresses — ignore silently
	}

	// The destination is ours, so this is a candidate deposit. But a deposit is
	// money arriving from OUTSIDE the platform: when the SENDER is also ours the
	// tokens never entered, they only moved within our own custody, and
	// crediting that mints balance out of thin air —
	//
	//   hot wallet -> user's deposit address   => credited as a "deposit"
	//   sweeper: deposit address -> hot wallet => tokens back where they started
	//
	// On-chain supply unchanged, ledger up by the full amount, repeatable
	// forever. So refuse internal senders. Checked after the destination lookup
	// so ordinary outgoing transfers (withdrawals, moving float around) don't
	// log anything.
	if fromAddr == w.hotAddr {
		log.Printf("watcher: hot wallet -> user %d deposit address: internal float, NOT credited as a deposit (tx=%s idx=%d)", userID, vLog.TxHash.Hex(), vLog.Index)
		return nil
	}
	if _, internal, err := w.lookup.UserIDForAddress(ctx, fromAddr); err != nil {
		return err
	} else if internal {
		log.Printf("watcher: deposit address %s -> user %d deposit address: internal float, NOT credited as a deposit (tx=%s idx=%d)", fromAddr.Hex(), userID, vLog.TxHash.Hex(), vLog.Index)
		return nil
	}

	amountRaw := new(big.Int).SetBytes(vLog.Data)
	amount := weiToDecimal(amountRaw, int(w.decimals))

	err = w.ledger.CreditDeposit(ctx, userID, amount, vLog.TxHash.Hex(), int(vLog.Index))
	if err != nil {
		if err == ledger.ErrAlreadyProcessed {
			return nil // fine, already handled — idempotent
		}
		return err
	}

	log.Printf("credited deposit: user=%d amount=%s tx=%s", userID, amount.String(), vLog.TxHash.Hex())
	return nil
}

// weiToDecimal converts a raw on-chain integer amount (base units) into a
// decimal.Decimal using exact integer division — no float64 anywhere in
// this path, so no precision drift.
func weiToDecimal(raw *big.Int, decimals int) decimal.Decimal {
	return decimal.NewFromBigInt(raw, -int32(decimals))
}
