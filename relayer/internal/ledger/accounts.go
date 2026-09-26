package ledger

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// Account is a user plus their cached wallet balance.
type Account struct {
	UserID    int64           `json:"user_id"`
	Balance   decimal.Decimal `json:"balance"`
	CreatedAt time.Time       `json:"created_at"`
}

// Entry is one row of the double-entry ledger, as shown in a user's history.
// Amount is signed: positive is a credit, negative a debit.
type Entry struct {
	ID          int64           `json:"id"`
	Amount      decimal.Decimal `json:"amount"`
	EntryType   string          `json:"entry_type"`
	ReferenceID string          `json:"reference_id"`
	CreatedAt   time.Time       `json:"created_at"`
}

// WithdrawalRecord is a withdrawal request and where it currently is in the
// pending -> broadcasting -> confirmed/failed lifecycle.
type WithdrawalRecord struct {
	ID        int64           `json:"id"`
	Amount    decimal.Decimal `json:"amount"`
	Fee       decimal.Decimal `json:"fee"`
	ToAddress string          `json:"to_address"`
	Status    string          `json:"status"`
	TxHash    string          `json:"tx_hash"`
	CreatedAt time.Time       `json:"created_at"`
}

// CreateUser inserts a user and their wallet row in a single transaction.
// Both must exist before anything moves money: Transfer locks wallet rows by
// user_id, and a missing row would let a credit silently update nothing.
func (l *Ledger) CreateUser(ctx context.Context) (int64, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) // no-op if committed

	var userID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO users DEFAULT VALUES RETURNING id`,
	).Scan(&userID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO wallets (user_id, balance) VALUES ($1, 0)`, userID,
	); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return userID, nil
}

// Balance returns the cached wallet balance. Returns pgx.ErrNoRows if the
// account (or its wallet row) does not exist.
func (l *Ledger) Balance(ctx context.Context, userID int64) (decimal.Decimal, error) {
	var balance decimal.Decimal
	err := l.pool.QueryRow(ctx,
		`SELECT balance FROM wallets WHERE user_id = $1`, userID,
	).Scan(&balance)
	return balance, err
}

// AvailableBalance returns the spendable balance after P2P escrow locks.
func (l *Ledger) AvailableBalance(ctx context.Context, userID int64) (decimal.Decimal, error) {
	var balance, locked decimal.Decimal
	err := l.pool.QueryRow(ctx,
		`SELECT balance, locked_balance FROM wallets WHERE user_id = $1`, userID,
	).Scan(&balance, &locked)
	return balance.Sub(locked), err
}

// Accounts lists every account with its balance, treasury included.
func (l *Ledger) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT u.id, w.balance, u.created_at
		FROM users u
		JOIN wallets w ON w.user_id = u.id
		ORDER BY u.id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Account{}
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.UserID, &a.Balance, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// History returns a user's most recent ledger entries, newest first.
func (l *Ledger) History(ctx context.Context, userID int64, limit int) ([]Entry, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT id, amount, entry_type, COALESCE(reference_id, ''), created_at
		FROM ledger_entries
		WHERE user_id = $1
		ORDER BY id DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Amount, &e.EntryType, &e.ReferenceID, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Withdrawals returns a user's most recent withdrawal requests, newest first,
// so the UI can show whether the relayer has broadcast them yet.
func (l *Ledger) Withdrawals(ctx context.Context, userID int64, limit int) ([]WithdrawalRecord, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT id, amount, fee, to_address, status, COALESCE(tx_hash, ''), created_at
		FROM withdrawals
		WHERE user_id = $1
		ORDER BY id DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []WithdrawalRecord{}
	for rows.Next() {
		var wr WithdrawalRecord
		if err := rows.Scan(&wr.ID, &wr.Amount, &wr.Fee, &wr.ToAddress, &wr.Status, &wr.TxHash, &wr.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, wr)
	}
	return out, rows.Err()
}
