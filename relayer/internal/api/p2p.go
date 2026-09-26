// P2P trading endpoints. All money-moving actions (order creation, payment
// confirmation, cancellation) use the authenticated session's user_id —
// never a value from the request body — so nobody can act as anyone but
// themselves. See internal/ledger/p2p_store.go for the actual escrow logic;
// this file is just HTTP plumbing around it.
package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"

	"bdt-relayer/internal/fees"
	"bdt-relayer/internal/ledger"
)

func (s *Server) RegisterP2PRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/mobile/p2p/ads", wrap(s.requireMobileAuth(s.handleCreateAd)))
	mux.HandleFunc("GET /api/v1/mobile/p2p/ads", wrap(s.handleListAds))
	mux.HandleFunc("GET /api/v1/mobile/p2p/ads/mine", wrap(s.requireMobileAuth(s.handleListMyAds)))
	mux.HandleFunc("POST /api/v1/mobile/p2p/ads/{id}/cancel", wrap(s.requireMobileAuth(s.handleCancelAd)))

	mux.HandleFunc("POST /api/v1/mobile/p2p/orders", wrap(s.requireMobileAuth(s.handleCreateOrder)))
	mux.HandleFunc("GET /api/v1/mobile/p2p/orders", wrap(s.requireMobileAuth(s.handleListMyOrders)))
	mux.HandleFunc("GET /api/v1/mobile/p2p/orders/{id}", wrap(s.requireMobileAuth(s.handleGetOrder)))
	mux.HandleFunc("POST /api/v1/mobile/p2p/orders/{id}/mark-paid", wrap(s.requireMobileAuth(s.handleMarkPaid)))
	mux.HandleFunc("POST /api/v1/mobile/p2p/orders/{id}/confirm", wrap(s.requireMobileAuth(s.handleConfirmPayment)))
	mux.HandleFunc("POST /api/v1/mobile/p2p/orders/{id}/cancel", wrap(s.requireMobileAuth(s.handleCancelOrder)))
	mux.HandleFunc("POST /api/v1/mobile/p2p/orders/{id}/dispute", wrap(s.requireMobileAuth(s.handleDisputeOrder)))
}

// --- Ads ---

type createAdRequest struct {
	Side           string `json:"side"` // "sell" or "buy"
	Price          string `json:"price"`
	MinOrder       string `json:"min_order"`
	MaxOrder       string `json:"max_order"`
	TotalAmount    string `json:"total_amount"`
	PaymentMethods string `json:"payment_methods"` // "bKash,Nagad,Bank"
}

func (s *Server) handleCreateAd(w http.ResponseWriter, r *http.Request) {
	userID := authedUserID(r)
	var req createAdRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	price, err1 := decimal.NewFromString(req.Price)
	minOrder, err2 := decimal.NewFromString(req.MinOrder)
	maxOrder, err3 := decimal.NewFromString(req.MaxOrder)
	total, err4 := decimal.NewFromString(req.TotalAmount)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil ||
		price.LessThanOrEqual(decimal.Zero) || minOrder.LessThanOrEqual(decimal.Zero) ||
		maxOrder.LessThan(minOrder) || total.LessThan(minOrder) {
		writeError(w, 400, "invalid ad parameters")
		return
	}
	if req.PaymentMethods == "" {
		writeError(w, 400, "at least one payment method required")
		return
	}

	// A 'sell' ad promises to provide up to `total` token — sanity-check the
	// maker actually has at least that much AVAILABLE right now. This is a
	// courtesy check at ad-creation time, not an escrow (nothing is locked
	// here) — the maker's balance could still change before an order comes
	// in, which is exactly why CreateOrder re-checks available balance for
	// real at order time.
	if req.Side == "sell" {
		balance, err := s.ledger.AvailableBalance(r.Context(), userID)
		if err != nil {
			log.Printf("p2p create ad: available balance: %v", err)
			writeError(w, 500, "could not check available balance")
			return
		}
		if balance.LessThan(total) {
			writeError(w, 402, "you don't have enough available BDT to back a sell ad of this size")
			return
		}
	}

	adID, err := s.ledger.CreateAd(r.Context(), userID, req.Side, price, minOrder, maxOrder, total, req.PaymentMethods)
	if err != nil {
		log.Printf("p2p create ad: %v", err)
		writeError(w, 500, "could not create ad")
		return
	}
	writeJSON(w, 201, map[string]any{"ad_id": adID})
}

func (s *Server) handleListAds(w http.ResponseWriter, r *http.Request) {
	side := r.URL.Query().Get("side")
	ads, err := s.ledger.ListActiveAds(r.Context(), side)
	if err != nil {
		writeError(w, 500, "could not load ads")
		return
	}
	writeJSON(w, 200, ads)
}

func (s *Server) handleListMyAds(w http.ResponseWriter, r *http.Request) {
	ads, err := s.ledger.ListUserAds(r.Context(), authedUserID(r))
	if err != nil {
		writeError(w, 500, "could not load ads")
		return
	}
	writeJSON(w, 200, ads)
}

