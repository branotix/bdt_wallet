// Command api serves the BDT graphical test console.
//
// It only needs Postgres and the deposit-wallet mnemonic — no RPC endpoint and
// no hot wallet key, since it never touches the chain itself. Run it alongside
// `go run ./cmd` (the relayer): this process moves ledger balances and queues
// withdrawals, the relayer does everything on-chain.
//
//	DATABASE_URL=postgres://... DEPOSIT_WALLET_MNEMONIC="..." go run ./cmd/api
//
// Environment:
//
//	DATABASE_URL              required
//	DEPOSIT_WALLET_MNEMONIC   optional; without it deposit addresses are hidden
//	HOT_WALLET_ADDRESS        optional but strongly recommended (see below)
//	API_ADDR                  default 127.0.0.1:8080
//	API_AUTH_TOKEN            required unless bound to loopback
//	BDT_NETWORK               "testnet" (default) or "mainnet" — labels and links
package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"bdt-relayer/internal/api"
	"bdt-relayer/internal/auth"
	"bdt-relayer/internal/chain"
	"bdt-relayer/internal/ledger"
)

func main() {
	// Same .env the relayer reads; missing file is fine when env vars are
	// injected directly (systemd, Docker, Heroku).
	_ = godotenv.Load()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("missing required env var: DATABASE_URL")
	}

	addr := listenAddr()
	authToken := strings.TrimSpace(os.Getenv("API_AUTH_TOKEN"))

	// These endpoints identify the account by the user_id in the request body,
	// with no session behind it. On loopback that is fine — only this machine can
	// reach it. On any other interface it means anyone who finds the port can
	// drain every balance, so refuse to start rather than quietly expose it.
	if !isLoopback(addr) && authToken == "" {
		log.Fatalf("refusing to listen on %s without API_AUTH_TOKEN: these endpoints move money and have no per-user auth.\n"+
			"Either bind to 127.0.0.1 (default) or set API_AUTH_TOKEN to a long random string.", addr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("ping postgres (is ledger/schema.sql loaded?): %v", err)
	}

	opts := api.Options{
		Resolver:       chain.NewDBDepositLookup(pool),
		AuthToken:      authToken,
		AllowedOrigins: splitCSV(os.Getenv("CORS_ALLOWED_ORIGINS")),
	}
	opts.Network, opts.ExplorerBase = networkLabels()

	// Optional: without a mnemonic the console still runs, it just cannot show
	// deposit addresses.
	if mnemonic := os.Getenv("DEPOSIT_WALLET_MNEMONIC"); mnemonic != "" {
		mgr, err := chain.NewDepositAddressManager(mnemonic, pool)
		if err != nil {
			log.Fatalf("deposit address manager: %v", err)
		}
		opts.Addresses = mgr
	} else {
		log.Println("warning: DEPOSIT_WALLET_MNEMONIC not set — deposit addresses disabled")
	}

	// Needed to refuse withdrawals aimed back into our own custody. Without
	// HOT_WALLET_ADDRESS a user can withdraw to the hot wallet itself, which
	// debits their balance while the tokens never leave.
	opts.HotWallet = common.HexToAddress(os.Getenv("HOT_WALLET_ADDRESS"))
	if (opts.HotWallet == common.Address{}) {
		log.Println("warning: HOT_WALLET_ADDRESS not set — cannot reject withdrawals to the hot wallet")
	}

	// Passkeys are entirely optional — unset WEBAUTHN_RPID and the app keeps
	// working exactly as it does today, phone+PIN only.
	//
	// RPID must be the bare domain (no scheme/port) the app is served from —
	// "localhost" for local testing. RPOrigin must be the EXACT
	// scheme+host+port the browser sends as Origin, e.g.
	// "http://localhost:5173" while testing, "https://wallet.yourdomain.com"
	// in production. These two are how WebAuthn refuses to sign a challenge
	// for a phishing site even if it looks pixel-identical — get them wrong
	// and passkeys simply fail closed (safe), not open.
	if rpID := strings.TrimSpace(os.Getenv("WEBAUTHN_RPID")); rpID != "" {
		origin := strings.TrimSpace(os.Getenv("WEBAUTHN_RPORIGIN"))
		if origin == "" {
			log.Fatal("WEBAUTHN_RPID is set but WEBAUTHN_RPORIGIN is not — both are required together")
		}
		wa, err := auth.NewWebAuthn(rpID, "BDT Wallet", []string{origin})
		if err != nil {
			log.Fatalf("webauthn setup: %v", err)
		}
		opts.WebAuthn = wa
		log.Printf("passkey login enabled (RPID=%s, origin=%s)", rpID, origin)
	} else {
		log.Println("WEBAUTHN_RPID not set — passkey login disabled, phone+PIN only (this is fine)")
	}

	opts.SMSTargetNumber = strings.TrimSpace(os.Getenv("SMS_TARGET_NUMBER"))
	opts.SMSWebhookSecret = strings.TrimSpace(os.Getenv("SMS_WEBHOOK_SECRET"))
	if opts.SMSTargetNumber == "" {
		log.Println("SMS_TARGET_NUMBER not set — inbound SMS phone verification disabled (this is fine)")
	}

	srv := api.NewServer(ledger.New(pool), opts)
	if authToken == "" {
		log.Printf("test console on http://%s  (%s, no auth — loopback only)", addr, opts.Network)
	} else {
		log.Printf("test console on http://%s  (%s, bearer token required)", addr, opts.Network)
	}
	if err := srv.Listen(ctx, addr); err != nil {
		log.Fatalf("http server: %v", err)
	}
	log.Println("shutting down")
}

// listenAddr prefers API_ADDR, falls back to PORT (which is how Heroku and most
// PaaS tell a process where to listen), and finally to loopback.
func listenAddr() string {
	if a := strings.TrimSpace(os.Getenv("API_ADDR")); a != "" {
		return a
	}
	if p := strings.TrimSpace(os.Getenv("PORT")); p != "" {
		return ":" + p
	}
	return "127.0.0.1:8080"
}

// isLoopback reports whether addr can only be reached from this machine. An
// empty or wildcard host ("" / "0.0.0.0" / ":8080") is NOT loopback.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false // unparseable, so assume the worst
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func splitCSV(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func networkLabels() (network, explorer string) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("BDT_NETWORK")), "mainnet") {
		return "BSC mainnet", "https://bscscan.com"
	}
	return "BSC testnet", "https://testnet.bscscan.com"
}
