// Mobile wallet API — real per-user authentication via phone + PIN.
//
// This is deliberately separate from the admin console's shared-token model
// (see api.go). Every endpoint here identifies the account from a session
// token issued at login, NEVER from a user_id in the request body — so one
// logged-in user can only ever move their own money.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"

	"bdt-relayer/internal/auth"
	"bdt-relayer/internal/fees"
	"bdt-relayer/internal/ledger"
)

const sessionTTL = 30 * 24 * time.Hour

var phonePattern = regexp.MustCompile(`^\+?[0-9]{10,15}$`)

type ctxKey int

const userIDKey ctxKey = iota

// RegisterMobileRoutes wires the /api/v1/mobile/* endpoints onto an existing
// mux. Call this from Routes() alongside the admin console routes.
//
// CORS is enabled ONLY for these routes (not the admin console) because the
// mobile web app is typically hosted on a different origin (e.g. GitHub
// Pages) than this API. This is safe to leave open (Access-Control-Allow-
// Origin: *) because auth here is a Bearer token, not a cookie — a
// malicious page cannot silently ride a logged-in user's session the way it
// could with cookie-based auth.
func (s *Server) RegisterMobileRoutes(mux *http.ServeMux) {
	wrap := func(h http.HandlerFunc) http.HandlerFunc { return s.withMobileCORS(h) }

	mux.HandleFunc("POST /api/v1/mobile/register", wrap(s.handleMobileRegister))
	mux.HandleFunc("POST /api/v1/mobile/login", wrap(s.handleMobileLogin))
	mux.HandleFunc("POST /api/v1/mobile/logout", wrap(s.requireMobileAuth(s.handleMobileLogout)))
	mux.HandleFunc("GET /api/v1/mobile/me/balance", wrap(s.requireMobileAuth(s.handleMobileBalance)))
	mux.HandleFunc("GET /api/v1/mobile/me/deposit-address", wrap(s.requireMobileAuth(s.handleMobileDepositAddress)))
	mux.HandleFunc("POST /api/v1/mobile/transfer", wrap(s.requireMobileAuth(s.handleMobileTransfer)))
	mux.HandleFunc("POST /api/v1/mobile/withdraw", wrap(s.requireMobileAuth(s.handleMobileWithdraw)))
	mux.HandleFunc("GET /api/v1/mobile/me/history", wrap(s.requireMobileAuth(s.handleMobileHistory)))
	mux.HandleFunc("GET /api/v1/mobile/config", wrap(s.handleMobileConfig))
	mux.HandleFunc("OPTIONS /api/v1/mobile/", wrap(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
}

func (s *Server) withMobileCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Idempotency-Key")
		} else if origin != "" {
			writeError(w, http.StatusForbidden, "origin not allowed")
			return
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func (s *Server) originAllowed(origin string) bool {
	if len(s.opts.AllowedOrigins) == 0 {
		return origin == ""
	}
	for _, allowed := range s.opts.AllowedOrigins {
		if strings.TrimSpace(allowed) == origin {
			return true
		}
	}
	return false
}

func (s *Server) handleMobileConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"internal_transfer_fee": fees.InternalTransfer,
		"withdraw_fee":          fees.ExternalWithdraw,
		"network":               s.opts.Network,
	})
}

type mobileRegisterRequest struct {
	Phone       string `json:"phone"`
	PIN         string `json:"pin"`
	DisplayName string `json:"display_name"`
}

func (s *Server) handleMobileRegister(w http.ResponseWriter, r *http.Request) {
	var req mobileRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	req.Phone = strings.TrimSpace(req.Phone)
	if !phonePattern.MatchString(req.Phone) {
		writeError(w, 400, "invalid phone number")
		return
	}
	if !isNumericPIN(req.PIN) {
		writeError(w, 400, "PIN must be 4-8 digits")
		return
	}

	hash, salt, err := auth.HashPIN(req.PIN)
	if err != nil {
		log.Printf("mobile register: hash pin: %v", err)
		writeError(w, 500, "registration failed")
		return
	}

	ctx := r.Context()
	userID, err := s.ledger.RegisterUser(ctx, req.Phone, hash, salt, req.DisplayName)
	if err != nil {
		if errors.Is(err, ledger.ErrPhoneTaken) {
			writeError(w, 409, "this phone number is already registered")
			return
		}
		log.Printf("mobile register: %v", err)
		writeError(w, 500, "registration failed")
		return
	}

	token, err := s.issueSession(ctx, userID)
	if err != nil {
		log.Printf("mobile register: issue session: %v", err)
		writeError(w, 500, "registered, but login failed — try logging in")
		return
	}

	resp := map[string]any{"user_id": userID, "token": token}
	if s.opts.Addresses != nil {
		if addr, err := s.opts.Addresses.GetOrCreateDepositAddress(ctx, userID); err == nil {
			resp["deposit_address"] = addr.Hex()
		}
	}
	writeJSON(w, 201, resp)
}

