package chain

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"
	hdwallet "github.com/miguelmota/go-ethereum-hdwallet"
)

// DepositAddressManager derives a unique, deterministic deposit address per
// user from a single mnemonic seed (m/44'/60'/0'/0/{userID}), and persists
// the mapping so the watcher can look it up cheaply.
//
// SECURITY: the mnemonic is the master key to every deposit address AND every
// derived private key. Store it the same way you'd store the hot wallet key —
// secrets manager / HSM, never in code or plaintext config in git. Anyone
// with this mnemonic can drain every deposit address you've ever generated.
type DepositAddressManager struct {
	wallet *hdwallet.Wallet
	pool   *pgxpool.Pool
}

func NewDepositAddressManager(mnemonic string, pool *pgxpool.Pool) (*DepositAddressManager, error) {
	w, err := hdwallet.NewFromMnemonic(mnemonic)
	if err != nil {
		return nil, fmt.Errorf("invalid mnemonic: %w", err)
	}
	return &DepositAddressManager{wallet: w, pool: pool}, nil
}

// GetOrCreateDepositAddress returns the user's deposit address, deriving and
// persisting it on first call. Idempotent — safe to call every time a user
// opens their "deposit" screen.
func (m *DepositAddressManager) GetOrCreateDepositAddress(ctx context.Context, userID int64) (common.Address, error) {
	var existing string
	err := m.pool.QueryRow(ctx,
		`SELECT address FROM deposit_addresses WHERE user_id = $1`, userID,
	).Scan(&existing)
	if err == nil {
		return common.HexToAddress(existing), nil
	}

	path := hdwallet.MustParseDerivationPath(fmt.Sprintf("m/44'/60'/0'/0/%d", userID))
	account, err := m.wallet.Derive(path, false)
	if err != nil {
		return common.Address{}, err
	}

	_, err = m.pool.Exec(ctx,
		`INSERT INTO deposit_addresses (user_id, address, derivation_index) VALUES ($1, $2, $3)
		 ON CONFLICT (user_id) DO NOTHING`,
		userID, account.Address.Hex(), userID,
	)
	if err != nil {
		return common.Address{}, err
	}
	return account.Address, nil
}

// PrivateKeyFor re-derives the private key for a deposit address on demand,
// used only by the sweep job. Never persist derived private keys to disk or DB.
func (m *DepositAddressManager) PrivateKeyFor(userID int64) (accounts.Account, error) {
	path := hdwallet.MustParseDerivationPath(fmt.Sprintf("m/44'/60'/0'/0/%d", userID))
	return m.wallet.Derive(path, false)
}

// --- DepositLookup implementation used by the watcher (internal/chain/watcher.go) ---

type dbDepositLookup struct {
	pool *pgxpool.Pool
}

func NewDBDepositLookup(pool *pgxpool.Pool) DepositLookup {
	return &dbDepositLookup{pool: pool}
}

func (d *dbDepositLookup) UserIDForAddress(ctx context.Context, addr common.Address) (int64, bool, error) {
	var userID int64
	err := d.pool.QueryRow(ctx,
		`SELECT user_id FROM deposit_addresses WHERE address = $1`, addr.Hex(),
	).Scan(&userID)
	if err != nil {
		return 0, false, nil // not found is not an error here — just an address we don't own
	}
	return userID, true, nil
}
