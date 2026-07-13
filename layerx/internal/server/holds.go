package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/paxlabs-inc/layerx/internal/auth"
	"github.com/paxlabs-inc/layerx/internal/store"
	"github.com/paxlabs-inc/layerx/pkg/types"
)

// maxHoldTTL bounds a hold's lifetime so no payer's funds can be locked
// indefinitely by a stalled captor (expiry auto-releases via the sweep).
const maxHoldTTL = 24 * time.Hour

// handleHold is POST /v1/hold — authorize: debit the payer's spendable balance
// into an open hold naming a payer-authorized captor, a fixed payee, an amount
// bound, and a TTL. Same dual authorization as /v1/pay (principal token or
// DID-signed intent, invariant i6); insufficient balance is a 402 in parity
// with pay.
func (s *Server) handleHold(w http.ResponseWriter, r *http.Request) {
	var req types.HoldRequest
	if !decode(w, r, &req) {
		return
	}
	if _, err := auth.ParseDID(req.ToDID); err != nil {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "to_did must be a valid did:matrix")
		return
	}
	if _, err := auth.ParseDID(req.CaptorDID); err != nil {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "captor_did must be a valid did:matrix")
		return
	}
	amount, err := types.ParseUSDX(req.AmountUSDX)
	if err != nil || amount <= 0 {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "amount_usdx must be a positive USDX decimal")
		return
	}
	if req.TTLSeconds <= 0 || time.Duration(req.TTLSeconds)*time.Second > maxHoldTTL {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, fmt.Sprintf("ttl_s must be in (0, %d]", int64(maxHoldTTL.Seconds())))
		return
	}
	ref := strings.TrimSpace(req.Ref)
	if ref != "" && !refRe.MatchString(ref) {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "ref must be a 0x-prefixed 32-byte hex digest")
		return
	}
	// The signed hold intent binds every capture bound the payer consents to:
	// payee, normalized amount, ttl, ref (may be empty), and the captor.
	preimage := auth.IntentMessage("hold", req.FromDID, req.Nonce,
		req.ToDID, types.FormatUSDX(amount), fmt.Sprintf("%d", req.TTLSeconds), ref, req.CaptorDID)
	payerDID, ok := s.writeCaller(w, r, req.FromDID, req.PublicKey, req.Nonce, req.Signature, preimage)
	if !ok {
		return
	}
	h, err := s.store.CreateHold(r.Context(), payerDID, req.ToDID, req.CaptorDID, amount, ref,
		time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInsufficientFunds):
			writeFail(w, http.StatusPaymentRequired, types.CodeInsufficientFunds, "insufficient escrow-backed balance")
		case errors.Is(err, store.ErrNotFound):
			writeFail(w, http.StatusPaymentRequired, types.CodeInsufficientFunds, "account not funded; deposit first")
		default:
			s.log.Error("hold failed", "error", err.Error())
			writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not create hold")
		}
		return
	}
	s.log.Info("hold created", "hold", h.ID, "payer", payerDID, "payee", req.ToDID, "captor", req.CaptorDID)
	writeJSON(w, http.StatusOK, types.OK(toHoldView(h)))
}

