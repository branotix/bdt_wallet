// Package api exposes the ledger over HTTP and serves the graphical test
// console that drives it.
//
// SECURITY: these endpoints move money and identify the account purely by the
// user_id in the request body. There is no session, so whoever can reach them
// can move anyone's balance.
//
//   - For local testing, bind to 127.0.0.1 and that is the whole security model.
//   - For anything reachable from a network, set API_AUTH_TOKEN. Every request
//     then needs `Authorization: Bearer <token>`, which stops strangers but does
//     NOT separate one user from another — a shared token means every holder is
//     effectively an admin.
//   - Before real users touch this, replace the user_id-in-body scheme with your
//     own auth and derive user_id from the session. See Options.AuthToken.
package api

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"bdt-relayer/internal/fees"
	"bdt-relayer/internal/ledger"
)

//go:embed static/index.html
var indexHTML []byte

// DepositAddresser is the slice of chain.DepositAddressManager this package
// needs. Keeping it an interface lets the console start without a mnemonic
// configured — it just won't be able to show deposit addresses.
type DepositAddresser interface {
	GetOrCreateDepositAddress(ctx context.Context, userID int64) (common.Address, error)
}

// AddressResolver reports whether an address is one of the platform's own
// deposit addresses. Used to refuse withdrawals that would move tokens in a
// circle inside our own custody while still debiting the user.
type AddressResolver interface {
	UserIDForAddress(ctx context.Context, addr common.Address) (int64, bool, error)
}

// Options configures the console. Every field is optional; the zero value gives
// an unauthenticated testnet console with no deposit addresses.
type Options struct {
	// Addresses derives per-user deposit addresses. Nil disables that panel.
	Addresses DepositAddresser
	// Resolver and HotWallet are used to refuse withdrawals aimed back into our
	// own custody. Leaving either unset skips that check, which is not safe.
	Resolver  AddressResolver
	HotWallet common.Address
	// AuthToken, when non-empty, is required as `Authorization: Bearer <token>`
	// on every request. Empty means no authentication at all.
	AuthToken string
	// Network is the label shown in the console header, e.g. "BSC testnet".
	Network string
	// ExplorerBase is the block-explorer origin used for address links, e.g.
	// "https://bscscan.com". No trailing slash.
	ExplorerBase string
	// WebAuthn enables passkey login/registration endpoints when non-nil
	// (see cmd/main.go for setup). Nil is completely safe — phone+PIN login
	// keeps working exactly as before, the passkey routes just don't exist.
	WebAuthn *webauthn.WebAuthn
	// SMSTargetNumber is the phone number (SIM in an Android device running
	// an SMS-forwarder app) users text their verification code to. Empty
	// disables the verify-phone/start endpoint.
	SMSTargetNumber string
	// SMSWebhookSecret gates /api/v1/sms-webhook. Empty disables that route
	// entirely rather than leaving it open — see phone_verify.go.
	SMSWebhookSecret string
	// AllowedOrigins is the explicit CORS allow-list for the mobile API.
	// Empty means same-origin only; use a comma-separated env value in production.
	AllowedOrigins []string
}

type Server struct {
	ledger *ledger.Ledger
	opts   Options
}

func NewServer(l *ledger.Ledger, opts Options) *Server {
	if opts.Network == "" {
		opts.Network = "BSC testnet"
	}
	if opts.ExplorerBase == "" {
		opts.ExplorerBase = "https://testnet.bscscan.com"
	}
	return &Server{ledger: l, opts: opts}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	mux.HandleFunc("POST /api/accounts", s.handleCreateAccount)
	mux.HandleFunc("GET /api/account", s.handleAccountDetail)
	mux.HandleFunc("POST /api/transfer", s.handleTransfer)
	mux.HandleFunc("POST /api/withdraw", s.handleWithdraw)

	// Mobile wallet endpoints have their OWN per-user auth (phone+PIN ->
	// session token, see mobile.go) and must NOT also be gated behind the
	// admin console's shared AuthToken below — a real end user doesn't have
	// that token and shouldn't need it.
	s.RegisterMobileRoutes(mux)
	s.RegisterPasskeyRoutes(mux)
	s.RegisterPhoneVerifyRoutes(mux)
	s.RegisterP2PRoutes(mux)
	s.RegisterAdminP2PRoutes(mux)

	var h http.Handler = mux
	if s.opts.AuthToken != "" {
		h = s.requireToken(h)
	}
	h = s.securityMiddleware(h)
	return h
}

