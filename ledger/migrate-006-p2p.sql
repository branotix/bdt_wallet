-- P2P trading (Binance-P2P-style): a user posts an ad to trade token for
-- real BDT taka via external payment (bKash/Nagad/bank), a counterparty
-- opens an order against it, the token amount is escrowed IMMEDIATELY, and
-- only released once the token-provider confirms the fiat payment arrived.

BEGIN;

-- Escrow requires wallets to distinguish "total balance" from "available to
-- spend right now". Every existing balance check elsewhere in the app
-- (internal transfer, withdrawal) MUST be updated to check
-- (balance - locked_balance), not balance alone — otherwise a "locked"
-- amount could still be transferred or withdrawn elsewhere, which would
-- make escrow meaningless. See ledger.go's Transfer/ReserveWithdrawal.
ALTER TABLE wallets ADD COLUMN locked_balance NUMERIC(20, 2) NOT NULL DEFAULT 0 CHECK (locked_balance >= 0);
ALTER TABLE wallets ADD CONSTRAINT locked_not_over_balance CHECK (locked_balance <= balance);

-- An ad is a standing offer: "I will sell/buy up to X token at price P per
-- token, in chunks between min and max, via these payment methods."
CREATE TABLE p2p_ads (
    id                BIGSERIAL PRIMARY KEY,
    maker_id          BIGINT NOT NULL REFERENCES users(id),
    -- 'sell': maker provides TOKEN, receives fiat.
    -- 'buy':  maker provides FIAT (externally), receives TOKEN.
    side              TEXT NOT NULL CHECK (side IN ('sell', 'buy')),
    price             NUMERIC(20, 4) NOT NULL CHECK (price > 0), -- BDT taka per token
    min_order         NUMERIC(20, 2) NOT NULL CHECK (min_order > 0),
    max_order         NUMERIC(20, 2) NOT NULL CHECK (max_order >= min_order),
    total_amount      NUMERIC(20, 2) NOT NULL CHECK (total_amount >= min_order),
    remaining_amount  NUMERIC(20, 2) NOT NULL CHECK (remaining_amount >= 0),
    payment_methods   TEXT NOT NULL, -- comma-separated, e.g. "bKash,Nagad,Bank"
    status            TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused', 'cancelled', 'completed')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_p2p_ads_active ON p2p_ads(side, status) WHERE status = 'active';
CREATE INDEX idx_p2p_ads_maker ON p2p_ads(maker_id);

-- An order is one specific trade against an ad. token_provider_id always
-- names whoever's TOKEN gets locked/released — naming it this way (instead
-- of "buyer"/"seller", which flip meaning depending on ad.side) is
-- deliberate, so the escrow code never has to branch on ad.side to figure
-- out whose balance to touch.
CREATE TABLE p2p_orders (
    id                  BIGSERIAL PRIMARY KEY,
    ad_id               BIGINT NOT NULL REFERENCES p2p_ads(id),
    token_provider_id   BIGINT NOT NULL REFERENCES users(id), -- token locked from here, released from here
    fiat_payer_id       BIGINT NOT NULL REFERENCES users(id), -- pays real taka externally, receives token
    amount              NUMERIC(20, 2) NOT NULL CHECK (amount > 0), -- token amount
    price               NUMERIC(20, 4) NOT NULL,                    -- copied from the ad at order time, so a later ad edit can't change an in-flight order's price
    fiat_amount         NUMERIC(20, 2) NOT NULL,                    -- amount * price, the real taka the fiat_payer must send
    payment_method      TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending_payment'
                        CHECK (status IN ('pending_payment', 'paid', 'completed', 'cancelled', 'disputed')),
    payment_deadline    TIMESTAMPTZ NOT NULL, -- auto-cancelled if still pending_payment after this
    paid_at             TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    cancelled_at        TIMESTAMPTZ,
    dispute_reason      TEXT,
    dispute_opened_at   TIMESTAMPTZ,
    dispute_opened_by   BIGINT REFERENCES users(id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_p2p_orders_provider ON p2p_orders(token_provider_id);
CREATE INDEX idx_p2p_orders_payer ON p2p_orders(fiat_payer_id);
CREATE INDEX idx_p2p_orders_pending_deadline ON p2p_orders(status, payment_deadline) WHERE status = 'pending_payment';

COMMIT;
