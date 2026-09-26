// Cost-free inbound phone verification. Instead of the backend paying to
// send an OTP, the user sends a short code FROM their own phone TO a fixed
// number; an Android device holding that SIM (running any SMS-forwarder
// app) POSTs the incoming message to /api/v1/sms-webhook.
//
// SECURITY MODEL, read before deploying:
//   - The webhook is NOT protected by a user's session (it can't be — the
//     SMS forwarder app isn't a logged-in user). It IS protected by
//     SMSWebhookSecret, a value only your Android forwarder app should
//     know. Without this, anyone on the internet could POST fake
//     {"from": "...", "body": "123456"} payloads and potentially guess
//     their way to marking phone numbers verified.
//   - A 6-digit code is 1-in-a-million per guess, but that's only safe if
//     guesses are RATE-LIMITED — see maxMatchAttempts in phone_verify_store.go.
//     Without that limit, someone could script a flood of webhook calls
//     trying every code within the 5-minute window.
//   - This verifies "this phone number can currently send SMS to our
//     number", not identity. It does not replace PIN or passkey login.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"bdt-relayer/internal/ledger"
)

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *Server) RegisterPhoneVerifyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/mobile/verify-phone/start", wrap(s.requireMobileAuth(s.handleVerifyPhoneStart)))
	mux.HandleFunc("GET /api/v1/mobile/verify-phone/status", wrap(s.requireMobileAuth(s.handleVerifyPhoneStatus)))

	if s.opts.SMSWebhookSecret == "" {
		log.Println("SMS_WEBHOOK_SECRET not set — /api/v1/sms-webhook is DISABLED (phone verification via SMS will not work, but nothing insecure is exposed)")
		return
	}
	mux.HandleFunc("POST /api/v1/sms-webhook", s.handleSMSWebhook)
}

type verifyPhoneStartRequest struct {
	Phone string `json:"phone"`
}

func (s *Server) handleVerifyPhoneStart(w http.ResponseWriter, r *http.Request) {
	if s.opts.SMSTargetNumber == "" {
		writeError(w, 501, "phone verification is not configured on this server")
		return
	}
	userID := authedUserID(r)
	var req verifyPhoneStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	phone := strings.TrimSpace(req.Phone)
	if phone == "" {
		writeError(w, 400, "phone required")
		return
	}

	code, err := s.ledger.CreateVerification(r.Context(), userID, phone)
	if errors.Is(err, ledger.ErrVerificationCooldown) {
		writeError(w, 429, "please wait a bit before requesting another code")
		return
	}
	if err != nil {
		log.Printf("verify-phone start: %v", err)
		writeError(w, 500, "could not start verification")
		return
	}

	writeJSON(w, 200, map[string]any{
		"code":           code,
		"target_number":  s.opts.SMSTargetNumber,
		"expires_in_sec": 300,
		"instructions":   "এই কোডটি SMS করে পাঠাও এই নাম্বারে, তোমার নিজের ফোন থেকে",
	})
}

func (s *Server) handleVerifyPhoneStatus(w http.ResponseWriter, r *http.Request) {
	userID := authedUserID(r)
	verified, err := s.ledger.VerificationStatus(r.Context(), userID)
	if err != nil {
		writeError(w, 500, "could not check status")
		return
	}
	writeJSON(w, 200, map[string]any{"verified": verified})
}

type smsWebhookPayload struct {
	From string `json:"from"`
	Body string `json:"body"`
}

func (s *Server) handleSMSWebhook(w http.ResponseWriter, r *http.Request) {
	// Constant-time-ish check is less critical here than for the admin
	// token (this isn't gating money movement, just a verification flag),
	// but there's no reason not to reuse the same safe comparison helper.
	got := r.Header.Get("X-Webhook-Secret")
	if got == "" {
		got = r.URL.Query().Get("secret")
	}
	if !constantTimeEqual(got, s.opts.SMSWebhookSecret) {
		writeError(w, 401, "invalid webhook secret")
		return
	}

	var payload smsWebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, 400, "invalid payload")
		return
	}
	if payload.From == "" || payload.Body == "" {
		writeError(w, 400, "from and body are required")
		return
	}

	userID, err := s.ledger.MatchIncomingSMS(r.Context(), payload.From, payload.Body)
	switch {
	case errors.Is(err, ledger.ErrNoMatchingCode):
		// Not an error worth 4xx-ing loudly over — could just be an
		// unrelated text sent to this number. 200 with matched:false tells
		// the forwarder app "received, nothing to do" rather than making it
		// think delivery failed and retry forever.
		writeJSON(w, 200, map[string]any{"matched": false})
		return
	case errors.Is(err, ledger.ErrCodeExpired), errors.Is(err, ledger.ErrTooManyAttempts):
		writeJSON(w, 200, map[string]any{"matched": false, "reason": err.Error()})
		return
	case err != nil:
		log.Printf("sms webhook: %v", err)
		writeError(w, 500, "processing failed")
		return
	}

	log.Printf("phone verified via SMS: user=%d from=%s", userID, payload.From)
	writeJSON(w, 200, map[string]any{"matched": true, "user_id": userID})
}