// requireToken gates the admin console behind a shared bearer token.
//
// GET / is deliberately exempt: it is a static page with no data in it, and the
// console needs to load before it can ask the user for the token. Every endpoint
// that reads or moves money is covered. /api/v1/mobile/* is also exempt — those
// routes authenticate each request with its own per-user session token instead.
func (s *Server) requireToken(next http.Handler) http.Handler {
	want := []byte(s.opts.AuthToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/api/v1/mobile/") || r.URL.Path == "/api/v1/sms-webhook" {
			next.ServeHTTP(w, r)
			return
		}
		got := []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		// Constant-time so the comparison cannot be turned into an oracle that
		// leaks the token one byte at a time.
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="bdt-console"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid API token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

// --- console configuration ---

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"fees": map[string]decimal.Decimal{
			"internal_transfer": fees.InternalTransfer,
			"external_withdraw": fees.ExternalWithdraw,
			"deposit":           fees.Deposit,
		},
		"network":          s.opts.Network,
		"explorer_base":    s.opts.ExplorerBase,
		"treasury_user_id": fees.TreasuryUserID,
	})
}

// --- accounts ---

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	userID, err := s.ledger.CreateUser(r.Context())
	if err != nil {
		serverError(w, "create account", err)
		return
	}

	resp := map[string]any{"user_id": userID, "balance": decimal.Zero}
	// Derive the deposit address immediately so a fresh account has somewhere
	// to receive BDT without an extra round trip.
	if s.opts.Addresses != nil {
		if addr, err := s.opts.Addresses.GetOrCreateDepositAddress(r.Context(), userID); err != nil {
			log.Printf("api: deposit address for user %d: %v", userID, err)
		} else {
			resp["deposit_address"] = addr.Hex()
		}
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.ledger.Accounts(r.Context())
	if err != nil {
		serverError(w, "list accounts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":         accounts,
		"treasury_user_id": fees.TreasuryUserID,
	})
}

type accountDetail struct {
	UserID         int64                     `json:"user_id"`
	Balance        decimal.Decimal           `json:"balance"`
	DepositAddress string                    `json:"deposit_address"`
	Entries        []ledger.Entry            `json:"entries"`
	Withdrawals    []ledger.WithdrawalRecord `json:"withdrawals"`
}

func (s *Server) handleAccountDetail(w http.ResponseWriter, r *http.Request) {
	userID, ok := queryUserID(w, r, "user_id")
	if !ok {
		return
	}
	ctx := r.Context()

	balance, err := s.ledger.Balance(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no account with user_id "+strconv.FormatInt(userID, 10))
		return
	}
	if err != nil {
		serverError(w, "read balance", err)
		return
	}

	entries, err := s.ledger.History(ctx, userID, 25)
	if err != nil {
		serverError(w, "read history", err)
		return
	}
	withdrawals, err := s.ledger.Withdrawals(ctx, userID, 10)
	if err != nil {
		serverError(w, "read withdrawals", err)
		return
	}

	detail := accountDetail{
		UserID:      userID,
		Balance:     balance,
		Entries:     entries,
		Withdrawals: withdrawals,
	}
	if s.opts.Addresses != nil {
		if addr, err := s.opts.Addresses.GetOrCreateDepositAddress(ctx, userID); err != nil {
			log.Printf("api: deposit address for user %d: %v", userID, err)
		} else {
			detail.DepositAddress = addr.Hex()
		}
	}
	writeJSON(w, http.StatusOK, detail)
}

// --- internal transfer ---

type transferRequest struct {
	FromUserID int64           `json:"from_user_id"`
	ToUserID   int64           `json:"to_user_id"`
	Amount     decimal.Decimal `json:"amount"`
}

func (s *Server) handleTransfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.FromUserID <= 0 || req.ToUserID <= 0 {
		writeError(w, http.StatusBadRequest, "from_user_id and to_user_id are required")
		return
	}
	if req.FromUserID == req.ToUserID {
		writeError(w, http.StatusBadRequest, "cannot transfer to the same account")
		return
	}
	if err := validateAmount(req.Amount); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	// Both wallet rows must exist before Transfer runs: it only verifies the
	// sender's balance, so a missing receiver row would debit into thin air.
	if !s.requireAccounts(ctx, w, req.FromUserID, req.ToUserID) {
		return
	}

	fee := fees.InternalTransfer
	err := s.ledger.Transfer(ctx, req.FromUserID, req.ToUserID, req.Amount, fee, fees.TreasuryUserID)
	if errors.Is(err, ledger.ErrInsufficientBalance) {
		writeError(w, http.StatusBadRequest, "insufficient balance — need "+req.Amount.Add(fee).String()+" BDT (amount + "+fee.String()+" fee)")
		return
	}
	if err != nil {
		serverError(w, "transfer", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"amount":       req.Amount,
		"fee":          fee,
		"total_debit":  req.Amount.Add(fee),
		"from_user_id": req.FromUserID,
		"to_user_id":   req.ToUserID,
	})
}

// --- external withdrawal ---

type withdrawRequest struct {
	UserID    int64           `json:"user_id"`
	ToAddress string          `json:"to_address"`
	Amount    decimal.Decimal `json:"amount"`
}

