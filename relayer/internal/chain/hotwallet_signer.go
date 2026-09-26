package chain

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// HotWalletSigner is the ONLY thing in this codebase allowed to sign a
// transaction with the hot wallet's private key. Both the withdraw worker
// and the sweeper's gas-funding step need to send transactions from the
// same hot wallet address — without a shared, mutex-guarded signer, two
// goroutines calling PendingNonceAt at nearly the same moment can both get
// back the same "next nonce", sign two different transactions with it, and
// have one silently dropped by the network. A dropped withdrawal recovers
// on its own (the confirmation poller refunds it), but a dropped
// gas-funding transaction has no such recovery — the sweep it was meant to
// unblock just fails forever with a confusing "insufficient BNB" error.
//
// Locking here is cheap: hot-wallet sends are already rate-limited by
// real-world things (poll intervals, block times), so serializing them
// costs nothing in practice.
type HotWalletSigner struct {
	mu      sync.Mutex
	client  *ethclient.Client
	key     *ecdsa.PrivateKey
	Address common.Address
	chainID *big.Int
}

func NewHotWalletSigner(client *ethclient.Client, privateKeyHex string, chainID *big.Int) (*HotWalletSigner, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(privateKeyHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("invalid hot wallet private key: %w", err)
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)
	return &HotWalletSigner{client: client, key: key, Address: addr, chainID: chainID}, nil
}

// Send builds, signs, and broadcasts a transaction from the hot wallet.
// Safe to call from multiple goroutines — calls are serialized internally.
func (s *HotWalletSigner) Send(ctx context.Context, to common.Address, value *big.Int, data []byte, gasLimit uint64) (*types.Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	nonce, err := s.client.PendingNonceAt(ctx, s.Address)
	if err != nil {
		return nil, fmt.Errorf("get nonce: %w", err)
	}
	gasPrice, err := s.client.SuggestGasPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("get gas price: %w", err)
	}

	tx := types.NewTx(&types.LegacyTx{
		Nonce: nonce, To: &to, Value: value, Gas: gasLimit, GasPrice: gasPrice, Data: data,
	})
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(s.chainID), s.key)
	if err != nil {
		return nil, fmt.Errorf("sign tx: %w", err)
	}
	if err := s.client.SendTransaction(ctx, signedTx); err != nil {
		return nil, fmt.Errorf("broadcast tx (nonce %d): %w", nonce, err)
	}
	return signedTx, nil
}

// SignOnly builds and signs without broadcasting, and also returns the nonce
// used — the withdraw worker needs this so it can record (hash, nonce,
// raw_tx) in the same DB transaction that claims the withdrawal, BEFORE
// broadcasting (see withdraw.go's comment on why that ordering matters).
// The lock is held until Broadcast or Abandon is called, so nothing else can
// grab this nonce out from under the caller in between.
func (s *HotWalletSigner) SignOnly(ctx context.Context, to common.Address, value *big.Int, data []byte, gasLimit uint64) (*types.Transaction, func() error, func(), error) {
	s.mu.Lock()

	nonce, err := s.client.PendingNonceAt(ctx, s.Address)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, nil, fmt.Errorf("get nonce: %w", err)
	}
	gasPrice, err := s.client.SuggestGasPrice(ctx)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, nil, fmt.Errorf("get gas price: %w", err)
	}
	tx := types.NewTx(&types.LegacyTx{
		Nonce: nonce, To: &to, Value: value, Gas: gasLimit, GasPrice: gasPrice, Data: data,
	})
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(s.chainID), s.key)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, nil, fmt.Errorf("sign tx: %w", err)
	}

	broadcast := func() error {
		defer s.mu.Unlock()
		return s.client.SendTransaction(ctx, signedTx)
	}
	abandon := func() { s.mu.Unlock() }

	return signedTx, broadcast, abandon, nil
}
