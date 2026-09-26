// Package fees holds the platform's flat fee schedule in one place, so the
// API, the withdraw worker and any future admin tooling can never disagree
// about what a transfer costs.
package fees

import "github.com/shopspring/decimal"

// TreasuryUserID is the account every collected fee is credited to. It is
// seeded as user_id = 1 by ledger/schema.sql — never let a real signup take it.
const TreasuryUserID int64 = 1

var (
	// InternalTransfer is charged on user-to-user transfers inside the
	// Postgres ledger. Flat, not percentage-based: 3 BDT regardless of amount.
	InternalTransfer = decimal.NewFromInt(3)

	// ExternalWithdraw is charged when BDT leaves the platform to an outside
	// wallet. Flat 10 BDT, debited on top of the withdrawn amount, and it is
	// what pays for the hot wallet's BNB gas.
	ExternalWithdraw = decimal.NewFromInt(10)

	// Deposit is free — the user already paid their own gas to send the
	// on-chain transfer in, so we credit the full amount.
	Deposit = decimal.Zero

	// P2PTradeFeePercent is deducted from the token amount released to the
	// fiat payer when a P2P trade completes — e.g. 0.005 = 0.5%. Zero
	// disables the fee entirely (payer receives the full escrowed amount).
	P2PTradeFeePercent = decimal.NewFromFloat(0.005)
)
