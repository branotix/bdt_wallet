package auth

import (
	"github.com/go-webauthn/webauthn/webauthn"
)

// NewWebAuthn builds the library's config. RPID must be the exact domain
// the app is served from (no scheme, no port) — "localhost" for local
// testing, your real domain in production. RPOrigins must be the exact
// scheme+host+port the browser sends as Origin — WebAuthn deliberately
// fails closed if these don't match exactly, which is the whole point (it's
// what makes a passkey unphishable: it will not sign a challenge for the
// wrong origin, even if a fake site LOOKS identical).
func NewWebAuthn(rpID, rpDisplayName string, rpOrigins []string) (*webauthn.WebAuthn, error) {
	return webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: rpDisplayName,
		RPOrigins:     rpOrigins,
	})
}

// WebAuthnUser adapts our user + their stored credentials to the interface
// go-webauthn's library needs. Nothing sensitive lives here — a public key
// is, by definition, public; there is no PIN or private key anywhere in
// this file.
type WebAuthnUser struct {
	UserID      int64
	Phone       string
	DisplayName string
	Credentials []webauthn.Credential
}

func (u *WebAuthnUser) WebAuthnID() []byte {
	return []byte(intToBytes(u.UserID))
}

func (u *WebAuthnUser) WebAuthnName() string {
	return u.Phone
}

func (u *WebAuthnUser) WebAuthnDisplayName() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Phone
}

func (u *WebAuthnUser) WebAuthnCredentials() []webauthn.Credential {
	return u.Credentials
}

// WebAuthnIcon is required by the v0.10.x User interface but deprecated in
// the WebAuthn spec itself — no client actually uses it anymore. Returning
// empty is correct and safe.
func (u *WebAuthnUser) WebAuthnIcon() string {
	return ""
}

func intToBytes(id int64) []byte {
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = byte(id & 0xff)
		id >>= 8
	}
	return b
}

func BytesToInt(b []byte) int64 {
	var id int64
	for _, c := range b {
		id = (id << 8) | int64(c)
	}
	return id
}
