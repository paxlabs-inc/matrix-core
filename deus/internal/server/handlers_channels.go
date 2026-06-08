package server

import (
	"encoding/json"
	"net/http"

	"github.com/paxlabs-inc/deus/internal/auth"
	"github.com/paxlabs-inc/deus/internal/channels"
	"github.com/paxlabs-inc/deus/pkg/types"
)

func (s *Server) handleOpenChannel(w http.ResponseWriter, r *http.Request) {
	if s.deps.Gateway == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "gateway not configured", nil)
		return
	}
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "caller required", nil)
		return
	}
	var body types.OpenChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid json", nil)
		return
	}
	ch, err := s.deps.Gateway.OpenChannel(r.Context(), caller, body.CapWei, body.FundTx, body.EscrowAddr)
	if err != nil {
		s.writeGatewayErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, types.ChannelResponse{
		ID:            ch.ID,
		CallerDID:     ch.CallerDID,
		BalanceWei:    ch.BalanceWei,
		ReservedWei:   ch.ReservedWei,
		CumulativeWei: ch.CumulativeWei,
		WindowEnd:     ch.WindowEnd,
		Status:        ch.Status,
	})
}

func (s *Server) handleVoucherCosign(w http.ResponseWriter, r *http.Request) {
	if s.deps.Gateway == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "gateway not configured", nil)
		return
	}
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "caller required", nil)
		return
	}
	var body types.VoucherCosignRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid json", nil)
		return
	}
	id, err := s.deps.Gateway.CosignVoucher(r.Context(), caller, channels.CosignInput{
		ChannelID:       body.ChannelID,
		CumulativeWei:   body.CumulativeWei,
		ChargeWei:       body.ChargeWei,
		Nonce:           body.Nonce,
		LastReceiptHash: body.LastReceiptHash,
		Digest:          body.Digest,
		CallerSig:       body.CallerSig,
	})
	if err != nil {
		s.writeGatewayErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, types.VoucherCosignResponse{VoucherID: id})
}

func (s *Server) handleSettleRun(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settler == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "settler not configured", nil)
		return
	}
	var body struct {
		DeveloperID string `json:"developer_id"`
		PayoutAddr  string `json:"payout_address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid json", nil)
		return
	}
	res, err := s.deps.Settler.RunWindow(r.Context(), body.DeveloperID, body.PayoutAddr)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settlement_id": res.SettlementID,
		"total_wei":     res.TotalWei,
		"count":         res.Count,
		"merkle_root":   res.MerkleRoot,
		"tx_hash":       res.TxHash,
	})
}
