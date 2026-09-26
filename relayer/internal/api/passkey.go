// Passkey endpoints. Adds passkey login ALONGSIDE phone+PIN (migrate-003) —
// it never replaces it. A user always has PIN as a fallback, so a lost
// device or a bug in this code can never lock someone out of their own
// money; worst case, they just log in with PIN like before.
//
// The actual cryptographic verification (attestation/assertion signatures,
// origin/RPID matching, challenge freshness) is entirely handled by
// github.com/go-webauthn/webauthn, not by anything in this file — that
// library is widely used and independently audited, which is deliberate:
// hand-rolling WebAuthn's signature verification ourselves is exactly the
// kind of security-critical code that should NOT be written from scratch
// without extensive testing.
package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"bdt-relayer/internal/ledger"
)

// ceremonyStore holds in-progress WebAuthn ceremonies (the challenge issued
// by "begin", needed again by "finish"). In-memory is fine here — a
// ceremony is a single login/registration attempt that completes within
// seconds, and losing it on a restart just means the user retries, no data
// is at risk. Keyed by phone (login) or "user:<id>" (registration).
type ceremonyStore struct {
	mu      sync.Mutex
	entries map[string]ceremonyEntry
}

type ceremonyEntry struct {
	session *webauthn.SessionData
	expires time.Time
}

func newCeremonyStore() *ceremonyStore {
	return &ceremonyStore{entries: make(map[string]ceremonyEntry)}
}

func (c *ceremonyStore) put(key string, session *webauthn.SessionData) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = ceremonyEntry{session: session, expires: time.Now().Add(2 * time.Minute)}
}

func (c *ceremonyStore) take(key string) (*webauthn.SessionData, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	delete(c.entries, key) // one-shot: a challenge is used at most once, replay is not allowed
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.session, true
}

var passkeyCeremonies = newCeremonyStore()

func (s *Server) RegisterPasskeyRoutes(mux *http.ServeMux) {
	if s.opts.WebAuthn == nil {
		return // passkeys not configured (see cmd/main.go) — routes simply don't exist, PIN-only still works fine
	}
	mux.HandleFunc("POST /api/v1/mobile/passkey/register/begin", wrap(s.requireMobileAuth(s.handlePasskeyRegisterBegin)))
	mux.HandleFunc("POST /api/v1/mobile/passkey/register/finish", wrap(s.requireMobileAuth(s.handlePasskeyRegisterFinish)))
	mux.HandleFunc("POST /api/v1/mobile/passkey/login/begin", wrap(s.handlePasskeyLoginBegin))
	mux.HandleFunc("POST /api/v1/mobile/passkey/login/finish", wrap(s.handlePasskeyLoginFinish))
	mux.HandleFunc("GET /api/v1/mobile/passkey/available", wrap(s.handlePasskeyAvailable))
}

func wrap(h http.HandlerFunc) http.HandlerFunc { return withMobileCORS(h) }

// withMobileCORS provides basic CORS handling for mobile passkey requests.
func withMobileCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		h(w, r)
	}
}

// --- Registration (adding a passkey to an ALREADY-logged-in account) ---

func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	userID := authedUserID(r)
	user, err := s.ledger.WebAuthnUserByID(r.Context(), userID)
	if err != nil {
		log.Printf("passkey register begin: %v", err)
		writeError(w, 500, "could not start passkey setup")
		return
	}

	options, session, err := s.opts.WebAuthn.BeginRegistration(user)
	if err != nil {
		log.Printf("passkey register begin: %v", err)
		writeError(w, 500, "could not start passkey setup")
		return
	}
	passkeyCeremonies.put(ceremonyKeyForUser(userID), session)
	writeJSON(w, 200, options)
}

