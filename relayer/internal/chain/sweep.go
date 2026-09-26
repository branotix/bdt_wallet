package chain

import (
	"context"
	"log"
	"math/big"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/shopspring/decimal"
)

const erc20BalanceABI = `[{"constant":true,"inputs":[{"name":"_owner","type":"address"}],"name":"balanceOf","outputs":[{"name":"balance","type":"uint256"}],"type":"function"},
{"constant":false,"inputs":[{"name":"_to","type":"address"},{"name":"_value","type":"uint256"}],"name":"transfer","outputs":[{"name":"","type":"bool"}],"type":"function"}]`

// SweepAddress is a deposit address with a balance worth sweeping.
type SweepAddress struct {
	UserID  int64
	Address common.Address
}

type Sweeper struct {
	client    *ethclient.Client
	tokenAddr common.Address
	signer    *HotWalletSigner
	chainID   *big.Int
	addrMgr   *DepositAddressManager
	tokenABI  abi.ABI
	decimals  int32
}

// NewSweeper takes the SAME *HotWalletSigner instance passed to
// NewWithdrawWorker — sharing it is what prevents the sweeper's gas-funding
// sends and the withdraw worker's payout sends from colliding on the hot
// wallet's nonce (see hotwallet_signer.go). chainID is still needed
// separately here because the actual sweep transfer (deposit address ->
// hot wallet) is signed by the DEPOSIT ADDRESS's own derived key, not the
// hot wallet's — only the gas-funding step goes through the shared signer.
func NewSweeper(client *ethclient.Client, tokenAddr common.Address, signer *HotWalletSigner, chainID *big.Int, addrMgr *DepositAddressManager) (*Sweeper, error) {
	parsedABI, err := abi.JSON(strings.NewReader(erc20BalanceABI))
	if err != nil {
		return nil, err
	}
	return &Sweeper{
		client: client, tokenAddr: tokenAddr, signer: signer, chainID: chainID,
		addrMgr: addrMgr, tokenABI: parsedABI, decimals: 18,
	}, nil
}

// Run periodically sweeps every deposit address with a non-zero balance.
// This should run much less frequently than the deposit watcher — e.g.
// every 10-30 minutes — since it costs real gas per address swept.
//
// markSwept records that an address was swept, so listCandidates stops
// returning it. Without it every address that ever received a deposit is
// re-checked on every single tick, forever.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration, listCandidates func(ctx context.Context) ([]SweepAddress, error), markSwept func(ctx context.Context, userID int64) error) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Sweep once up front. A ticker doesn't fire until a full interval has
	// passed, so without this the hot wallet stays empty — and every
	// withdrawal reverts — for the first `interval` after every restart.
	s.sweepAll(ctx, listCandidates, markSwept)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.sweepAll(ctx, listCandidates, markSwept)
		}
	}
}

func (s *Sweeper) sweepAll(ctx context.Context, listCandidates func(ctx context.Context) ([]SweepAddress, error), markSwept func(ctx context.Context, userID int64) error) {
	candidates, err := listCandidates(ctx)
	if err != nil {
		log.Printf("sweeper: list candidates failed: %v", err)
		return
	}
	for _, c := range candidates {
		if err := s.sweepOne(ctx, c, markSwept); err != nil {
			log.Printf("sweeper: failed sweeping user %d addr %s: %v", c.UserID, c.Address.Hex(), err)
		}
	}
}

func (s *Sweeper) sweepOne(ctx context.Context, addr SweepAddress, markSwept func(ctx context.Context, userID int64) error) error {
	balance, err := s.tokenBalance(ctx, addr.Address)
	if err != nil {
		return err
	}
	if balance.Sign() == 0 {
		// BDT is fully swept. Also try to recover any leftover BNB gas dust
		// (see recoverGasDust) — best-effort, a failure here shouldn't block
		// marking the address swept, since the BDT (the part that matters)
		// is already safely at zero.
		if err := s.recoverGasDust(ctx, addr); err != nil {
			log.Printf("sweeper: gas dust recovery failed for user %d: %v", addr.UserID, err)
		}
		// Nothing left here, so the address is fully swept — record that and it
		// drops out of the candidate list until a NEW deposit arrives. Without
		// this stamp every address that ever received a deposit is re-checked
		// on every tick, forever.
		if markSwept != nil {
			if err := markSwept(ctx, addr.UserID); err != nil {
				log.Printf("sweeper: failed recording swept_at for user %d: %v", addr.UserID, err)
			}
		}
		return nil
	}

	// Step 1: ensure the deposit address has enough BNB to pay its own gas.
	// Compare against a small fixed floor (enough for one transfer at a
	// generously high gas price) just to decide WHETHER funding is needed —
	// fundGas itself computes the actual amount to send from the live gas
	// price, so this floor doesn't need to be exact.
	minGasFloor := big.NewInt(3e14) // 0.0003 BNB
	bnbBal, err := s.client.BalanceAt(ctx, addr.Address, nil)
	if err != nil {
		return err
	}
	if bnbBal.Cmp(minGasFloor) < 0 {
		if err := s.fundGas(ctx, addr.Address); err != nil {
			return err
		}
		// Give the funding tx a moment to confirm before spending it.
		time.Sleep(6 * time.Second)
	}

	// Step 2: send the full BDT balance from the deposit address to the hot wallet.
	account, err := s.addrMgr.PrivateKeyFor(addr.UserID)
	if err != nil {
		return err
	}
	privKey, err := s.addrMgr.wallet.PrivateKey(account)
	if err != nil {
		return err
	}

	nonce, err := s.client.PendingNonceAt(ctx, addr.Address)
	if err != nil {
		return err
	}
	gasPrice, err := s.client.SuggestGasPrice(ctx)
	if err != nil {
		return err
	}
	data, err := s.tokenABI.Pack("transfer", s.signer.Address, balance)
	if err != nil {
		return err
	}
	tx := types.NewTx(&types.LegacyTx{
		Nonce: nonce, To: &s.tokenAddr, Value: big.NewInt(0),
		Gas: 80000, GasPrice: gasPrice, Data: data,
	})
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(s.chainID), privKey)
	if err != nil {
		return err
	}
	if err := s.client.SendTransaction(ctx, signedTx); err != nil {
		return err
	}

	log.Printf("swept user=%d addr=%s balance=%s tx=%s", addr.UserID, addr.Address.Hex(), decimal.NewFromBigInt(balance, -s.decimals).String(), signedTx.Hash().Hex())

	// Deliberately NOT stamping swept_at here. The sweep is only broadcast at
	// this point, not confirmed — if it reverts, stamping would strand the
	// tokens by removing this address from the candidate list. Instead the next
	// tick re-checks the balance and stamps once it reads zero, which makes a
	// failed sweep self-healing.
	return nil
}

