package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrPhoneTaken     = errors.New("phone number already registered")
	ErrInvalidLogin   = errors.New("invalid phone or PIN")
	ErrAccountLocked  = errors.New("account temporarily locked from too many failed attempts")
	ErrSessionExpired = errors.New("session expired")
	ErrUserNotFound   = errors.New("user not found")
)

// RegisterUser creates a user + wallet row with phone/PIN credentials
// already hashed by the caller (see internal/auth). Returns the new user_id.
func (l *Ledger) RegisterUser(ctx context.Context, phone, pinHash, pinSalt, displayName string) (int64, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var userID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO users (phone, pin_hash, pin_salt, display_name) VALUES ($1, $2, $3, $4) RETURNING id`,
		phone, pinHash, pinSalt, displayName,
	).Scan(&userID)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrPhoneTaken
		}
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO wallets (user_id, balance) VALUES ($1, 0)`, userID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return userID, nil
}

// UserCredentialsByPhone returns what's needed to verify a login attempt,
// plus lockout state — the caller must check IsLocked before calling
// VerifyPIN, and must call RecordFailedLogin / RecordSuccessfulLogin
// afterward so repeated wrong guesses actually cost something.
func (l *Ledger) UserCredentialsByPhone(ctx context.Context, phone string) (userID int64, pinHash, pinSalt string, failedAttempts int, lockedUntil *time.Time, err error) {
	err = l.pool.QueryRow(ctx,
		`SELECT id, pin_hash, pin_salt, failed_login_attempts, locked_until FROM users WHERE phone = $1`, phone,
	).Scan(&userID, &pinHash, &pinSalt, &failedAttempts, &lockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", "", 0, nil, ErrInvalidLogin
	}
	return userID, pinHash, pinSalt, failedAttempts, lockedUntil, err
}

const (
	maxFailedLoginAttempts = 5
	lockoutDuration        = 15 * time.Minute
)

// RecordFailedLogin increments the counter and, once it crosses the
// threshold, locks the account for lockoutDuration. This is what makes
// "just guess the PIN" stop working after a handful of tries — without it,
// a 4-digit PIN is only 10,000 guesses, trivial to brute-force with no rate
// limit at all.
func (l *Ledger) RecordFailedLogin(ctx context.Context, userID int64) error {
	_, err := l.pool.Exec(ctx, `
		UPDATE users
		SET failed_login_attempts = failed_login_attempts + 1,
		    locked_until = CASE
		        WHEN failed_login_attempts + 1 >= $2 THEN now() + $3::interval
		        ELSE locked_until
		    END
		WHERE id = $1`,
		userID, maxFailedLoginAttempts, lockoutDuration,
	)
	return err
}

// RecordSuccessfulLogin resets the failure counter — a correct PIN always
// clears past failed attempts, it doesn't just let the lock passively expire.
func (l *Ledger) RecordSuccessfulLogin(ctx context.Context, userID int64) error {
	_, err := l.pool.Exec(ctx,
		`UPDATE users SET failed_login_attempts = 0, locked_until = NULL WHERE id = $1`, userID,
	)
	return err
}

// UserIDByPhone looks up a user for sending a transfer to them by phone.
func (l *Ledger) UserIDByPhone(ctx context.Context, phone string) (int64, error) {
	var id int64
	err := l.pool.QueryRow(ctx, `SELECT id FROM users WHERE phone = $1`, phone).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrUserNotFound
	}
	return id, err
}

// UpdatePINHash upgrades a legacy credential representation after a verified login.
func (l *Ledger) UpdatePINHash(ctx context.Context, userID int64, pinHash, pinSalt string) error {
	_, err := l.pool.Exec(ctx, `UPDATE users SET pin_hash = $1, pin_salt = $2 WHERE id = $3`, pinHash, pinSalt, userID)
	return err
}

// CreateSession stores a session token hash with an expiry.
func (l *Ledger) CreateSession(ctx context.Context, tokenHash string, userID int64, expiresAt time.Time) error {
	_, err := l.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, userID, expiresAt,
	)
	return err
}

// ResolveSession returns the user_id for a valid, non-expired session token
// hash, and slides the expiry forward so active users stay logged in.

// DeleteSession revokes one bearer session immediately.
func (l *Ledger) DeleteSession(ctx context.Context, tokenHash string, userID int64) error {
	_, err := l.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1 AND user_id = $2`, tokenHash, userID)
	return err
}

// CleanupExpiredSessions removes stale bearer sessions. Call periodically from
// the API process; this keeps the sessions table bounded.
func (l *Ledger) CleanupExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := l.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (l *Ledger) ResolveSession(ctx context.Context, tokenHash string, newExpiry time.Time) (int64, error) {
	var userID int64
	var expiresAt time.Time
	err := l.pool.QueryRow(ctx,
		`SELECT user_id, expires_at FROM sessions WHERE token_hash = $1`, tokenHash,
	).Scan(&userID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInvalidLogin
	}
	if err != nil {
		return 0, err
	}
	if time.Now().After(expiresAt) {
		return 0, ErrSessionExpired
	}
	_, _ = l.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = now(), expires_at = $1 WHERE token_hash = $2`, newExpiry, tokenHash)
	return userID, nil
}

// TransferHistory and Balance already exist on Ledger in this codebase
// (History/Balance) — reused as-is by the mobile handlers, not redefined here.

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
