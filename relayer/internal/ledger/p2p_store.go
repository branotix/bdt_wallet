package ledger

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var (
	ErrAdNotFound              = errors.New("ad not found or not active")
	ErrOrderNotFound           = errors.New("order not found")
	ErrAmountOutOfRange        = errors.New("amount is outside the ad's min/max order size")
	ErrAmountExceedsAd         = errors.New("amount exceeds what's remaining on this ad")
	ErrCannotTradeOwnAd        = errors.New("cannot open an order against your own ad")
	ErrWrongOrderStatus        = errors.New("order is not in a state that allows this action")
	ErrNotOrderParty           = errors.New("you are not a party to this order")
	ErrAdHasOpenOrders         = errors.New("cannot cancel an ad with open orders against it")
	ErrPaymentMethodNotAllowed = errors.New("payment method is not offered by this ad")
)

const paymentWindow = 15 * time.Minute

// P2PAd mirrors the p2p_ads table for API responses.
type P2PAd struct {
	ID              int64           `json:"id"`
	MakerID         int64           `json:"maker_id"`
	Side            string          `json:"side"`
	Price           decimal.Decimal `json:"price"`
	MinOrder        decimal.Decimal `json:"min_order"`
	MaxOrder        decimal.Decimal `json:"max_order"`
	TotalAmount     decimal.Decimal `json:"total_amount"`
	RemainingAmount decimal.Decimal `json:"remaining_amount"`
	PaymentMethods  string          `json:"payment_methods"`
	Status          string          `json:"status"`
	CreatedAt       time.Time       `json:"created_at"`
}

// P2POrder mirrors the p2p_orders table for API responses.
type P2POrder struct {
	ID              int64           `json:"id"`
	AdID            int64           `json:"ad_id"`
	TokenProviderID int64           `json:"token_provider_id"`
	FiatPayerID     int64           `json:"fiat_payer_id"`
	Amount          decimal.Decimal `json:"amount"`
	Price           decimal.Decimal `json:"price"`
	FiatAmount      decimal.Decimal `json:"fiat_amount"`
	PaymentMethod   string          `json:"payment_method"`
	Status          string          `json:"status"`
	PaymentDeadline time.Time       `json:"payment_deadline"`
	CreatedAt       time.Time       `json:"created_at"`
}

// CreateAd posts a new standing offer. side='sell' means the maker is
// offering TOKEN for fiat; side='buy' means the maker wants to buy token,
// paying fiat externally. Posting an ad does NOT lock anything — locking
// happens per-order, when a counterparty actually commits to a trade (see
// CreateOrder). This matches how Binance P2P ads work: the ad is just a
// listing of intent and available size.
func (l *Ledger) CreateAd(ctx context.Context, makerID int64, side string, price, minOrder, maxOrder, totalAmount decimal.Decimal, paymentMethods string) (int64, error) {
	if side != "sell" && side != "buy" {
		return 0, errors.New("side must be 'sell' or 'buy'")
	}
	var adID int64
	err := l.pool.QueryRow(ctx,
		`INSERT INTO p2p_ads (maker_id, side, price, min_order, max_order, total_amount, remaining_amount, payment_methods)
		 VALUES ($1, $2, $3, $4, $5, $6, $6, $7) RETURNING id`,
		makerID, side, price, minOrder, maxOrder, totalAmount, paymentMethods,
	).Scan(&adID)
	return adID, err
}

func (l *Ledger) ListActiveAds(ctx context.Context, side string) ([]P2PAd, error) {
	query := `SELECT id, maker_id, side, price, min_order, max_order, total_amount, remaining_amount, payment_methods, status, created_at
	          FROM p2p_ads WHERE status = 'active' AND remaining_amount > 0`
	args := []any{}
	if side == "sell" || side == "buy" {
		query += ` AND side = $1`
		args = append(args, side)
	}
	query += ` ORDER BY price ASC, created_at ASC`

	rows, err := l.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ads []P2PAd
	for rows.Next() {
		var a P2PAd
		if err := rows.Scan(&a.ID, &a.MakerID, &a.Side, &a.Price, &a.MinOrder, &a.MaxOrder, &a.TotalAmount, &a.RemainingAmount, &a.PaymentMethods, &a.Status, &a.CreatedAt); err != nil {
			return nil, err
		}
		ads = append(ads, a)
	}
	if ads == nil {
		ads = []P2PAd{}
	}
	return ads, nil
}