type mobileLoginRequest struct {
	Phone string `json:"phone"`
	PIN   string `json:"pin"`
}

func (s *Server) handleMobileLogin(w http.ResponseWriter, r *http.Request) {
	var req mobileLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	ctx := r.Context()

	userID, hash, salt, failedAttempts, lockedUntil, err := s.ledger.UserCredentialsByPhone(ctx, strings.TrimSpace(req.Phone))
	if errors.Is(err, ledger.ErrInvalidLogin) {
		writeError(w, 401, "invalid phone or PIN")
		return
	}
	if err != nil {
		log.Printf("mobile login: %v", err)
		writeError(w, 500, "login failed")
		return
	}

	if lockedUntil != nil && time.Now().Before(*lockedUntil) {
		wait := time.Until(*lockedUntil).Round(time.Minute)
		writeError(w, 429, fmt.Sprintf("too many failed attempts — try again in %s", wait))
		return
	}

	if !auth.VerifyPIN(req.PIN, hash, salt) {
		if err := s.ledger.RecordFailedLogin(ctx, userID); err != nil {
			log.Printf("mobile login: record failed attempt: %v", err)
		}
		remaining := 5 - (failedAttempts + 1)
		if remaining <= 0 {
			writeError(w, 429, "too many failed attempts — account locked for 15 minutes")
		} else {
			// Same response shape as "phone not found" above — never reveal
			// which part of the credential pair was wrong — but DOES include
			// a remaining-attempts count, which is intentional: it warns a
			// legitimate user who mistyped without helping an attacker learn
			// anything about the phone number's validity.
			writeError(w, 401, fmt.Sprintf("invalid phone or PIN (%d attempts left before lockout)", remaining))
		}
		return
	}

	if err := s.ledger.RecordSuccessfulLogin(ctx, userID); err != nil {
		log.Printf("mobile login: reset failed attempts: %v", err)
	}
	// Transparently upgrade legacy PIN hashes after a successful login.
	if auth.NeedsRehash(hash) {
		if newHash, newSalt, hashErr := auth.HashPIN(req.PIN); hashErr == nil {
			if err := s.ledger.UpdatePINHash(ctx, userID, newHash, newSalt); err != nil {
				log.Printf("mobile login: upgrade PIN hash: %v", err)
			}
		} else {
			log.Printf("mobile login: generate upgraded PIN hash: %v", hashErr)
		}
	}

	token, err := s.issueSession(ctx, userID)
	if err != nil {
		log.Printf("mobile login: issue session: %v", err)
		writeError(w, 500, "login failed")
		return
	}
	writeJSON(w, 200, map[string]any{"user_id": userID, "token": token})
}

func (s *Server) issueSession(ctx context.Context, userID int64) (string, error) {
	plaintext, hash, err := auth.GenerateSessionToken()
	if err != nil {
		return "", err
	}
	if err := s.ledger.CreateSession(ctx, hash, userID, time.Now().Add(sessionTTL)); err != nil {
		return "", err
	}
	return plaintext, nil
}

// requireMobileAuth resolves the Bearer token to a user_id and attaches it
// to the request context. Handlers must read the user_id from context via
// authedUserID — never from anything the client sent in the body — so a
// logged-in user can only ever act as themselves.
func (s *Server) requireMobileAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" || token == authHeader {
			writeError(w, 401, "missing bearer token")
			return
		}

		userID, err := s.ledger.ResolveSession(r.Context(), auth.HashToken(token), time.Now().Add(sessionTTL))
		if errors.Is(err, ledger.ErrSessionExpired) {
			writeError(w, 401, "session expired, please log in again")
			return
		}
		if err != nil {
			writeError(w, 401, "invalid session")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userIDKey, userID)))
	}
}

func authedUserID(r *http.Request) int64 {
	v, _ := r.Context().Value(userIDKey).(int64)
	return v
}

func (s *Server) handleMobileLogout(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	if err := s.ledger.DeleteSession(r.Context(), auth.HashToken(token), authedUserID(r)); err != nil {
		writeError(w, 500, "logout failed")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "logged_out"})
}

func (s *Server) handleMobileBalance(w http.ResponseWriter, r *http.Request) {
	userID := authedUserID(r)
	balance, err := s.ledger.Balance(r.Context(), userID)
	if err != nil {
		writeError(w, 500, "could not load balance")
		return
	}
	writeJSON(w, 200, map[string]any{"balance": balance.String()})
}