func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	var req withdrawRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.UserID <= 0 {
		writeError(w, http.StatusBadRequest, "user_id is required")
		return
	}
	if !common.IsHexAddress(req.ToAddress) {
		writeError(w, http.StatusBadRequest, "to_address is not a valid 0x… address")
		return
	}
	if err := validateAmount(req.Amount); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.requireExternalDestination(r.Context(), w, common.HexToAddress(req.ToAddress)) {
		return
	}

	fee := fees.ExternalWithdraw
	// Reserving debits the balance, credits the fee to the treasury and queues
	// the row, all in one DB transaction — the relayer's withdraw worker is what
	// actually broadcasts it on-chain.
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey != "" && (len(idempotencyKey) < 16 || len(idempotencyKey) > 128) {
		writeError(w, http.StatusBadRequest, "Idempotency-Key must be 16-128 characters")
		return
	}
	id, err := s.ledger.ReserveWithdrawal(r.Context(), req.UserID, req.Amount, fee, req.ToAddress, idempotencyKey, fees.TreasuryUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no account with user_id "+strconv.FormatInt(req.UserID, 10))
		return
	}
	if errors.Is(err, ledger.ErrInsufficientBalance) {
		writeError(w, http.StatusBadRequest, "insufficient balance — need "+req.Amount.Add(fee).String()+" BDT (amount + "+fee.String()+" fee)")
		return
	}
	if err != nil {
		serverError(w, "reserve withdrawal", err)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"withdrawal_id": id,
		"amount":        req.Amount,
		"fee":           fee,
		"total_debit":   req.Amount.Add(fee),
		"status":        "pending",
	})
}

// --- helpers ---

// requireExternalDestination refuses a withdrawal aimed at an address the
// platform already controls.
//
// Withdrawing to the hot wallet is a self-transfer: the tokens never leave our
// custody, but the user's balance is debited anyway. Withdrawing to a deposit
// address is worse — the tokens land somewhere the watcher is listening, and
// the sweeper then pulls them straight back to the hot wallet. Either way the
// on-chain supply is untouched while the ledger moves, which is how a closed
// loop ends up minting balance out of nothing.
func (s *Server) requireExternalDestination(ctx context.Context, w http.ResponseWriter, to common.Address) bool {
	var zero common.Address
	if s.opts.HotWallet != zero && to == s.opts.HotWallet {
		writeError(w, http.StatusBadRequest,
			"that is the platform's own hot wallet — the tokens would never actually leave, but your balance would still be debited")
		return false
	}
	if s.opts.Resolver != nil {
		userID, ours, err := s.opts.Resolver.UserIDForAddress(ctx, to)
		if err != nil {
			serverError(w, "check destination address", err)
			return false
		}
		if ours {
			writeError(w, http.StatusBadRequest,
				"that is user "+strconv.FormatInt(userID, 10)+"'s deposit address — use an internal transfer instead of a withdrawal")
			return false
		}
	}
	return true
}

// requireAccounts 404s unless every listed account has a wallet row.
func (s *Server) requireAccounts(ctx context.Context, w http.ResponseWriter, userIDs ...int64) bool {
	for _, id := range userIDs {
		_, err := s.ledger.Balance(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "no account with user_id "+strconv.FormatInt(id, 10))
			return false
		}
		if err != nil {
			serverError(w, "read balance", err)
			return false
		}
	}
	return true
}

// validateAmount rejects anything the NUMERIC(20,2) ledger columns would
// silently round, so a user is never told they sent 1.005 BDT.
func validateAmount(amount decimal.Decimal) error {
	if !amount.IsPositive() {
		return errors.New("amount must be greater than 0")
	}
	if !amount.Equal(amount.Truncate(2)) {
		return errors.New("amount supports at most 2 decimal places")
	}
	return nil
}

func queryUserID(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	userID, err := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	if err != nil || userID <= 0 {
		writeError(w, http.StatusBadRequest, key+" must be a positive integer")
		return 0, false
	}
	return userID, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("api: write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// serverError logs the real cause and returns a generic message — DB errors
// can carry connection strings and table internals.
func serverError(w http.ResponseWriter, op string, err error) {
	log.Printf("api: %s: %v", op, err)
	writeError(w, http.StatusInternalServerError, op+" failed — check the server log")
}

// Listen starts the console with sane timeouts and shuts down when ctx is done.
func (s *Server) Listen(ctx context.Context, addr string) error {
	cleanupCtx, cleanupCancel := context.WithCancel(ctx)
	defer cleanupCancel()
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-cleanupCtx.Done():
				return
			case <-ticker.C:
				if n, err := s.ledger.CleanupExpiredSessions(cleanupCtx); err != nil {
					log.Printf("api: cleanup expired sessions: %v", err)
				} else if n > 0 {
					log.Printf("api: removed %d expired session(s)", n)
				}
			}
		}
	}()

	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
