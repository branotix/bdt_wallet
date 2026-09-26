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
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/shopspring/decimal"

	"bdt-relayer/internal/ledger"
)

const erc20ABI = `[{"constant":false,"inputs":[{"name":"_to","type":"address"},{"name":"_value","type":"uint256"}],"name":"transfer","outputs":[{"name":"","type":"bool"}],"type":"function"},
{"constant":true,"inputs":[{"name":"_owner","type":"address"}],"name":"balanceOf","outputs":[{"name":"balance","type":"uint256"}],"type":"function"}]`

type WithdrawWorker struct {
	client    *ethclient.Client
	tokenAddr common.Address
	signer    *HotWalletSigner
	ledger    *ledger.Ledger
	tokenABI  abi.ABI
	decimals  int32
}

// NewWithdrawWorker takes a shared *HotWalletSigner (see hotwallet_signer.go)
// rather than a raw private key — the sweeper uses the SAME signer instance,
// which is what prevents the two from colliding on the hot wallet's nonce.
func NewWithdrawWorker(client *ethclient.Client, tokenAddr common.Address, signer *HotWalletSigner, l *ledger.Ledger) (*WithdrawWorker, error) {
	parsedABI, err := abi.JSON(strings.NewReader(erc20ABI))
	if err != nil {
		return nil, err
	}
	return &WithdrawWorker{
		client: client, tokenAddr: tokenAddr, signer: signer,
		ledger: l, tokenABI: parsedABI, decimals: 18,
	}, nil
}

type PendingWithdrawal struct {
	ID        int64
	ToAddress string
	Amount    decimal.Decimal
}

// Run polls the withdrawals table for pending rows and processes them.
// Nonce safety no longer depends on "only run one instance" alone — the
// shared HotWalletSigner serializes every send from this hot wallet, so even
// concurrent callers within this same process (the sweeper's gas funding)
// cannot collide. Running two separate PROCESSES against the same hot wallet
// is still unsafe (see MAINNET.md, worker=1) since the mutex is in-memory
// and can't coordinate across processes — that's what the advisory lock in
// cmd/main.go guards against.
func (w *WithdrawWorker) Run(ctx context.Context, pollInterval time.Duration, fetchPending func(ctx context.Context) ([]PendingWithdrawal, error)) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			pending, err := fetchPending(ctx)
			if err != nil {
				continue
			}
			for _, p := range pending {
				if err := w.processOne(ctx, p); err != nil {
					// Two cases, both safe to just log:
					//
					//  - we never got as far as claiming the row (RPC error, no hot
					//    wallet gas, not enough BDT swept in yet): status is still
					//    'pending' and the next tick retries.
					//  - we claimed it and the send failed: status is
					//    'broadcasting' with raw_tx stored, and the confirmation
					//    poller re-broadcasts those exact bytes.
					//
					// Never refund here. Only the confirmation poller may do that,
					// and only once the nonce proves the tx can never be mined.
					log.Printf("withdraw worker: withdrawal %d not completed this tick: %v", p.ID, err)
				}
			}
		}
	}
}

func (w *WithdrawWorker) processOne(ctx context.Context, p PendingWithdrawal) error {
	amountWei := decimalToWei(p.Amount, w.decimals)

	// The hot wallet only holds BDT that the sweeper has consolidated out of
	// deposit addresses, so it can legitimately be short right after a deposit.
	// Check before broadcasting: an on-chain revert costs gas, and it drags the
	// user's balance through a debit -> refund round trip for nothing.
	hotBalance, err := w.tokenBalance(ctx, w.signer.Address)
	if err != nil {
		return err
	}
	if hotBalance.Cmp(amountWei) < 0 {
		return fmt.Errorf("hot wallet holds %s BDT but withdrawal needs %s — waiting for a sweep to top it up",
			decimal.NewFromBigInt(hotBalance, -w.decimals), p.Amount)
	}

	data, err := w.tokenABI.Pack("transfer", common.HexToAddress(p.ToAddress), amountWei)
	if err != nil {
		return err
	}

	// SignOnly holds the hot wallet's lock until we call broadcast() or
	// abandon() below — nothing else (including the sweeper) can grab this
	// nonce in between.
	signedTx, broadcast, abandon, err := w.signer.SignOnly(ctx, w.tokenAddr, big.NewInt(0), data, 80000)
	if err != nil {
		return err
	}
	rawTx, err := signedTx.MarshalBinary()
	if err != nil {
		abandon()
		return err
	}

	// Record hash + nonce + raw bytes BEFORE broadcasting, and only proceed if
	// this call is the one that moved the row out of 'pending'. Broadcasting
	// first and writing afterwards leaves a window where the tokens are gone but
	// the row still says 'pending' — the next tick signs a SECOND transfer and
	// the user is paid twice out of the hot wallet float.
	//
	// The other ordering is safe: if the write lands and the send fails, no
	// tokens moved and the poller re-broadcasts these exact bytes.
	claimed, err := w.ledger.ClaimForBroadcast(ctx, p.ID, signedTx.Hash().Hex(), signedTx.Nonce(), hexutil.Encode(rawTx))
	if err != nil {
		abandon()
		return err
	}
	if !claimed {
		// Someone else already claimed it, or it is no longer pending. Sending
		// now would be a duplicate payout.
		abandon()
		return fmt.Errorf("withdrawal %d is no longer pending — not broadcasting", p.ID)
	}

	if err := broadcast(); err != nil {
		// Left in 'broadcasting'. The poller finds no receipt, sees the nonce is
		// still unused, and re-sends the stored raw_tx — self-healing without any
		// risk of a second payout, because it is the same signed transaction.
		return fmt.Errorf("broadcast withdrawal %d (nonce %d tx %s) failed, poller will retry: %w",
			p.ID, signedTx.Nonce(), signedTx.Hash().Hex(), err)
	}

	log.Printf("withdraw worker: broadcast withdrawal %d nonce=%d amount=%s to=%s tx=%s",
		p.ID, signedTx.Nonce(), p.Amount, p.ToAddress, signedTx.Hash().Hex())
	return nil
}

// tokenBalance reads the BDT balance of an address with a plain eth_call — no
// gas, no transaction.
func (w *WithdrawWorker) tokenBalance(ctx context.Context, addr common.Address) (*big.Int, error) {
	data, err := w.tokenABI.Pack("balanceOf", addr)
	if err != nil {
		return nil, err
	}
	result, err := w.client.CallContract(ctx, ethereum.CallMsg{To: &w.tokenAddr, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	var out *big.Int
	if err := w.tokenABI.UnpackIntoInterface(&out, "balanceOf", result); err != nil {
		return nil, err
	}
	return out, nil
}

// decimalToWei converts a decimal.Decimal token amount into the integer
// base-unit representation the chain expects, using exact decimal shifting
// (no float64 anywhere in this path).
func decimalToWei(amount decimal.Decimal, decimals int32) *big.Int {
	shifted := amount.Shift(decimals)
	return shifted.BigInt()
}