// handleHoldCapture is POST /v1/hold/{id}/capture — authorized ONLY as the
// hold's payer-consented captor_did, bounded by amount, the fixed payee, and
// expiry. The capture consumes the hold and emits a STANDARD transfer (same
// seq/leaf/sequencer-sig path as pay); the response carries that receipt.
func (s *Server) handleHoldCapture(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !uuidRe.MatchString(id) {
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "hold not found")
		return
	}
	var req types.CaptureRequest
	if !decode(w, r, &req) {
		return
	}
	amount, err := types.ParseUSDX(req.AmountUSDX)
	if err != nil || amount <= 0 {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "amount_usdx must be a positive USDX decimal")
		return
	}
	preimage := auth.IntentMessage("capture", req.FromDID, req.Nonce, id, types.FormatUSDX(amount))
	captorDID, ok := s.writeCaller(w, r, req.FromDID, req.PublicKey, req.Nonce, req.Signature, preimage)
	if !ok {
		return
	}
	receipt, h, err := s.ledger.CaptureHold(r.Context(), id, captorDID, amount)
	if err != nil {
		s.writeHoldError(w, err, "capture")
		return
	}
	// A capture IS a transfer: same live-stream + tiered-settlement policy as pay.
	if receipt.Tier == types.TierMaterial && s.settler != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if _, err := s.settler.SettleNow(ctx); err != nil {
				s.log.Error("auto force-settle failed", "seq", receipt.Seq, "error", err.Error())
			}
		}()
	}
	s.log.Info("hold captured", "hold", h.ID, "seq", receipt.Seq, "captor", captorDID)
	s.publishTransfer(receipt, h.PayerDID, h.PayeeDID)
	writeJSON(w, http.StatusOK, types.OK(types.CaptureResponse{Hold: toHoldView(h), Receipt: receipt}))
}

// handleHoldRelease is POST /v1/hold/{id}/release — authorized as the hold's
// captor or payer; returns the full held amount to the payer. Idempotent.
func (s *Server) handleHoldRelease(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !uuidRe.MatchString(id) {
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "hold not found")
		return
	}
	var req types.ReleaseRequest
	if !decode(w, r, &req) {
		return
	}
	preimage := auth.IntentMessage("release", req.FromDID, req.Nonce, id)
	callerDID, ok := s.writeCaller(w, r, req.FromDID, req.PublicKey, req.Nonce, req.Signature, preimage)
	if !ok {
		return
	}
	h, err := s.store.ReleaseHold(r.Context(), id, callerDID)
	if err != nil {
		s.writeHoldError(w, err, "release")
		return
	}
	s.log.Info("hold released", "hold", h.ID, "caller", callerDID)
	writeJSON(w, http.StatusOK, types.OK(toHoldView(h)))
}

// handleHoldGet is GET /v1/hold/{id} — a public read (PHASE 4 transparency).
func (s *Server) handleHoldGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !uuidRe.MatchString(id) {
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "hold not found")
		return
	}
	h, err := s.store.GetHold(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeFail(w, http.StatusNotFound, types.CodeNotFound, "hold not found")
			return
		}
		s.log.Error("get hold failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not read hold")
		return
	}
	writeJSON(w, http.StatusOK, types.OK(toHoldView(h)))
}

// writeHoldError maps the store's hold lifecycle errors onto the stable wire
// codes.
func (s *Server) writeHoldError(w http.ResponseWriter, err error, op string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "hold not found")
	case errors.Is(err, store.ErrNotCaptor):
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "caller is not authorized for this hold")
	case errors.Is(err, store.ErrHoldExpired):
		writeFail(w, http.StatusConflict, types.CodeConflict, "hold expired")
	case errors.Is(err, store.ErrHoldClosed):
		writeFail(w, http.StatusConflict, types.CodeConflict, "hold is not open")
	case errors.Is(err, store.ErrCaptureExceedsHold):
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "capture amount exceeds held amount")
	default:
		s.log.Error("hold "+op+" failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not "+op+" hold")
	}
}

func toHoldView(h store.Hold) types.HoldView {
	v := types.HoldView{
		HoldID:     h.ID,
		PayerDID:   h.PayerDID,
		PayeeDID:   h.PayeeDID,
		CaptorDID:  h.CaptorDID,
		AmountUSDX: types.FormatUSDX(h.AmountMicro),
		Ref:        h.Ref,
		Status:     h.Status,
		ExpiresAt:  h.ExpiresAt,
		CreatedAt:  h.CreatedAt,
		UpdatedAt:  h.UpdatedAt,
	}
	if h.Status == "captured" {
		v.CapturedUSDX = types.FormatUSDX(h.CapturedMicro)
		v.CaptureSeq = h.CaptureSeq
	}
	return v
}
