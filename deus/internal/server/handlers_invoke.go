package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/paxlabs-inc/deus/internal/auth"
	"github.com/paxlabs-inc/deus/internal/gateway"
	"github.com/paxlabs-inc/deus/pkg/types"
)

func (s *Server) mountInvokeRoutes(r chi.Router) {
	r.Route("/v1", func(r chi.Router) {
		r.Use(auth.Middleware(s.deps.DevMode))
		r.Post("/quote/{id}", s.handleQuote)
		r.Post("/invoke/{id}", s.handleInvoke)
		r.Get("/invocations/{id}", s.handleGetInvocation)
		r.Get("/receipts/{id}", s.handleGetReceipt)
		r.Post("/channels", s.handleOpenChannel)
		r.Post("/vouchers/cosign", s.handleVoucherCosign)
	})
	r.Post("/internal/settle/run", s.handleSettleRun)
}

func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	if s.deps.Gateway == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "gateway not configured", nil)
		return
	}
	serviceID := chi.URLParam(r, "id")
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "caller required", nil)
		return
	}
	var body types.QuoteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid json body", nil)
		return
	}
	res, err := s.deps.Gateway.BuildQuote(r.Context(), caller, gateway.QuoteRequest{
		ServiceID:      serviceID,
		Operation:      body.Operation,
		EstimatedUnits: body.EstimatedUnits,
	})
	if err != nil {
		s.writeGatewayErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, types.QuoteResponse{
		QuoteID:        res.QuoteID,
		ServiceID:      res.ServiceID,
		Operation:      res.Operation,
		UnitPriceWei:   res.UnitPriceWei,
		MaxUnits:       res.MaxUnits,
		MaxTotalWei:    res.MaxTotalWei,
		PricingVersion: res.PricingVersion,
		ExpiresAt:      res.ExpiresAt,
		EIP712: types.EIP712Sig{
			Domain:    res.EIP712.Domain,
			Digest:    res.EIP712.Digest,
			Signature: res.EIP712.Signature,
		},
	})
}

func (s *Server) handleInvoke(w http.ResponseWriter, r *http.Request) {
	if s.deps.Gateway == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "gateway not configured", nil)
		return
	}
	serviceID := chi.URLParam(r, "id")
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "caller required", nil)
		return
	}
	var body types.InvokeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid json body", nil)
		return
	}
	idem := strings.TrimSpace(body.IdempotencyKey)
	if idem == "" {
		idem = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	rail := ""
	if body.Payment.Rail != "" {
		rail = body.Payment.Rail
	}
	res, err := s.deps.Gateway.Invoke(r.Context(), caller, gateway.InvokeRequest{
		ServiceID:        serviceID,
		Operation:        body.Operation,
		Args:             body.Args,
		QuoteID:          body.QuoteID,
		PaymentRail:      rail,
		IdempotencyKey:   idem,
		CallerVoucherSig: body.CallerVoucherSig,
	})
	if err != nil {
		s.writeGatewayErr(w, err)
		return
	}
	var voucher *types.VoucherSummary
	if res.Voucher != nil {
		voucher = &types.VoucherSummary{
			ChannelID:       res.Voucher.ChannelID,
			CumulativeWei:   res.Voucher.CumulativeWei,
			Nonce:           res.Voucher.Nonce,
			LastReceiptHash: res.Voucher.LastReceiptHash,
			Digest:          res.Voucher.Digest,
			NeedsSignature:  res.Voucher.NeedsSignature,
			VoucherID:       res.Voucher.VoucherID,
		}
	}
	writeJSON(w, http.StatusOK, types.InvokeResponse{
		InvocationID: res.InvocationID,
		Outcome:      res.Outcome,
		Result:       res.Result,
		ChargedWei:   res.ChargedWei,
		LatencyMS:    res.LatencyMS,
		Receipt: types.ReceiptSummary{
			Digest:     res.Receipt.Digest,
			GatewaySig: res.Receipt.GatewaySig,
			RunnerSig:  res.Receipt.RunnerSig,
		},
		Voucher: voucher,
	})
}

func (s *Server) handleGetInvocation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	row, err := s.deps.Store.GetInvocation(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "not_found", "invocation not found", nil)
		return
	}
	var receipt *types.ReceiptDetail
	if rec, err := s.deps.Store.GetReceipt(r.Context(), id); err == nil {
		receipt = &types.ReceiptDetail{
			InvocationID: rec.InvocationID,
			Digest:       rec.Digest,
			GatewaySig:   rec.GatewaySig,
			RunnerSig:    rec.RunnerSig,
		}
	}
	writeJSON(w, http.StatusOK, types.InvocationResponse{
		ID:         row.ID,
		ServiceID:  row.ServiceID,
		Outcome:    row.Outcome,
		ChargedWei: row.PriceWei,
		LatencyMS:  row.LatencyMS,
		Receipt:    receipt,
	})
}

func (s *Server) handleGetReceipt(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rec, err := s.deps.Store.GetReceipt(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "not_found", "receipt not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, types.ReceiptDetail{
		InvocationID: rec.InvocationID,
		Digest:       rec.Digest,
		GatewaySig:   rec.GatewaySig,
		RunnerSig:    rec.RunnerSig,
	})
}

func (s *Server) writeGatewayErr(w http.ResponseWriter, err error) {
	var ge *gateway.Error
	if errors.As(err, &ge) {
		status := ge.HTTPStatus
		if status == 0 {
			status = http.StatusBadRequest
		}
		writeAPIError(w, status, ge.Code, ge.Message, ge.Detail)
		return
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		writeAPIError(w, http.StatusNotFound, "not_found", msg, nil)
	case strings.Contains(msg, "expired"):
		writeAPIError(w, http.StatusConflict, "quote_expired", msg, nil)
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal_error", msg, nil)
	}
}