func (l *Ledger) ListUserAds(ctx context.Context, userID int64) ([]P2PAd, error) {
	rows, err := l.pool.Query(ctx,
		`SELECT id, maker_id, side, price, min_order, max_order, total_amount, remaining_amount, payment_methods, status, created_at
		 FROM p2p_ads WHERE maker_id = $1 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ads []P2PAd
	for rows.Next() {
		var a P2PAd
		if err := rows.Scan(&a.ID, &a.MakerID, &a.Side, &a.Price, &a.MinOrder, &a.MaxOrder, &a.TotalAmount, &a.RemainingAmount, &a.PaymentMethods, &a.Status, &a.CreatedAt); err != nil {
			return nil, err
		}
		ads = append(ads, a)
	}
	if ads == nil {
		ads = []P2PAd{}
	}
	return ads, nil
}

// CancelAd stops an ad from accepting new orders. Refuses if there are
// still orders against it that haven't reached a final state — cancelling
// out from under an in-progress trade would orphan its escrow bookkeeping.
func (l *Ledger) CancelAd(ctx context.Context, adID, requestedBy int64) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var makerID int64
	err = tx.QueryRow(ctx, `SELECT maker_id FROM p2p_ads WHERE id = $1 FOR UPDATE`, adID).Scan(&makerID)
	if err != nil {
		return err
	}
	if makerID != requestedBy {
		return ErrNotOrderParty
	}

	var openOrders int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM p2p_orders WHERE ad_id = $1 AND status IN ('pending_payment', 'paid', 'disputed')`,
		adID,
	).Scan(&openOrders); err != nil {
		return err
	}
	if openOrders > 0 {
		return ErrAdHasOpenOrders
	}

	if _, err := tx.Exec(ctx, `UPDATE p2p_ads SET status = 'cancelled', updated_at = now() WHERE id = $1`, adID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateOrder opens a trade against an ad and LOCKS the token amount from
// whichever party is providing token, immediately and atomically with
// creating the order row — there is no window where the order exists but
// the escrow doesn't, or vice versa.
func (l *Ledger) CreateOrder(ctx context.Context, adID, takerID int64, amount decimal.Decimal, paymentMethod string) (int64, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var makerID int64
	var side string
	var price, minOrder, maxOrder, remaining decimal.Decimal
	var status string
	err = tx.QueryRow(ctx,
		`SELECT maker_id, side, price, min_order, max_order, remaining_amount, status FROM p2p_ads WHERE id = $1 FOR UPDATE`,
		adID,
	).Scan(&makerID, &side, &price, &minOrder, &maxOrder, &remaining, &status)
	if err != nil {
		return 0, ErrAdNotFound
	}
	if status != "active" {
		return 0, ErrAdNotFound
	}
	if makerID == takerID {
		return 0, ErrCannotTradeOwnAd
	}
	if amount.LessThan(minOrder) || amount.GreaterThan(maxOrder) {
		return 0, ErrAmountOutOfRange
	}
	if amount.GreaterThan(remaining) {
		return 0, ErrAmountExceedsAd
	}

	// The taker may only choose a payment method explicitly offered by the ad.
	// Treat the stored comma-separated list as data, not as a free-form client
	// choice.
	methodOK := false
	var methods string
	if err := tx.QueryRow(ctx, `SELECT payment_methods FROM p2p_ads WHERE id = $1`, adID).Scan(&methods); err == nil {
		for _, m := range strings.Split(methods, ",") {
			if strings.EqualFold(strings.TrimSpace(m), strings.TrimSpace(paymentMethod)) {
				methodOK = true
				break
			}
		}
	}
	if !methodOK {
		return 0, ErrPaymentMethodNotAllowed
	}

	// side='sell': maker provides token -> token_provider = maker.
	// side='buy':  maker provides fiat, taker provides token -> token_provider = taker.
	var tokenProviderID, fiatPayerID int64
	if side == "sell" {
		tokenProviderID, fiatPayerID = makerID, takerID
	} else {
		tokenProviderID, fiatPayerID = takerID, makerID
	}

	// Lock the token provider's wallet row (FOR UPDATE) and check AVAILABLE
	// balance (balance - locked_balance) — the same "available, not just
	// total" principle as Transfer/ReserveWithdrawal in ledger.go.
	var balance, locked decimal.Decimal
	if err := tx.QueryRow(ctx,
		`SELECT balance, locked_balance FROM wallets WHERE user_id = $1 FOR UPDATE`, tokenProviderID,
	).Scan(&balance, &locked); err != nil {
		return 0, err
	}
	available := balance.Sub(locked)
	if available.LessThan(amount) {
		return 0, ErrInsufficientBalance
	}

	if _, err := tx.Exec(ctx,
		`UPDATE wallets SET locked_balance = locked_balance + $1 WHERE user_id = $2`,
		amount, tokenProviderID,
	); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE p2p_ads SET remaining_amount = remaining_amount - $1,
			status = CASE WHEN remaining_amount - $1 <= 0 THEN 'completed' ELSE status END,
			updated_at = now() WHERE id = $2`,
		amount, adID,
	); err != nil {
		return 0, err
	}

	fiatAmount := amount.Mul(price)
	var orderID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO p2p_orders (ad_id, token_provider_id, fiat_payer_id, amount, price, fiat_amount, payment_method, payment_deadline)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		adID, tokenProviderID, fiatPayerID, amount, price, fiatAmount, paymentMethod, time.Now().Add(paymentWindow),
	).Scan(&orderID)
	if err != nil {
		return 0, err
	}

	return orderID, tx.Commit(ctx)
}

// MarkPaid is called by the fiat payer once they've sent the real-money
// payment externally (bKash/Nagad/bank — nothing this app can see or
// verify directly). This is a claim, not proof; the token provider still
// has to actually check their account and call ConfirmPayment themselves.
// Marking paid also takes the order OUT of the auto-expiry path (see
// ExpireStalePendingOrders) — once a payment claim exists, only a human
// (provider confirming, or a dispute) should resolve it, never a timeout.
func (l *Ledger) MarkPaid(ctx context.Context, orderID, requestedBy int64) error {
	tag, err := l.pool.Exec(ctx,
		`UPDATE p2p_orders SET status = 'paid', paid_at = now(), updated_at = now()
		 WHERE id = $1 AND fiat_payer_id = $2 AND status = 'pending_payment'`,
		orderID, requestedBy,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrWrongOrderStatus
	}
	return nil
}

// ConfirmPayment is called by the token provider after they've personally
// verified the fiat arrived. This is the ONLY thing that actually releases
// escrowed token — nothing in this codebase auto-releases based on the
// fiat_payer's claim alone. An optional platform fee (feePercent) is
// deducted from the released amount and credited to the treasury, same
// pattern as internal transfers and withdrawals.
func (l *Ledger) ConfirmPayment(ctx context.Context, orderID, requestedBy int64, feePercent decimal.Decimal, treasuryUserID int64) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var tokenProviderID, fiatPayerID int64
	var amount decimal.Decimal
	var status string
	err = tx.QueryRow(ctx,
		`SELECT token_provider_id, fiat_payer_id, amount, status FROM p2p_orders WHERE id = $1 FOR UPDATE`,
		orderID,
	).Scan(&tokenProviderID, &fiatPayerID, &amount, &status)
	if err != nil {
		return ErrOrderNotFound
	}
	if tokenProviderID != requestedBy {
		return ErrNotOrderParty
	}
	if status != "paid" && status != "pending_payment" {
		// Allow confirming straight from pending_payment too — some
		// providers check their bKash and confirm before the payer even
		// clicks "I've paid". Either way this is a deliberate, in-person
		// verification the provider is vouching for.
		return ErrWrongOrderStatus
	}

	fee := amount.Mul(feePercent).Round(2)
	releaseToPayer := amount.Sub(fee)

	if _, err := tx.Exec(ctx,
		`UPDATE wallets SET balance = balance - $1, locked_balance = locked_balance - $1, updated_at = now() WHERE user_id = $2`,
		amount, tokenProviderID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE wallets SET balance = balance + $1, updated_at = now() WHERE user_id = $2`,
		releaseToPayer, fiatPayerID,
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

	group := uuid.New()
	type entry struct {
		userID int64
		delta  decimal.Decimal
	}
	entries := []entry{
		{tokenProviderID, amount.Neg()},
		{fiatPayerID, releaseToPayer},
	}
	if fee.IsPositive() {
		entries = append(entries, entry{treasuryUserID, fee})
	}
	for _, e := range entries {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (transaction_group, user_id, amount, entry_type, reference_id)
			 VALUES ($1, $2, $3, 'p2p_trade', $4)`,
			group, e.userID, e.delta, "p2p_order:"+itoa64(orderID),
		); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE p2p_orders SET status = 'completed', completed_at = now(), updated_at = now() WHERE id = $1`,
		orderID,
	); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// CancelOrder unwinds an order BEFORE the fiat payer has claimed to pay —
// releases the escrow back to the token provider's available balance and
// restores the ad's remaining_amount, with no token movement between
// parties (nothing was ever actually exchanged). Either party may cancel
// while still pending_payment; once marked paid, cancellation requires a
// dispute instead (see DisputeOrder) — a unilateral cancel after a payment
// claim would let the token provider walk away with an escrow release owed
// to nobody while the payer may have genuinely already sent real money.
func (l *Ledger) CancelOrder(ctx context.Context, orderID, requestedBy int64) error {
	return l.unwindOrder(ctx, orderID, requestedBy, true)
}

// expireOrder is the same unwind as CancelOrder but invoked by the
// background expiry sweep rather than a user action — see
// ExpireStalePendingOrders.
func (l *Ledger) expireOrder(ctx context.Context, orderID int64) error {
	return l.unwindOrder(ctx, orderID, 0, false)
}

func (l *Ledger) unwindOrder(ctx context.Context, orderID, requestedBy int64, checkParty bool) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var adID, tokenProviderID, fiatPayerID int64
	var amount decimal.Decimal
	var status string
	err = tx.QueryRow(ctx,
		`SELECT ad_id, token_provider_id, fiat_payer_id, amount, status FROM p2p_orders WHERE id = $1 FOR UPDATE`,
		orderID,
	).Scan(&adID, &tokenProviderID, &fiatPayerID, &amount, &status)
	if err != nil {
		return ErrOrderNotFound
	}
	if status != "pending_payment" {
		return ErrWrongOrderStatus
	}
	if checkParty && requestedBy != tokenProviderID && requestedBy != fiatPayerID {
		return ErrNotOrderParty
	}

	if _, err := tx.Exec(ctx,
		`UPDATE wallets SET locked_balance = locked_balance - $1 WHERE user_id = $2`,
		amount, tokenProviderID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE p2p_ads SET remaining_amount = remaining_amount + $1, updated_at = now() WHERE id = $2`,
		amount, adID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE p2p_orders SET status = 'cancelled', cancelled_at = now(), updated_at = now() WHERE id = $1`,
		orderID,
	); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ExpireStalePendingOrders auto-cancels orders still awaiting a payment
// claim past their deadline — meant to be called periodically by a
// background worker (see cmd/main.go). Orders already marked 'paid' are
// NEVER touched here, on purpose (see MarkPaid's comment).
func (l *Ledger) ExpireStalePendingOrders(ctx context.Context) (int, error) {
	rows, err := l.pool.Query(ctx,
		`SELECT id FROM p2p_orders WHERE status = 'pending_payment' AND payment_deadline < now()`,
	)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	count := 0
	for _, id := range ids {
		if err := l.expireOrder(ctx, id); err != nil {
			continue // best-effort; a failed one gets retried next sweep
		}
		count++
	}
	return count, nil
}

// DisputeOrder flags an order for manual review instead of letting either
// party unilaterally resolve it — appropriate once a payment claim exists
// (status='paid') and the two parties disagree about what actually
// happened. Resolution is deliberately NOT automated here; see
// AdminResolveDispute for the two possible outcomes an operator can apply
// after actually looking into it.
func (l *Ledger) DisputeOrder(ctx context.Context, orderID, requestedBy int64, reason string) error {
	tag, err := l.pool.Exec(ctx,
		`UPDATE p2p_orders SET status = 'disputed', dispute_reason = $1, dispute_opened_at = now(),
		    dispute_opened_by = $2, updated_at = now()
		 WHERE id = $3 AND status IN ('pending_payment', 'paid')
		   AND (token_provider_id = $2 OR fiat_payer_id = $2)`,
		reason, requestedBy, orderID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrWrongOrderStatus
	}
	return nil
}

// AdminResolveDispute is for the operator (gated behind the admin console's
// AuthToken, not exposed to regular users) to manually settle a disputed
// order after actually investigating it. favorPayer=true releases escrow to
// the fiat payer (use when payment genuinely was received); false unwinds
// it back to the token provider (use when it wasn't).
func (l *Ledger) AdminResolveDispute(ctx context.Context, orderID int64, favorPayer bool, feePercent decimal.Decimal, treasuryUserID int64) error {
	var tokenProviderID int64
	if err := l.pool.QueryRow(ctx, `SELECT token_provider_id FROM p2p_orders WHERE id = $1 AND status = 'disputed'`, orderID).Scan(&tokenProviderID); err != nil {
		return ErrOrderNotFound
	}
	if favorPayer {
		return l.ConfirmPayment(ctx, orderID, tokenProviderID, feePercent, treasuryUserID)
	}
	return l.unwindOrder(ctx, orderID, 0, false)
}

func (l *Ledger) GetOrder(ctx context.Context, orderID int64) (*P2POrder, error) {
	var o P2POrder
	err := l.pool.QueryRow(ctx,
		`SELECT id, ad_id, token_provider_id, fiat_payer_id, amount, price, fiat_amount, payment_method, status, payment_deadline, created_at
		 FROM p2p_orders WHERE id = $1`,
		orderID,
	).Scan(&o.ID, &o.AdID, &o.TokenProviderID, &o.FiatPayerID, &o.Amount, &o.Price, &o.FiatAmount, &o.PaymentMethod, &o.Status, &o.PaymentDeadline, &o.CreatedAt)
	if err != nil {
		return nil, ErrOrderNotFound
	}
	return &o, nil
}

func (l *Ledger) ListUserOrders(ctx context.Context, userID int64) ([]P2POrder, error) {
	rows, err := l.pool.Query(ctx,
		`SELECT id, ad_id, token_provider_id, fiat_payer_id, amount, price, fiat_amount, payment_method, status, payment_deadline, created_at
		 FROM p2p_orders WHERE token_provider_id = $1 OR fiat_payer_id = $1 ORDER BY created_at DESC LIMIT 50`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orders []P2POrder
	for rows.Next() {
		var o P2POrder
		if err := rows.Scan(&o.ID, &o.AdID, &o.TokenProviderID, &o.FiatPayerID, &o.Amount, &o.Price, &o.FiatAmount, &o.PaymentMethod, &o.Status, &o.PaymentDeadline, &o.CreatedAt); err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	if orders == nil {
		orders = []P2POrder{}
	}
	return orders, nil
}

func (l *Ledger) ListDisputedOrders(ctx context.Context) ([]P2POrder, error) {
	rows, err := l.pool.Query(ctx,
		`SELECT id, ad_id, token_provider_id, fiat_payer_id, amount, price, fiat_amount, payment_method, status, payment_deadline, created_at
		 FROM p2p_orders WHERE status = 'disputed' ORDER BY dispute_opened_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orders []P2POrder
	for rows.Next() {
		var o P2POrder
		if err := rows.Scan(&o.ID, &o.AdID, &o.TokenProviderID, &o.FiatPayerID, &o.Amount, &o.Price, &o.FiatAmount, &o.PaymentMethod, &o.Status, &o.PaymentDeadline, &o.CreatedAt); err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	if orders == nil {
		orders = []P2POrder{}
	}
	return orders, nil
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