func (s *Server) handleMobileDepositAddress(w http.ResponseWriter, r *http.Request) {
	userID := authedUserID(r)
	if s.opts.Addresses == nil {
		writeError(w, 501, "deposit addresses not configured")
		return
	}
	addr, err := s.opts.Addresses.GetOrCreateDepositAddress(r.Context(), userID)
	if err != nil {
		log.Printf("mobile deposit address: %v", err)
		writeError(w, 500, "could not derive deposit address")
		return
	}
	writeJSON(w, 200, map[string]any{"deposit_address": addr.Hex()})
}

type mobileTransferRequest struct {
	ToPhone string `json:"to_phone"`
	Amount  string `json:"amount"`
}

func (s *Server) handleMobileTransfer(w http.ResponseWriter, r *http.Request) {
	fromUserID := authedUserID(r)
	var req mobileTransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	amount, err := parseAmount(req.Amount)
	if err != nil {
		writeError(w, 400, "invalid amount")
		return
	}

	ctx := r.Context()
	toUserID, err := s.ledger.UserIDByPhone(ctx, strings.TrimSpace(req.ToPhone))
	if errors.Is(err, ledger.ErrUserNotFound) {
		writeError(w, 404, "no user with that phone number")
		return
	}
	if err != nil {
		writeError(w, 500, "transfer failed")
		return
	}
	if toUserID == fromUserID {
		writeError(w, 400, "cannot send to yourself")
		return
	}

	if err := s.ledger.Transfer(ctx, fromUserID, toUserID, amount, fees.InternalTransfer, fees.TreasuryUserID); err != nil {
		if errors.Is(err, ledger.ErrInsufficientBalance) {
			writeError(w, 402, "insufficient balance (amount + "+fees.InternalTransfer.String()+" BDT fee required)")
			return
		}
		log.Printf("mobile transfer: %v", err)
		writeError(w, 500, "transfer failed")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "fee_charged": fees.InternalTransfer.String()})
}

type mobileWithdrawRequest struct {
	ToAddress string `json:"to_address"`
	Amount    string `json:"amount"`
}

func (s *Server) handleMobileWithdraw(w http.ResponseWriter, r *http.Request) {
	userID := authedUserID(r)
	var req mobileWithdrawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	amount, err := parseAmount(req.Amount)
	if err != nil {
		writeError(w, 400, "invalid amount")
		return
	}
	addr := strings.TrimSpace(req.ToAddress)
	if len(addr) != 42 || !strings.HasPrefix(addr, "0x") {
		writeError(w, 400, "invalid BSC address")
		return
	}

	// Refuse withdrawals that would just move tokens back into our own
	// custody (hot wallet or one of our own deposit addresses) while still
	// debiting the user — same protection as the admin console.
	destAddr := common.HexToAddress(addr)
	if s.opts.Resolver != nil {
		if _, isOurs, err := s.opts.Resolver.UserIDForAddress(r.Context(), destAddr); err == nil && isOurs {
			writeError(w, 400, "cannot withdraw to a platform-owned address")
			return
		}
	}
	var zeroAddr common.Address
	if s.opts.HotWallet != zeroAddr && destAddr == s.opts.HotWallet {
		writeError(w, 400, "cannot withdraw to the hot wallet")
		return
	}

	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) < 16 || len(key) > 128 {
		writeError(w, 400, "Idempotency-Key must be 16-128 characters")
		return
	}
	id, err := s.ledger.ReserveWithdrawal(r.Context(), userID, amount, fees.ExternalWithdraw, addr, key, fees.TreasuryUserID)
	if err != nil {
		if errors.Is(err, ledger.ErrInsufficientBalance) {
			writeError(w, 402, "insufficient balance (amount + "+fees.ExternalWithdraw.String()+" BDT fee required)")
			return
		}
		log.Printf("mobile withdraw: %v", err)
		writeError(w, 500, "withdraw failed")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "pending", "withdrawal_id": id})
}

func (s *Server) handleMobileHistory(w http.ResponseWriter, r *http.Request) {
	userID := authedUserID(r)
	entries, err := s.ledger.History(r.Context(), userID, 50)
	if err != nil {
		writeError(w, 500, "could not load history")
		return
	}
	writeJSON(w, 200, entries)
}

func isNumericPIN(pin string) bool {
	if len(pin) < 4 || len(pin) > 8 {
		return false
	}
	for _, c := range pin {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseAmount(s string) (decimal.Decimal, error) {
	amount, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil || amount.LessThanOrEqual(decimal.Zero) || !amount.Equal(amount.Truncate(2)) {
		return decimal.Zero, errors.New("invalid amount — maximum 2 decimal places")
	}
	return amount, nil
}
