package chain

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/shopspring/decimal"

	"bdt-relayer/internal/ledger"
)

// BroadcastedWithdrawal is what the confirmation poller needs to check on
// a "broadcasting" status withdrawal.
type BroadcastedWithdrawal struct {
	ID     int64
	UserID int64
	TxHash string
	Amount decimal.Decimal
	Fee    decimal.Decimal
	// Nonce of the signed transaction. Pointer because rows written before this
	// column existed have NULL, and without it we cannot prove a missing
	// transaction is dead, so we must not refund it.
	Nonce *int64
	// RawTx is the hex of the signed transaction, so it can be re-sent verbatim
	// if the first broadcast was lost. Empty for rows written before the column
	// existed.
	RawTx     string
	CreatedAt time.Time
}

type ConfirmationPoller struct {
	client *ethclient.Client
	ledger *ledger.Ledger
	// hotAddr is the wallet the withdrawal was signed from. Its confirmed nonce
	// is the only reliable evidence that an unmined transaction is truly dead.
	hotAddr        common.Address
	treasuryUserID int64
	confirmations  uint64
	// maxWait is how long an unmined-but-still-mineable transaction may sit
	// before we start warning a human about it. It is NOT a refund trigger —
	// see checkNotMined for why a timeout can never justify a refund.
	maxWait time.Duration
	// lastWarned throttles the "stuck" warning to one line per withdrawal per
	// warnInterval. Only touched from the single Run goroutine, so no lock.
	lastWarned map[int64]time.Time
}

const warnInterval = 5 * time.Minute

func NewConfirmationPoller(client *ethclient.Client, l *ledger.Ledger, hotAddr common.Address, treasuryUserID int64, confirmations uint64, maxWait time.Duration) *ConfirmationPoller {
	return &ConfirmationPoller{
		client:         client,
		ledger:         l,
		hotAddr:        hotAddr,
		treasuryUserID: treasuryUserID,
		confirmations:  confirmations,
		maxWait:        maxWait,
		lastWarned:     map[int64]time.Time{},
	}
}

func (c *ConfirmationPoller) Run(ctx context.Context, pollInterval time.Duration, fetchBroadcasting func(ctx context.Context) ([]BroadcastedWithdrawal, error)) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			list, err := fetchBroadcasting(ctx)
			if err != nil {
				continue
			}
			latest, err := c.client.BlockNumber(ctx)
			if err != nil {
				continue
			}
			for _, w := range list {
				c.checkOne(ctx, w, latest)
			}
		}
	}
}

func (c *ConfirmationPoller) checkOne(ctx context.Context, w BroadcastedWithdrawal, latestBlock uint64) {
	receipt, err := c.client.TransactionReceipt(ctx, common.HexToHash(w.TxHash))
	if errors.Is(err, ethereum.NotFound) {
		c.checkNotMined(ctx, w)
		return
	}
	if err != nil {
		log.Printf("confirmation check error for withdrawal %d: %v", w.ID, err)
		return
	}

	delete(c.lastWarned, w.ID)

	if receipt.Status == 0 {
		// Mined but REVERTED on-chain. The tokens definitively did not move and
		// the nonce is consumed, so this is the one case where a refund is
		// unambiguously safe and immediate.
		log.Printf("withdrawal %d tx %s reverted on-chain — refunding", w.ID, w.TxHash)
		c.refund(ctx, w)
		return
	}

	confirmations := latestBlock - receipt.BlockNumber.Uint64()
	if confirmations >= c.confirmations {
		if err := c.ledger.MarkWithdrawalStatus(ctx, w.ID, "confirmed", w.TxHash); err != nil {
			log.Printf("mark confirmed failed for withdrawal %d: %v", w.ID, err)
		}
	}
	// else: mined but not enough confirmations yet, check again next tick
}