func (s *Sweeper) tokenBalance(ctx context.Context, addr common.Address) (*big.Int, error) {
	data, err := s.tokenABI.Pack("balanceOf", addr)
	if err != nil {
		return nil, err
	}
	result, err := s.client.CallContract(ctx, ethereum.CallMsg{To: &s.tokenAddr, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	var out *big.Int
	if err := s.tokenABI.UnpackIntoInterface(&out, "balanceOf", result); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Sweeper) fundGas(ctx context.Context, to common.Address) error {
	// Fund only what THIS transfer actually needs (current gas price × gas
	// limit), plus a 20% buffer for gas price moving between now and when the
	// deposit address's own send goes out — not a flat guess. The old fixed
	// 0.001 BNB was ~4x the real cost at typical gas prices, and since sweeps
	// only recover BDT (never BNB) from a deposit address, every bit of
	// overfunding sat there stranded forever.
	gasPrice, err := s.client.SuggestGasPrice(ctx)
	if err != nil {
		return err
	}
	needed := new(big.Int).Mul(gasPrice, big.NewInt(80000))
	needed = new(big.Int).Div(new(big.Int).Mul(needed, big.NewInt(120)), big.NewInt(100)) // +20%

	_, err = s.signer.Send(ctx, to, needed, nil, 21000)
	return err
}

// recoverGasDust sweeps any leftover BNB out of a deposit address back to
// the hot wallet, after its BDT has already been swept to zero. Without
// this, every bit of gas we ever funded (even at the right amount, some
// dust remains from rounding and unused headroom) is gone forever — 10,000
// active users means 10,000 tiny stuck balances that add up.
//
// Leaves a small amount behind (leaveBehind) rather than draining to exactly
// zero, since a zero-BNB address that receives one more deposit needs
// funding again anyway — better to let dust accumulate toward covering that
// than sweep it out and immediately fund it back in.
func (s *Sweeper) recoverGasDust(ctx context.Context, addr SweepAddress) error {
	bnbBal, err := s.client.BalanceAt(ctx, addr.Address, nil)
	if err != nil {
		return err
	}
	leaveBehind := big.NewInt(2e14) // 0.0002 BNB, roughly one more transfer's gas
	if bnbBal.Cmp(leaveBehind) <= 0 {
		return nil // not worth a transaction to move
	}
	amountToRecover := new(big.Int).Sub(bnbBal, leaveBehind)

	account, err := s.addrMgr.PrivateKeyFor(addr.UserID)
	if err != nil {
		return err
	}
	privKey, err := s.addrMgr.wallet.PrivateKey(account)
	if err != nil {
		return err
	}
	nonce, err := s.client.PendingNonceAt(ctx, addr.Address)
	if err != nil {
		return err
	}
	gasPrice, err := s.client.SuggestGasPrice(ctx)
	if err != nil {
		return err
	}
	gasCost := new(big.Int).Mul(gasPrice, big.NewInt(21000))
	sendable := new(big.Int).Sub(amountToRecover, gasCost)
	if sendable.Sign() <= 0 {
		return nil // dust is smaller than the cost of moving it, leave it
	}
	tx := types.NewTx(&types.LegacyTx{
		Nonce: nonce, To: &s.signer.Address, Value: sendable, Gas: 21000, GasPrice: gasPrice,
	})
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(s.chainID), privKey)
	if err != nil {
		return err
	}
	return s.client.SendTransaction(ctx, signedTx)
}