func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	userID := authedUserID(r)
	session, ok := passkeyCeremonies.take(ceremonyKeyForUser(userID))
	if !ok {
		writeError(w, 400, "passkey setup expired or was never started — try again")
		return
	}
	user, err := s.ledger.WebAuthnUserByID(r.Context(), userID)
	if err != nil {
		writeError(w, 500, "could not verify passkey")
		return
	}

	cred, err := s.opts.WebAuthn.FinishRegistration(user, *session, r)
	if err != nil {
		// A failure here means the signature, origin, or challenge did not
		// check out — never treat this as "close enough". Reject outright.
		log.Printf("passkey register finish: verification failed: %v", err)
		writeError(w, 400, "could not verify this passkey — please try again")
		return
	}

	label := r.URL.Query().Get("label")
	if label == "" {
		label = "Passkey"
	}
	if err := s.ledger.SaveWebAuthnCredential(r.Context(), userID, cred, label); err != nil {
		log.Printf("passkey register finish: save: %v", err)
		writeError(w, 500, "passkey verified but could not be saved")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok"})
}

// --- Login (no session yet — this IS how one gets created) ---

func (s *Server) handlePasskeyAvailable(w http.ResponseWriter, r *http.Request) {
	phone := strings.TrimSpace(r.URL.Query().Get("phone"))
	if phone == "" {
		writeError(w, 400, "phone required")
		return
	}
	has, err := s.ledger.HasPasskey(r.Context(), phone)
	if err != nil {
		writeError(w, 500, "check failed")
		return
	}
	writeJSON(w, 200, map[string]any{"available": has})
}

type passkeyLoginBeginRequest struct {
	Phone string `json:"phone"`
}

func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	var req passkeyLoginBeginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	phone := strings.TrimSpace(req.Phone)

	user, err := s.ledger.WebAuthnUserByPhone(r.Context(), phone)
	if errors.Is(err, ledger.ErrInvalidLogin) || errors.Is(err, ledger.ErrNoPasskeys) {
		// Same generic response either way — don't reveal whether the phone
		// exists or just has no passkey registered.
		writeError(w, 401, "passkey login not available for this account")
		return
	}
	if err != nil {
		log.Printf("passkey login begin: %v", err)
		writeError(w, 500, "login failed")
		return
	}

	options, session, err := s.opts.WebAuthn.BeginLogin(user)
	if err != nil {
		log.Printf("passkey login begin: %v", err)
		writeError(w, 500, "login failed")
		return
	}
	passkeyCeremonies.put(ceremonyKeyForPhone(phone), session)
	writeJSON(w, 200, options)
}

func (s *Server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	phone := strings.TrimSpace(r.URL.Query().Get("phone"))
	if phone == "" {
		writeError(w, 400, "phone required")
		return
	}
	session, ok := passkeyCeremonies.take(ceremonyKeyForPhone(phone))
	if !ok {
		writeError(w, 400, "passkey login expired or was never started — try again")
		return
	}
	user, err := s.ledger.WebAuthnUserByPhone(r.Context(), phone)
	if err != nil {
		writeError(w, 500, "login failed")
		return
	}

	cred, err := s.opts.WebAuthn.FinishLogin(user, *session, r)
	if err != nil {
		log.Printf("passkey login finish: verification failed: %v", err)
		writeError(w, 401, "passkey verification failed")
		return
	}

	// Clone-detection: a legitimate authenticator's sign counter only ever
	// increases. If FinishLogin flags this, the same credential answered a
	// challenge with a counter that didn't advance as expected — consistent
	// with the credential having been cloned onto a second device. This does
	// NOT necessarily mean an attack (some authenticators legitimately don't
	// implement counters and report 0 every time), so it's logged loudly for
	// manual review rather than auto-locking the account.
	if cred.Authenticator.CloneWarning {
		log.Printf("SECURITY: passkey clone warning for phone=%s credential=%x — review before trusting this login",
			phone, cred.ID)
	}
	if err := s.ledger.UpdateSignCount(r.Context(), cred.ID, cred.Authenticator.SignCount); err != nil {
		log.Printf("passkey login finish: update sign count: %v", err)
	}

	token, err := s.issueSession(r.Context(), user.UserID)
	if err != nil {
		log.Printf("passkey login finish: issue session: %v", err)
		writeError(w, 500, "login failed")
		return
	}
	writeJSON(w, 200, map[string]any{"user_id": user.UserID, "token": token})
}

func ceremonyKeyForUser(userID int64) string  { return "user:" + strconv.FormatInt(userID, 10) }
func ceremonyKeyForPhone(phone string) string { return "phone:" + phone }
