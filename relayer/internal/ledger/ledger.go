package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

var ErrInsufficientBalance = errors.New("insufficient balance")
var ErrAlreadyProcessed = errors.New("chain event already processed")

type Ledger struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Ledger {
	return &Ledger{pool: pool}
}

// Transfer moves `amount` from senderID to receiverID atomically, with
// deadlock-safe lock ordering (always lock the lower user_id first).
// All monetary values use decimal.Decimal — NEVER float64 — to avoid
// rounding/precision drift on repeated arithmetic.
func (l *Ledger) Transfer(ctx context.Context, senderID, receiverID int64, amount, fee decimal.Decimal, treasuryUserID int64) error {
	if senderID == receiverID {
		return fmt.Errorf("sender and receiver cannot be the same user")
	}

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op if committed

	// Lock ordering: always ascending by user_id, regardless of who is
	// sender/receiver. This is what prevents deadlocks under concurrent
	// opposite-direction transfers (A->B and B->A at the same time).
	lo, hi := senderID, receiverID
	if lo > hi {
		lo, hi = hi, lo
	}
	rows, err := tx.Query(ctx,
		`SELECT user_id, balance, locked_balance FROM wallets WHERE user_id IN ($1, $2) ORDER BY user_id FOR UPDATE`,
		lo, hi,
	)
	if err != nil {
		return err
	}
	balances := map[int64]decimal.Decimal{}
	for rows.Next() {
		var uid int64
		var bal, locked decimal.Decimal
		if err := rows.Scan(&uid, &bal, &locked); err != nil {
			rows.Close()
			return err
		}
		// Available balance excludes anything locked in a P2P escrow (see
		// migrate-006-p2p.sql) — this is what actually makes escrow mean
		// something. Without this, a locked amount could still be
		// transferred out from under an open P2P order.
		balances[uid] = bal.Sub(locked)
	}
	rows.Close()

	totalDebit := amount.Add(fee)
	senderBal, ok := balances[senderID]
	if !ok || senderBal.LessThan(totalDebit) {
		return ErrInsufficientBalance
	}

	group := uuid.New()

	type entry struct {
		userID int64
		delta  decimal.Decimal
		typ    string
	}
	entries := []entry{
		{senderID, totalDebit.Neg(), "transfer"},
		{receiverID, amount, "transfer"},
	}
	if fee.IsPositive() {
		entries = append(entries, entry{treasuryUserID, fee, "fee"})
	}

	for _, e := range entries {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (transaction_group, user_id, amount, entry_type, reference_id)
			 VALUES ($1, $2, $3, $4, $5)`,
			group, e.userID, e.delta, e.typ, group.String(),
		); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE wallets SET balance = balance - $1, updated_at = now() WHERE user_id = $2`,
		totalDebit, senderID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE wallets SET balance = balance + $1, updated_at = now() WHERE user_id = $2`,
		amount, receiverID,
	); err != nil {
		return err
	}
	if fee.IsPositive() {
		if _, err := tx.Exec(ctx,
			`UPDATE wallets SET balance = balance + $1, updated_at = now() WHERE user_id = $2`,
			fee, treasuryUserID,
		); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// CreditDeposit credits a user's wallet after an on-chain deposit is confirmed.
// Idempotent: if txHash+logIndex was already processed, it's a no-op — safe to
// call repeatedly if your watcher restarts or re-scans blocks.
func (l *Ledger) CreditDeposit(ctx context.Context, userID int64, amount decimal.Decimal, txHash string, logIndex int) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx,
		`INSERT INTO processed_chain_events (tx_hash, log_index) VALUES ($1, $2)`,
		txHash, logIndex,
	)
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			return ErrAlreadyProcessed
		}
		return err
	}

	group := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries (transaction_group, user_id, amount, entry_type, reference_id)
		 VALUES ($1, $2, $3, 'deposit', $4)`,
		group, userID, amount, txHash,
	); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE wallets SET balance = balance + $1, updated_at = now() WHERE user_id = $2`,
		amount, userID,
	); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ReserveWithdrawal debits the user's balance and creates a pending withdrawal
// row in the SAME transaction, so a balance can never be debited without a
// matching withdrawal record (and vice versa). The fee is credited to the
// treasury here, exactly like Transfer does — otherwise withdrawal fees are
// recorded in withdrawals.fee but never appear in anyone's balance, and the
// money is silently unaccounted for.
func (l *Ledger) ReserveWithdrawal(ctx context.Context, userID int64, amount, fee decimal.Decimal, toAddress, idempotencyKey string, treasuryUserID int64) (int64, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// Idempotency prevents a retry/double-tap from debiting the same request
	// twice. The unique DB constraint is the final authority.
	if idempotencyKey != "" {
		var existing int64
		err := tx.QueryRow(ctx, `SELECT id FROM withdrawals WHERE user_id = $1 AND idempotency_key = $2`, userID, idempotencyKey).Scan(&existing)
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
	}

	// Lock both rows in ascending user_id order, the same order Transfer uses.
	// Locking "user then treasury" would deadlock against a concurrent
	// treasury-to-user transfer. treasuryUserID may equal userID (the operator
	// withdrawing collected fees), in which case this locks one row.
	var balance decimal.Decimal
	rows, err := tx.Query(ctx,
		`SELECT user_id, balance, locked_balance FROM wallets WHERE user_id IN ($1, $2) ORDER BY user_id FOR UPDATE`,
		userID, treasuryUserID,
	)
	if err != nil {
		return 0, err
	}
	found := false
	for rows.Next() {
		var uid int64
		var bal, locked decimal.Decimal
		if err := rows.Scan(&uid, &bal, &locked); err != nil {
			rows.Close()
			return 0, err
		}
		if uid == userID {
			// Same principle as Transfer: money locked in an open P2P order
			// is not available for withdrawal either.
			balance, found = bal.Sub(locked), true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if !found {
		return 0, pgx.ErrNoRows
	}

	total := amount.Add(fee)
	if balance.LessThan(total) {
		return 0, ErrInsufficientBalance
	}

	var withdrawalID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO withdrawals (user_id, amount, fee, to_address, idempotency_key, status)
			 VALUES ($1, $2, $3, $4, NULLIF($5, ''), 'pending') RETURNING id`,
		userID, amount, fee, toAddress, idempotencyKey,
	).Scan(&withdrawalID)
	if err != nil {
		if idempotencyKey != "" && isUniqueViolation(err) {
			var existing int64
			if qerr := tx.QueryRow(ctx, `SELECT id FROM withdrawals WHERE user_id = $1 AND idempotency_key = $2`, userID, idempotencyKey).Scan(&existing); qerr == nil {
				return existing, nil
			}
		}
		return 0, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE wallets SET balance = balance - $1, updated_at = now() WHERE user_id = $2`,
		total, userID,
	); err != nil {
		return 0, err
	}

	group := uuid.New()
	reference := fmt.Sprintf("withdrawal:%d", withdrawalID)
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries (transaction_group, user_id, amount, entry_type, reference_id)
		 VALUES ($1, $2, $3, 'withdraw', $4)`,
		group, userID, total.Neg(), reference,
	); err != nil {
		return 0, err
	}

	if fee.IsPositive() {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (transaction_group, user_id, amount, entry_type, reference_id)
			 VALUES ($1, $2, $3, 'fee', $4)`,
			group, treasuryUserID, fee, reference,
		); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE wallets SET balance = balance + $1, updated_at = now() WHERE user_id = $2`,
			fee, treasuryUserID,
		); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return withdrawalID, nil
}

