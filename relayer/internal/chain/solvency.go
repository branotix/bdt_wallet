package chain

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/shopspring/decimal"
)

// SolvencyMonitor answers the one question that matters in custody: does the
// platform actually hold the tokens its ledger says it owes?
//
// This exists because of a real bug in this codebase. The watcher used to credit
// ANY transfer into a deposit address, including transfers from our own hot
// wallet. The sweeper then pulled those tokens straight back to the hot wallet,
// so the loop could be repeated forever: on-chain supply never changed, but the
// ledger grew by the full amount each time. Balances reached 215,986,819 BDT
// against an 18,000,000 supply before anyone noticed.
//
// The bug is fixed (see Watcher.processLog), but the class of bug is not — any
// future path that credits a balance without tokens arriving from outside will
// do the same thing. So we check the invariant continuously instead of trusting
// that we got every code path right:
//
//	sum(wallets.balance)  <=  BDT held by hot wallet + all deposit addresses
//
// A breach means users are owed tokens that do not exist. It is not
// self-correcting and it gets worse with every withdrawal, so it is logged as
// loudly as a log line can be.
type SolvencyMonitor struct {
	client    *ethclient.Client
	tokenAddr common.Address
	hotAddr   common.Address
	tokenABI  abi.ABI
	decimals  int32
	// minGas is the BNB balance below which the hot wallet is warned about.
	// Out of gas means withdrawals stop broadcasting and sweeps stop running,
	// both silently from a user's point of view.
	minGas decimal.Decimal
}

// maxCustodyAddresses caps how many deposit addresses one solvency pass will
// query, so the check can never turn into thousands of RPC calls on a tick.
const maxCustodyAddresses = 500

func NewSolvencyMonitor(client *ethclient.Client, tokenAddr, hotAddr common.Address) (*SolvencyMonitor, error) {
	parsedABI, err := abi.JSON(strings.NewReader(erc20ABI))
	if err != nil {
		return nil, err
	}
	return &SolvencyMonitor{
		client:    client,
		tokenAddr: tokenAddr,
		hotAddr:   hotAddr,
		tokenABI:  parsedABI,
		decimals:  18,
		minGas:    decimal.NewFromFloat(0.02),
	}, nil
}

// Run checks the invariant immediately and then on every tick. It never returns
// an error for a failed check — a check that cannot run must not take the
// relayer down with it.
func (m *SolvencyMonitor) Run(
	ctx context.Context,
	interval time.Duration,
	ledgerTotal func(ctx context.Context) (decimal.Decimal, error),
	custodyAddresses func(ctx context.Context) ([]common.Address, error),
) error {
	m.check(ctx, ledgerTotal, custodyAddresses)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.check(ctx, ledgerTotal, custodyAddresses)
		}
	}
}

func (m *SolvencyMonitor) check(
	ctx context.Context,
	ledgerTotal func(ctx context.Context) (decimal.Decimal, error),
	custodyAddresses func(ctx context.Context) ([]common.Address, error),
) {
	m.checkGas(ctx)

	owed, err := ledgerTotal(ctx)
	if err != nil {
		log.Printf("solvency: cannot read ledger total: %v", err)
		return
	}
	addrs, err := custodyAddresses(ctx)
	if err != nil {
		log.Printf("solvency: cannot list custody addresses: %v", err)
		return
	}
	if len(addrs) > maxCustodyAddresses {
		log.Printf("solvency: %d custody addresses exceeds the %d per-pass cap — skipping this check, move the audit to a batched RPC call",
			len(addrs), maxCustodyAddresses)
		return
	}

	held := decimal.Zero
	for _, a := range addrs {
		bal, err := m.tokenBalance(ctx, a)
		if err != nil {
			// A partial sum would look like a shortfall and cry wolf. Abort.
			log.Printf("solvency: cannot read BDT balance of %s, skipping this check: %v", a.Hex(), err)
			return
		}
		held = held.Add(decimal.NewFromBigInt(bal, -m.decimals))
	}

	if owed.GreaterThan(held) {
		log.Printf("!!! INSOLVENT: ledger owes %s BDT but only %s BDT is in custody (short %s). "+
			"Stop accepting withdrawals and run ledger/audit.sql — a balance was credited without tokens arriving.",
			owed.StringFixed(2), held.StringFixed(2), owed.Sub(held).StringFixed(2))
		return
	}
	log.Printf("solvency ok: ledger owes %s BDT, custody holds %s BDT across %d addresses",
		owed.StringFixed(2), held.StringFixed(2), len(addrs))
}

// checkGas warns when the hot wallet is running out of BNB. Every withdrawal and
// every sweep is paid for from here; when it empties, both stop and the only
// symptom is withdrawals sitting in 'pending' forever.
func (m *SolvencyMonitor) checkGas(ctx context.Context) {
	wei, err := m.client.BalanceAt(ctx, m.hotAddr, nil)
	if err != nil {
		log.Printf("solvency: cannot read hot wallet BNB balance: %v", err)
		return
	}
	bnb := decimal.NewFromBigInt(wei, -18)
	if bnb.LessThan(m.minGas) {
		log.Printf("WARNING: hot wallet %s holds only %s BNB — withdrawals and sweeps will stop. Top it up.",
			m.hotAddr.Hex(), bnb.StringFixed(6))
	}
}

func (m *SolvencyMonitor) tokenBalance(ctx context.Context, addr common.Address) (*big.Int, error) {
	data, err := m.tokenABI.Pack("balanceOf", addr)
	if err != nil {
		return nil, err
	}
	result, err := m.client.CallContract(ctx, ethereum.CallMsg{To: &m.tokenAddr, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	var out *big.Int
	if err := m.tokenABI.UnpackIntoInterface(&out, "balanceOf", result); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("empty balanceOf result for %s", addr.Hex())
	}
	return out, nil
}