func (s *Server) handleCancelAd(w http.ResponseWriter, r *http.Request) {
	adID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid ad id")
		return
	}
	err = s.ledger.CancelAd(r.Context(), adID, authedUserID(r))
	switch {
	case errors.Is(err, ledger.ErrNotOrderParty):
		writeError(w, 403, "not your ad")
	case errors.Is(err, ledger.ErrAdHasOpenOrders):
		writeError(w, 409, "cannot cancel — this ad has open orders in progress")
	case err != nil:
		writeError(w, 500, "could not cancel ad")
	default:
		writeJSON(w, 200, map[string]any{"status": "cancelled"})
	}
}

// --- Orders ---

type createOrderRequest struct {
	AdID          int64  `json:"ad_id"`
	Amount        string `json:"amount"`
	PaymentMethod string `json:"payment_method"`
}

func (s *Server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	userID := authedUserID(r)
	var req createOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		writeError(w, 400, "invalid amount")
		return
	}
	req.PaymentMethod = strings.TrimSpace(req.PaymentMethod)
	if len(req.PaymentMethod) < 2 || len(req.PaymentMethod) > 40 {
		writeError(w, 400, "invalid payment method")
		return
	}

	orderID, err := s.ledger.CreateOrder(r.Context(), req.AdID, userID, amount, req.PaymentMethod)
	switch {
	case errors.Is(err, ledger.ErrAdNotFound):
		writeError(w, 404, "ad not found or no longer active")
	case errors.Is(err, ledger.ErrCannotTradeOwnAd):
		writeError(w, 400, "cannot open an order against your own ad")
	case errors.Is(err, ledger.ErrAmountOutOfRange):
		writeError(w, 400, "amount is outside this ad's min/max order size")
	case errors.Is(err, ledger.ErrAmountExceedsAd):
		writeError(w, 409, "amount exceeds what's remaining on this ad")
	case errors.Is(err, ledger.ErrInsufficientBalance):
		writeError(w, 402, "the token provider doesn't have enough available BDT for this order size")
	case errors.Is(err, ledger.ErrPaymentMethodNotAllowed):
		writeError(w, 400, "payment method is not offered by this ad")
	case err != nil:
		log.Printf("p2p create order: %v", err)
		writeError(w, 500, "could not create order")
	default:
		writeJSON(w, 201, map[string]any{
			"order_id":           orderID,
			"payment_window_min": 15,
		})
	}
}

func (s *Server) handleListMyOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := s.ledger.ListUserOrders(r.Context(), authedUserID(r))
	if err != nil {
		writeError(w, 500, "could not load orders")
		return
	}
	writeJSON(w, 200, orders)
}

func (s *Server) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid order id")
		return
	}
	order, err := s.ledger.GetOrder(r.Context(), orderID)
	if err != nil {
		writeError(w, 404, "order not found")
		return
	}
	userID := authedUserID(r)
	if order.TokenProviderID != userID && order.FiatPayerID != userID {
		writeError(w, 403, "not your order")
		return
	}
	writeJSON(w, 200, order)
}

func (s *Server) handleMarkPaid(w http.ResponseWriter, r *http.Request) {
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid order id")
		return
	}
	err = s.ledger.MarkPaid(r.Context(), orderID, authedUserID(r))
	if errors.Is(err, ledger.ErrWrongOrderStatus) {
		writeError(w, 409, "this order can't be marked paid right now (wrong status, or not your order)")
		return
	}
	if err != nil {
		writeError(w, 500, "could not update order")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "paid"})
}

func (s *Server) handleConfirmPayment(w http.ResponseWriter, r *http.Request) {
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid order id")
		return
	}
	err = s.ledger.ConfirmPayment(r.Context(), orderID, authedUserID(r), fees.P2PTradeFeePercent, fees.TreasuryUserID)
	switch {
	case errors.Is(err, ledger.ErrNotOrderParty):
		writeError(w, 403, "only the token provider can confirm this order")
	case errors.Is(err, ledger.ErrWrongOrderStatus):
		writeError(w, 409, "order is not awaiting confirmation")
	case errors.Is(err, ledger.ErrOrderNotFound):
		writeError(w, 404, "order not found")
	case err != nil:
		log.Printf("p2p confirm payment: %v", err)
		writeError(w, 500, "could not confirm payment")
	default:
		writeJSON(w, 200, map[string]any{"status": "completed"})
	}
}

func (s *Server) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid order id")
		return
	}
	err = s.ledger.CancelOrder(r.Context(), orderID, authedUserID(r))
	switch {
	case errors.Is(err, ledger.ErrNotOrderParty):
		writeError(w, 403, "not your order")
	case errors.Is(err, ledger.ErrWrongOrderStatus):
		writeError(w, 409, "this order can no longer be cancelled directly — open a dispute instead")
	case err != nil:
		writeError(w, 500, "could not cancel order")
	default:
		writeJSON(w, 200, map[string]any{"status": "cancelled"})
	}
}

type disputeRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) handleDisputeOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid order id")
		return
	}
	var req disputeRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // reason is optional context, not required for the dispute to be valid

	err = s.ledger.DisputeOrder(r.Context(), orderID, authedUserID(r), req.Reason)
	if errors.Is(err, ledger.ErrWrongOrderStatus) {
		writeError(w, 409, "this order can't be disputed right now")
		return
	}
	if err != nil {
		writeError(w, 500, "could not open dispute")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "disputed"})
}