// ClaimForBroadcast moves a withdrawal from 'pending' to 'broadcasting' and
// records the tx hash, nonce and raw signed bytes. It returns false if the row
// was not in 'pending' any more, meaning another worker already claimed it.
//
// This MUST be called before the transaction is broadcast. Broadcasting first
// and recording afterwards leaves a window where the send succeeded but the
// row is still 'pending' — the next poll then signs and sends a SECOND
// transaction for the same withdrawal, paying the user twice.
func (l *Ledger) ClaimForBroadcast(ctx context.Context, id int64, txHash string, nonce uint64, rawTx string) (bool, error) {
	tag, err := l.pool.Exec(ctx,
		`UPDATE withdrawals
		    SET status = 'broadcasting', tx_hash = $1, nonce = $2, raw_tx = $3, updated_at = now()
		  WHERE id = $4 AND status = 'pending'`,
		txHash, int64(nonce), rawTx, id,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MarkWithdrawalStuck parks a withdrawal that was broadcast but never mined,
// and whose nonce is still live so it could confirm at any time. Refunding it
// would risk paying the user twice, so it needs a human decision instead.
func (l *Ledger) MarkWithdrawalStuck(ctx context.Context, id int64) error {
	_, err := l.pool.Exec(ctx,
		`UPDATE withdrawals SET status = 'stuck', updated_at = now() WHERE id = $1 AND status = 'broadcasting'`,
		id,
	)
	return err
}

// MarkWithdrawalStatus updates status after the on-chain send is attempted
// or confirmed by the withdraw worker / confirmation poller.
func (l *Ledger) MarkWithdrawalStatus(ctx context.Context, id int64, status, txHash string) error {
	_, err := l.pool.Exec(ctx,
		`UPDATE withdrawals SET status = $1, tx_hash = COALESCE($2, tx_hash), updated_at = now() WHERE id = $3`,
		status, nullIfEmpty(txHash), id,
	)
	return err
}

// RefundFailedWithdrawal credits the user back if a withdrawal ultimately
// fails on-chain (e.g. reverted, or provably never mineable). It also reverses
// the fee credited to the treasury by ReserveWithdrawal — a refunded
// withdrawal earned no fee.
//
// Only call this once it is certain the transaction can never be mined. See
// ConfirmationPoller.checkOne.
func (l *Ledger) RefundFailedWithdrawal(ctx context.Context, withdrawalID, userID int64, amount, fee decimal.Decimal, treasuryUserID int64) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Guard against a double refund: the poller could see the same row twice if
	// a previous run crashed between the credit and the status update. Only the
	// transition out of a non-final state is allowed to pay out.
	tag, err := tx.Exec(ctx,
		`UPDATE withdrawals SET status = 'failed', updated_at = now()
		 WHERE id = $1 AND status IN ('pending', 'broadcasting', 'stuck')`,
		withdrawalID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return nil // already finalised by someone else — do not credit again
	}

	total := amount.Add(fee)
	group := uuid.New()
	reference := fmt.Sprintf("refund:withdrawal:%d", withdrawalID)
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries (transaction_group, user_id, amount, entry_type, reference_id)
		 VALUES ($1, $2, $3, 'transfer', $4)`,
		group, userID, total, reference,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE wallets SET balance = balance + $1, updated_at = now() WHERE user_id = $2`,
		total, userID,
	); err != nil {
		return err
	}
	if fee.IsPositive() {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (transaction_group, user_id, amount, entry_type, reference_id)
			 VALUES ($1, $2, $3, 'fee', $4)`,
			group, treasuryUserID, fee.Neg(), reference,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE wallets SET balance = balance - $1, updated_at = now() WHERE user_id = $2`,
			fee, treasuryUserID,
		); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

var _ = pgx.ErrNoRows
