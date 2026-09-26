package config

import (
	"crypto/ecdsa"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/joho/godotenv"
)

type Config struct {
	// BSC RPC endpoint (use your own node or a provider like Ankr/QuickNode —
	// avoid public free RPCs in production, they rate-limit and drop connections)
	RPCUrl string

	// Deployed BDTToken contract address (from Phase 3 deployment)
	TokenAddress string

	// Hot wallet that holds funds for withdrawals. Its private key signs
	// outgoing transactions. NEVER commit this key or log it. Load from a
	// secrets manager or env var injected at runtime, not from a file in git.
	HotWalletPrivateKey string
	HotWalletAddress    string

	// Postgres connection string
	DatabaseURL string

	// How many block confirmations to wait before crediting a deposit.
	// BSC produces a block every ~3s. 5 is fine on testnet; use 15 or more on
	// mainnet, where a reorg means crediting a deposit that later vanishes.
	// Override with REQUIRED_CONFIRMATIONS.
	RequiredConfirmations uint64
}

// Load reads and validates the environment. It reports every problem at once
// rather than failing on the first one, because the usual way this goes wrong is
// a half-filled Heroku config where three vars are missing.
func Load() (*Config, error) {
	// Loads variables from a .env file in the current directory into the
	// process environment, if one exists. If no .env file is found, this
	// silently does nothing — real deployments (systemd/Docker/Heroku) inject
	// env vars directly without needing a .env file at all.
	_ = godotenv.Load()

	var problems []string
	get := func(key string) string {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			problems = append(problems, "missing required env var: "+key)
		}
		return v
	}

	cfg := &Config{
		RPCUrl:                get("BSC_RPC_URL"),
		TokenAddress:          get("BDT_TOKEN_ADDRESS"),
		HotWalletPrivateKey:   get("HOT_WALLET_PRIVATE_KEY"),
		HotWalletAddress:      get("HOT_WALLET_ADDRESS"),
		DatabaseURL:           get("DATABASE_URL"),
		RequiredConfirmations: 15,
	}

	if v := strings.TrimSpace(os.Getenv("REQUIRED_CONFIRMATIONS")); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n == 0 {
			problems = append(problems, "REQUIRED_CONFIRMATIONS must be a positive integer, got "+v)
		} else {
			cfg.RequiredConfirmations = n
		}
	}

	if cfg.TokenAddress != "" && !common.IsHexAddress(cfg.TokenAddress) {
		problems = append(problems, "BDT_TOKEN_ADDRESS is not a valid 0x… address")
	}
	if cfg.HotWalletAddress != "" && !common.IsHexAddress(cfg.HotWalletAddress) {
		problems = append(problems, "HOT_WALLET_ADDRESS is not a valid 0x… address")
	}

	// Verify the key and the address are the same wallet. A mismatch is quiet
	// and expensive: the sweeper would send gas to one address and sign from
	// another (so sweeps fail), and — worse — the watcher's "is this transfer
	// from our own hot wallet?" check would compare against the wrong address
	// and start crediting our own outgoing float as user deposits, inflating
	// every balance. Fail at startup instead.
	if cfg.HotWalletPrivateKey != "" && cfg.HotWalletAddress != "" {
		key, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.HotWalletPrivateKey, "0x"))
		if err != nil {
			problems = append(problems, "HOT_WALLET_PRIVATE_KEY is not a valid hex private key")
		} else {
			derived := crypto.PubkeyToAddress(*key.Public().(*ecdsa.PublicKey))
			if derived != common.HexToAddress(cfg.HotWalletAddress) {
				problems = append(problems, fmt.Sprintf(
					"HOT_WALLET_PRIVATE_KEY belongs to %s but HOT_WALLET_ADDRESS says %s — they must be the same wallet",
					derived.Hex(), common.HexToAddress(cfg.HotWalletAddress).Hex()))
			}
			// Normalise so string comparisons elsewhere can't be defeated by
			// checksum casing.
			cfg.HotWalletAddress = derived.Hex()
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}
