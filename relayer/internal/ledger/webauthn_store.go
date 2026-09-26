package ledger

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"

	"bdt-relayer/internal/auth"
)

var ErrNoPasskeys = errors.New("no passkeys registered for this account")

// SaveWebAuthnCredential persists a newly-registered passkey. Called once,
// right after WebAuthn.FinishRegistration succeeds — everything about the
// credential (including that its signature was already verified) has
// already been checked by the library before this is ever called.
func (l *Ledger) SaveWebAuthnCredential(ctx context.Context, userID int64, cred *webauthn.Credential, deviceLabel string) error {
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	_, err := l.pool.Exec(ctx,
		`INSERT INTO webauthn_credentials (user_id, credential_id, public_key, sign_count, transports, device_label)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		userID, cred.ID, cred.PublicKey, cred.Authenticator.SignCount, strings.Join(transports, ","), deviceLabel,
	)
	return err
}

// WebAuthnUserByPhone loads a user plus every passkey they've registered,
// in the shape the go-webauthn library expects. Returns ErrInvalidLogin if
// the phone isn't registered at all (same "don't reveal which part was
// wrong" principle as PIN login), and ErrNoPasskeys if the account exists
// but has never registered one — the caller should fall back to PIN login
// in that case, not treat it as a hard failure.
func (l *Ledger) WebAuthnUserByPhone(ctx context.Context, phone string) (*auth.WebAuthnUser, error) {
	var userID int64
	var displayName string
	err := l.pool.QueryRow(ctx, `SELECT id, COALESCE(display_name, '') FROM users WHERE phone = $1`, phone).
		Scan(&userID, &displayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidLogin
	}
	if err != nil {
		return nil, err
	}

	creds, err := l.webAuthnCredentialsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(creds) == 0 {
		return nil, ErrNoPasskeys
	}

	return &auth.WebAuthnUser{UserID: userID, Phone: phone, DisplayName: displayName, Credentials: creds}, nil
}

// WebAuthnUserByID is the same as WebAuthnUserByPhone but for use during
// registration, when the caller already has an authenticated session
// (they're adding a NEW passkey to an account they're already logged into
// via PIN) rather than a phone number from an unauthenticated request.
func (l *Ledger) WebAuthnUserByID(ctx context.Context, userID int64) (*auth.WebAuthnUser, error) {
	var phone, displayName string
	err := l.pool.QueryRow(ctx, `SELECT phone, COALESCE(display_name, '') FROM users WHERE id = $1`, userID).
		Scan(&phone, &displayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	creds, err := l.webAuthnCredentialsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &auth.WebAuthnUser{UserID: userID, Phone: phone, DisplayName: displayName, Credentials: creds}, nil
}

func (l *Ledger) webAuthnCredentialsForUser(ctx context.Context, userID int64) ([]webauthn.Credential, error) {
	rows, err := l.pool.Query(ctx,
		`SELECT credential_id, public_key, sign_count, transports FROM webauthn_credentials WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var creds []webauthn.Credential
	for rows.Next() {
		var id, pubKey []byte
		var signCount uint32
		var transportsRaw string
		if err := rows.Scan(&id, &pubKey, &signCount, &transportsRaw); err != nil {
			return nil, err
		}
		var transports []protocol.AuthenticatorTransport
		if transportsRaw != "" {
			for _, t := range strings.Split(transportsRaw, ",") {
				transports = append(transports, protocol.AuthenticatorTransport(t))
			}
		}
		creds = append(creds, webauthn.Credential{
			ID:        id,
			PublicKey: pubKey,
			Transport: transports,
			Authenticator: webauthn.Authenticator{
				SignCount: signCount,
			},
		})
	}
	return creds, nil
}

// UpdateSignCount is called after every successful passkey login. The sign
// counter is how a real, compromised-and-cloned authenticator gets caught:
// a legitimate device's counter only ever goes up. If a login ever presents
// a counter that is NOT greater than what's stored, the library flags
// CloneWarning — see passkey.go's login-finish handler for what happens
// when it does.
func (l *Ledger) UpdateSignCount(ctx context.Context, credentialID []byte, newCount uint32) error {
	_, err := l.pool.Exec(ctx,
		`UPDATE webauthn_credentials SET sign_count = $1, last_used_at = $2 WHERE credential_id = $3`,
		newCount, time.Now(), credentialID,
	)
	return err
}

// HasPasskey is a cheap existence check the frontend can use to decide
// whether to show a "Login with Passkey" option before the user even types
// a PIN.
func (l *Ledger) HasPasskey(ctx context.Context, phone string) (bool, error) {
	var count int
	err := l.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM webauthn_credentials wc JOIN users u ON u.id = wc.user_id WHERE u.phone = $1`,
		phone,
	).Scan(&count)
	return count > 0, err
}