// checkNotMined decides what to do about a broadcast transaction that has no
// receipt.
//
// The old rule here was "no receipt after 30 minutes => refund". That loses
// money: a transaction with a low gas price can sit in a mempool for hours and
// then confirm. Refund it and the user has both the credited balance and the
// tokens, paid out of the hot wallet float.
//
// A timeout proves nothing. The hot wallet's CONFIRMED nonce does:
//
//	confirmedNonce > ourNonce  => some other transaction already used our slot,
//	                              and it wasn't ours (we have no receipt), so
//	                              ours can never be mined. Refund is safe.
//	confirmedNonce <= ourNonce => the slot is still open. Our transaction can
//	                              confirm at any moment. Refunding is a double
//	                              payout, so we re-broadcast instead and wait.
func (c *ConfirmationPoller) checkNotMined(ctx context.Context, w BroadcastedWithdrawal) {
	if w.Nonce == nil {
		// Written before the nonce column existed, so the test above is
		// impossible. Refusing to refund is the only safe option; park it for a
		// human once it is clearly not coming back.
		if time.Since(w.CreatedAt) > c.maxWait {
			log.Printf("withdrawal %d tx %s unmined after %s and has no recorded nonce — marking stuck, needs manual review",
				w.ID, w.TxHash, c.maxWait)
			if err := c.ledger.MarkWithdrawalStuck(ctx, w.ID); err != nil {
				log.Printf("mark stuck failed for withdrawal %d: %v", w.ID, err)
			}
		}
		return
	}

	confirmedNonce, err := c.client.NonceAt(ctx, c.hotAddr, nil)
	if err != nil {
		log.Printf("withdrawal %d: cannot read hot wallet nonce, will retry: %v", w.ID, err)
		return
	}

	if confirmedNonce > uint64(*w.Nonce) {
		// Re-check the receipt before paying out. Between the lookup above and
		// this point our transaction could have been mined, which would ALSO
		// move the nonce forward — refunding then would be a double payout.
		if _, err := c.client.TransactionReceipt(ctx, common.HexToHash(w.TxHash)); err == nil {
			log.Printf("withdrawal %d tx %s appeared while checking nonce — not refunding", w.ID, w.TxHash)
			return
		} else if !errors.Is(err, ethereum.NotFound) {
			log.Printf("withdrawal %d: receipt re-check failed, not refunding: %v", w.ID, err)
			return
		}

		log.Printf("withdrawal %d tx %s can never be mined (hot wallet nonce %d is past tx nonce %d) — refunding",
			w.ID, w.TxHash, confirmedNonce, *w.Nonce)
		c.refund(ctx, w)
		delete(c.lastWarned, w.ID)
		return
	}

	// Slot still open, so the transaction is still live. Push it back out to the
	// network: public RPC nodes drop pending transactions, and re-sending the
	// identical signed bytes is free of risk because the nonce is the same, so
	// the chain can include it at most once.
	if w.RawTx != "" {
		if err := c.rebroadcast(ctx, w); err != nil {
			log.Printf("withdrawal %d: re-broadcast failed: %v", w.ID, err)
		}
	}

	if time.Since(w.CreatedAt) > c.maxWait && time.Since(c.lastWarned[w.ID]) > warnInterval {
		c.lastWarned[w.ID] = time.Now()
		log.Printf("WARNING: withdrawal %d tx %s unmined for %s. Nonce %d is still open so it CAN still confirm — "+
			"deliberately NOT refunding. If it must be cancelled, send a replacement transaction from the hot wallet "+
			"at nonce %d with a higher gas price; once that confirms this row is refunded automatically.",
			w.ID, w.TxHash, time.Since(w.CreatedAt).Truncate(time.Second), *w.Nonce, *w.Nonce)
	}
}

// rebroadcast re-sends the stored signed transaction. Errors meaning "the node
// already has this" are the expected case and stay silent.
func (c *ConfirmationPoller) rebroadcast(ctx context.Context, w BroadcastedWithdrawal) error {
	raw, err := hexutil.Decode(w.RawTx)
	if err != nil {
		return fmt.Errorf("decode raw_tx: %w", err)
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return fmt.Errorf("unmarshal raw_tx: %w", err)
	}
	if err := c.client.SendTransaction(ctx, &tx); err != nil {
		if isAlreadyKnown(err) {
			return nil // still in the mempool, exactly what we hoped
		}
		return err
	}
	log.Printf("withdrawal %d: re-broadcast tx %s (it had been dropped from the mempool)", w.ID, w.TxHash)
	return nil
}

func isAlreadyKnown(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"already known", "known transaction", "already exists", "nonce too low", "replacement transaction underpriced"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func (c *ConfirmationPoller) refund(ctx context.Context, w BroadcastedWithdrawal) {
	if err := c.ledger.RefundFailedWithdrawal(ctx, w.ID, w.UserID, w.Amount, w.Fee, c.treasuryUserID); err != nil {
		log.Printf("refund failed for withdrawal %d: %v", w.ID, err)
	}
}
