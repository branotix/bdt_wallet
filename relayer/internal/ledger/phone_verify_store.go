package ledger

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrVerificationCooldown = errors.New("please wait before requesting another code")
	ErrNoMatchingCode       = errors.New("no matching verification code")
	ErrCodeExpired          = errors.New("verification code expired")
	ErrTooManyAttempts      = errors.New("too many attempts — request a new code")
)

const (
	verificationTTL      = 5 * time.Minute
	verificationCooldown = 45 * time.Second // minimum gap between two /start calls for the same user
	maxMatchAttempts     = 5                // per code, across ALL incoming webhook calls — stops brute-forcing a 6-digit code by flooding the webhook
)

// NormalizePhone collapses the different ways a Bangladeshi number can be
// written ("01XXXXXXXXX", "+8801XXXXXXXXX", "8801XXXXXXXXX", with spaces or
// dashes) into one canonical 13-digit form ("880XXXXXXXXXX"), so the number
// a user registered with and the number an SMS actually arrived from can be
// compared reliably.
func NormalizePhone(raw string) string {
	var digits strings.Builder
	for _, c := range raw {
		if c >= '0' && c <= '9' {
			digits.WriteRune(c)
		}
	}
	d := digits.String()
	switch {
	case strings.HasPrefix(d, "880") && len(d) == 13:
		return d
	case strings.HasPrefix(d, "0") && len(d) == 11:
		return "880" + d[1:]
	case len(d) == 10:
		return "880" + d
	default:
		return d
	}
}

// generateCode returns a random 6-digit numeric string using crypto/rand,
// not math/rand — this is a value someone could brute-force, so it must
// come from a real randomness source.
func generateCode() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	n := (int(b[0])<<16 | int(b[1])<<8 | int(b[2])) % 1000000
	return fmt.Sprintf("%06d", n), nil
}

// CreateVerification generates a new code for a user's claimed phone number.
// Rate-limited per user (verificationCooldown) so this can't be used to spam
// SMS-sending prompts at a victim's real phone number.
func (l *Ledger) CreateVerification(ctx context.Context, userID int64, phone string) (code string, err error) {
	normalized := NormalizePhone(phone)

	var lastCreated time.Time
	err = l.pool.QueryRow(ctx,
		`SELECT created_at FROM phone_verifications WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1`,
		userID,
	).Scan(&lastCreated)
	if err == nil && time.Since(lastCreated) < verificationCooldown {
		return "", ErrVerificationCooldown
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}

	code, err = generateCode()
	if err != nil {
		return "", err
	}

	_, err = l.pool.Exec(ctx,
		`INSERT INTO phone_verifications (user_id, phone, code, expires_at) VALUES ($1, $2, $3, $4)`,
		userID, normalized, code, time.Now().Add(verificationTTL),
	)
	if err != nil {
		return "", err
	}
	return code, nil
}

// MatchIncomingSMS is called by the webhook for every forwarded SMS. It
// looks for the most recent unverified, unexpired code for the sending
// phone number and compares it against the message body. Every call against
// a given pending verification counts as an "attempt" regardless of whether
// it matched — this is what stops someone from just POSTing all million
// possible 6-digit codes to the webhook to force a match.
func (l *Ledger) MatchIncomingSMS(ctx context.Context, fromPhone, body string) (userID int64, err error) {
	normalizedFrom := NormalizePhone(fromPhone)
	submittedCode := extractDigits(body)

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var id int64
	var storedCode string
	var attempts int
	var expiresAt time.Time
	err = tx.QueryRow(ctx,
		`SELECT id, user_id, code, attempts, expires_at FROM phone_verifications
		 WHERE phone = $1 AND verified_at IS NULL
		 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
		normalizedFrom,
	).Scan(&id, &userID, &storedCode, &attempts, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoMatchingCode
	}
	if err != nil {
		return 0, err
	}

	if attempts >= maxMatchAttempts {
		return 0, ErrTooManyAttempts
	}
	if time.Now().After(expiresAt) {
		return 0, ErrCodeExpired
	}

	// Count this attempt regardless of outcome.
	if _, err := tx.Exec(ctx, `UPDATE phone_verifications SET attempts = attempts + 1 WHERE id = $1`, id); err != nil {
		return 0, err
	}

	if submittedCode != storedCode {
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return 0, ErrNoMatchingCode
	}

	if _, err := tx.Exec(ctx, `UPDATE phone_verifications SET verified_at = now() WHERE id = $1`, id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET phone_verified = true WHERE id = $1`, userID); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return userID, nil
}

// VerificationStatus is polled by the frontend to learn when the webhook
// has processed a match.
func (l *Ledger) VerificationStatus(ctx context.Context, userID int64) (verified bool, err error) {
	err = l.pool.QueryRow(ctx, `SELECT phone_verified FROM users WHERE id = $1`, userID).Scan(&verified)
	return verified, err
}

func extractDigits(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}
