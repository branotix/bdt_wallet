// Admin dispute resolution. These routes are NOT under /api/v1/mobile/, so
// they go through the admin console's normal AuthToken gate (see
// requireToken in api.go) rather than a user's session — resolving a
// dispute is an operator action, never something either party to the
// dispute should be able to trigger themselves.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"bdt-relayer/internal/fees"
	"bdt-relayer/internal/ledger"
)

func (s *Server) RegisterAdminP2PRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/p2p/disputes", s.handleListDisputes)
	mux.HandleFunc("POST /api/p2p/disputes/{id}/resolve", s.handleResolveDispute)
}

func (s *Server) handleListDisputes(w http.ResponseWriter, r *http.Request) {
	orders, err := s.ledger.ListDisputedOrders(r.Context())
	if err != nil {
		writeError(w, 500, "could not load disputes")
		return
	}
	writeJSON(w, 200, orders)
}

type resolveDisputeRequest struct {
	// true  = the fiat payment genuinely arrived — release escrow to the fiat payer.
	// false = it didn't — unwind escrow back to the token provider.
	// There is no third option; if you can't tell yet, don't call this endpoint
	// until you can — it is not reversible.
	FavorPayer bool `json:"favor_payer"`
}

func (s *Server) handleResolveDispute(w http.ResponseWriter, r *http.Request) {
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid order id")
		return
	}
	var req resolveDisputeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}

	err = s.ledger.AdminResolveDispute(r.Context(), orderID, req.FavorPayer, fees.P2PTradeFeePercent, fees.TreasuryUserID)
	if errors.Is(err, ledger.ErrOrderNotFound) {
		writeError(w, 404, "no disputed order with that id")
		return
	}
	if err != nil {
		writeError(w, 500, "could not resolve dispute")
		return
	}
	writeJSON(w, 200, map[string]any{
		"status":      "resolved",
		"favor_payer": req.FavorPayer,
	})
}
