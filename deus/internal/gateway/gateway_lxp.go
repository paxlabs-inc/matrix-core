package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/paxlabs-inc/deus/internal/auth"
	"github.com/paxlabs-inc/deus/internal/layerx"
	"github.com/paxlabs-inc/deus/internal/metering"
	"github.com/paxlabs-inc/deus/internal/pricing"
	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/pkg/lxp"
	"github.com/paxlabs-inc/deus/pkg/manifest"
	"github.com/paxlabs-inc/deus/pkg/pricingmath"
	lxtypes "github.com/paxlabs-inc/layerx/pkg/types"
)

// LXPEnabled reports whether the LayerX rail is wired.
func (g *Gateway) LXPEnabled() bool { return g.lxp != nil }

// LXP exposes the protocol half so the HTTP layer emits challenges/receipts
// through the same single implementation the pipeline settles with.
func (g *Gateway) LXP() *lxp.Server { return g.lxp }

// lxpQuote resolves what an invoke costs on the LayerX rail: the USDX charge,
// the payee DID, and the invocation-binding ref. When quote_id is present the
// signed quote is validated (service/caller/expiry/signature/version) and its
// terms are authoritative; otherwise the operation is priced fresh — the 402
// challenge carries equivalent terms, so the quote round-trip is optional.
func (g *Gateway) lxpQuote(ctx context.Context, caller auth.Caller, req InvokeRequest, svc store.ServiceRow) (lxp.Price, string, error) {
	m, err := manifest.Parse(svc.Manifest)
	if err != nil {
		return lxp.Price{}, "", &Error{Code: "internal_error", Message: "manifest unreadable", HTTPStatus: 500}
	}
	if m.PayeeDID == "" {
		return lxp.Price{}, "", &Error{Code: "service_unavailable", Message: "service has no payee_did; the LayerX rail cannot settle", HTTPStatus: 503}
	}

	argsHash, err := receipts.HashPayload(req.Args)
	if err != nil {
		return lxp.Price{}, "", &Error{Code: "invalid_request", Message: err.Error(), HTTPStatus: 400}
	}
	// The ref binds this exact invocation to its payment: provable from the
	// payer's own intent signature and both receipts (row-level v1).
	ref, err := receipts.HashPayload(map[string]any{
		"service_id": req.ServiceID,
		"operation":  req.Operation,
		"args_hash":  argsHash,
		"quote_id":   req.QuoteID,
		"caller_did": caller.DID,
	})
	if err != nil {
		return lxp.Price{}, "", &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}

	var chargeMicro int64
	if req.QuoteID != "" {
		q, err := g.validateQuoteUSDX(ctx, caller, req)
		if err != nil {
			return lxp.Price{}, "", err
		}
		chargeMicro = q
	} else {
		_, charge, err := g.pricing.QuoteUSDX(ctx, req.ServiceID, req.Operation, "1")
		if err != nil {
			return lxp.Price{}, "", &Error{Code: "invalid_request", Message: err.Error(), HTTPStatus: 400}
		}
		chargeMicro = charge
	}
	price := lxp.Price{
		AmountUSDX: pricingmath.FormatUSDX(chargeMicro),
		PayTo:      m.PayeeDID,
		Mode:       lxp.ModeExact,
		Ref:        ref,
		QuoteID:    req.QuoteID,
	}
	if m.SettlementMode == lxp.ModeHold {
		price.Mode = lxp.ModeHold
		price.TTLSeconds = m.HoldTTLS
	}
	return price, argsHash, nil
}

// validateQuoteUSDX validates a signed quote for the LayerX rail and returns
// its max USDX charge in micro. Mirrors validateQuote's checks with the USDX
// denomination authoritative.
func (g *Gateway) validateQuoteUSDX(ctx context.Context, caller auth.Caller, req InvokeRequest) (int64, error) {
	q, err := g.store.GetQuote(ctx, req.QuoteID)
	if err != nil {
		return 0, &Error{Code: "not_found", Message: "quote not found", HTTPStatus: 404}
	}
	if q.ServiceID != req.ServiceID {
		return 0, &Error{Code: "quote_expired", Message: "quote service mismatch", HTTPStatus: 409}
	}
	if q.CallerDID != caller.DID {
		return 0, &Error{Code: "forbidden", Message: "quote caller mismatch", HTTPStatus: 403}
	}
	if time.Now().After(q.ExpiresAt) {
		return 0, &Error{Code: "quote_expired", Message: "quote expired", HTTPStatus: 409}
	}
	if q.UnitPriceUSDX == "" {
		return 0, &Error{Code: "invalid_request", Message: "quote has no USDX denomination", HTTPStatus: 400}
	}
	unitMicro, err := pricingmath.ParseUSDX(q.UnitPriceUSDX)
	if err != nil {
		return 0, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	fields := receipts.QuoteFields{
		ServiceID:          q.ServiceID,
		EndpointID:         q.EndpointID,
		PricingVersion:     q.PricingVersion,
		UnitPriceWei:       q.UnitPriceWei,
		UnitPriceUSDXMicro: unitMicro,
		MaxUnits:           q.MaxUnits,
		Caller:             q.CallerDID,
		ExpiresAt:          q.ExpiresAt,
	}
	digest, _, err := g.signer.SignQuote(fields)
	if err != nil {
		return 0, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	if err := g.signer.VerifyQuote(digest, q.Signature); err != nil {
		return 0, &Error{Code: "quote_expired", Message: "invalid quote signature", HTTPStatus: 409}
	}
	plan, err := g.pricing.PlanForOperation(ctx, req.ServiceID, req.Operation)
	if err != nil {
		return 0, &Error{Code: "invalid_request", Message: err.Error(), HTTPStatus: 400}
	}
	if plan.Version != q.PricingVersion {
		return 0, &Error{Code: "quote_expired", Message: "pricing version mismatch", HTTPStatus: 409}
	}
	if !plan.HasUSDX() {
		return 0, &Error{Code: "invalid_request", Message: "operation has no USDX pricing", HTTPStatus: 400}
	}
	units, err := pricing.UnitsFor(plan, q.MaxUnits)
	if err != nil {
		return 0, &Error{Code: "invalid_request", Message: err.Error(), HTTPStatus: 400}
	}
	charge, err := pricingmath.ChargeUSDX(q.UnitPriceUSDX, plan.MinChargeUSDX, units)
	if err != nil {
		return 0, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	return charge, nil
}

// LXPChallenge prices the invoke and mints fresh lxp/1 terms for the caller —
// the 402 body (nonce prefetched for the caller's DID).
func (g *Gateway) LXPChallenge(ctx context.Context, caller auth.Caller, req InvokeRequest) (lxp.Terms, error) {
	svc, err := g.store.GetServiceByID(ctx, req.ServiceID)
	if err != nil {
		return lxp.Terms{}, &Error{Code: "not_found", Message: "service not found", HTTPStatus: 404}
	}
	price, _, perr := g.lxpQuote(ctx, caller, req, svc)
	if perr != nil {
		return lxp.Terms{}, perr
	}
	terms, err := g.lxp.Challenge(ctx, caller.DID, price)
	if err != nil {
		return lxp.Terms{}, &Error{Code: "payment_unavailable", Message: "payment rail unreachable", HTTPStatus: 503}
	}
	return terms, nil
}

// invokeLayerX is the LXP rail (DEUS-LAYERX req.6.1): verify the payer-signed
// payment against the priced terms, reserve the metering row (the
// idempotency/replay spine), then run the mode's settlement pipeline. Exact
// mode executes THEN settles, with void-on-failure metering so a failed
// execution never charges. Hold mode reserves funds inside the payer's own
// account BEFORE execution (captor = the gateway DID), captures the exact
// charge on success, and releases on failure — so the service is never
// served-unpaid and the payer is never charged-unserved. Both receipts are
// cross-bound via ref + layerx_seq. Deus never custodies: the payment moves
// payer-DID -> payee-DID on the LayerX ledger.
func (g *Gateway) invokeLayerX(ctx context.Context, caller auth.Caller, req InvokeRequest, svc store.ServiceRow) (InvokeResponse, error) {
	if !g.LXPEnabled() {
		return InvokeResponse{}, &Error{Code: "invalid_request", Message: "layerx rail not enabled", HTTPStatus: 400}
	}
	if svc.Mode != "proxy" && svc.Mode != "hosted" {
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: "unsupported service mode", HTTPStatus: 503}
	}
	if svc.Mode == "hosted" && g.hosting == nil {
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: "hosting not configured", HTTPStatus: 503}
	}
	price, argsHash, err := g.lxpQuote(ctx, caller, req, svc)
	if err != nil {
		return InvokeResponse{}, err
	}
	if req.Payment == nil {
		return InvokeResponse{}, &Error{Code: "payment_required", Message: lxp.ReasonPaymentRequired, HTTPStatus: 402}
	}
	if reason, verr := g.lxp.VerifyAgainstTerms(*req.Payment, price); verr != nil {
		return InvokeResponse{}, &Error{Code: "payment_required", Message: reason, HTTPStatus: 402}
	}
	if req.Payment.FromDID != caller.DID {
		return InvokeResponse{}, &Error{Code: "payment_required", Message: lxp.ReasonTermsMismatch, HTTPStatus: 402}
	}

	ep, err := g.store.EndpointByServiceOperation(ctx, req.ServiceID, req.Operation)
	if err != nil {
		return InvokeResponse{}, &Error{Code: "invalid_request", Message: "unknown operation", HTTPStatus: 400}
	}
	row, err := g.meter.Reserve(ctx, metering.ReserveInput{
		IdempotencyKey: req.IdempotencyKey,
		ServiceID:      req.ServiceID,
		EndpointID:     ep.ID,
		CallerDID:      caller.DID,
		CallerWallet:   caller.Wallet,
		QuoteID:        req.QuoteID,
		Units:          "1",
		PriceWei:       "0",
		PriceUSDX:      price.AmountUSDX,
		PricingVersion: 1,
		ArgsHash:       argsHash,
		Rail:           "layerx",
	})
	if err != nil {
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	// Replay with the same idempotency key returns the stored result WITHOUT a
	// second settlement — the metering ledger is the replay spine.
	if row.Outcome == "ok" {
		return g.replayLayerX(ctx, row)
	}
	if row.Outcome == "voided" {
		return InvokeResponse{}, &Error{Code: "conflict", Message: "prior invocation voided", HTTPStatus: 409}
	}

	// Hold mode: lock the charge in the payer's account before execution. A
	// failed execution releases the full amount; the hold TTL backstops a
	// gateway crash so no funds ever strand (F4 lesson, applied at the ledger).
	holdID := ""
	if price.Mode == lxp.ModeHold {
		hold, herr := g.lxp.OpenHold(ctx, *req.Payment, price.TTLSeconds)
		if herr != nil {
			_ = g.meter.Void(ctx, row.ID)
			return InvokeResponse{}, lxpSettleError(herr)
		}
		holdID = hold.HoldID
	}

	result, units, latencyMS, runnerSig, exErr := g.lxpExecute(ctx, caller, req, svc, ep, row.ID, argsHash)
	if exErr != nil {
		if holdID != "" {
			_ = g.lxp.Release(ctx, holdID)
		}
		_ = g.meter.Void(ctx, row.ID)
		return InvokeResponse{}, exErr
	}

	// Settle: hold mode captures the exact charge (remainder auto-returns to
	// the payer inside the ledger tx); exact mode submits the payer-signed pay
	// (execute-then-settle, the direct-rail ordering). A failed settlement
	// voids the row so a retry re-runs cleanly and nothing is
	// served-and-silently-unpaid twice.
	var payReceipt lxtypes.Receipt
	var serr error
	if holdID != "" {
		if payReceipt, serr = g.lxp.Capture(ctx, holdID, price.AmountUSDX); serr != nil {
			_ = g.lxp.Release(ctx, holdID)
		}
	} else {
		payReceipt, serr = g.lxp.SettleExact(ctx, *req.Payment)
	}
	if serr != nil {
		_ = g.meter.Void(ctx, row.ID)
		return InvokeResponse{}, lxpSettleError(serr)
	}

	resultHash, err := receipts.HashPayload(result)
	if err != nil {
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	now := time.Now().UTC()
	rf := receipts.ReceiptFields{
		InvocationID: row.ID,
		ServiceID:    req.ServiceID,
		Caller:       caller.DID,
		ArgsHash:     argsHash,
		ResultHash:   resultHash,
		Units:        units,
		Outcome:      "ok",
		Timestamp:    now,
		LayerXSeq:    payReceipt.Seq,
		Ref:          price.Ref,
	}
	digest, sig, err := g.signer.SignReceipt(rf)
	if err != nil {
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	if err := g.meter.Finalize(ctx, row.ID, "ok", resultHash, units, "0", latencyMS); err != nil {
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	_ = g.meter.AttachLayerX(ctx, row.ID, payReceipt.Seq, holdID)
	_ = g.store.InsertReceipt(ctx, store.ReceiptRow{
		InvocationID: row.ID,
		Digest:       digest,
		GatewaySig:   sig,
		RunnerSig:    runnerSig,
	})
	_ = g.quality.Sample(ctx, req.ServiceID, "ok", latencyMS)
	if svc.Mode == "hosted" {
		if dep, err := g.store.ActiveDeploymentForService(ctx, req.ServiceID); err == nil {
			_ = g.store.TouchDeploymentInvoked(ctx, dep.ID)
		}
	}

	return InvokeResponse{
		InvocationID: row.ID,
		Outcome:      "ok",
		Result:       result,
		ChargedUSDX:  price.AmountUSDX,
		LatencyMS:    latencyMS,
		Receipt: ReceiptSummary{
			Digest:     digest,
			GatewaySig: sig,
			RunnerSig:  runnerSig,
		},
		LayerXSeq:      payReceipt.Seq,
		Ref:            price.Ref,
		PaymentReceipt: &payReceipt,
	}, nil
}

// lxpExecute runs the priced call on the service backend — proxy or hosted —
// and returns the decoded result. Payment state is the caller's concern: on
// failure the row voids and any hold releases.
func (g *Gateway) lxpExecute(ctx context.Context, caller auth.Caller, req InvokeRequest, svc store.ServiceRow, ep store.Endpoint, invocationID, argsHash string) (map[string]any, string, int, *string, *Error) {
	timeoutMS, maxBytes := operationCaps(svc, req.Operation)
	if svc.Mode == "hosted" {
		execURL, err := g.hosting.ActiveEndpoint(ctx, req.ServiceID)
		if err != nil {
			return nil, "", 0, nil, &Error{Code: "service_unavailable", Message: "hosted deployment not ready", HTTPStatus: 503}
		}
		hostedReq := HostedInvokeRequest{
			InvocationID:  invocationID,
			Operation:     req.Operation,
			Args:          req.Args,
			CallerDID:     caller.DID,
			DeadlineMS:    timeoutMS,
			ReceiptDigest: hostedRequestDigest(invocationID, req.ServiceID, caller.DID, argsHash),
		}
		var hostedRes HostedResult
		var hostedErr error
		if isAppwriteExecutionsURL(execURL) {
			hostedRes, hostedErr = CallAppwriteExecution(ctx, execURL, g.appwriteProject, g.appwriteKey, hostedReq, timeoutMS, maxBytes)
		} else {
			hostedRes, hostedErr = CallHosted(ctx, execURL, hostedReq, timeoutMS, maxBytes)
		}
		if hostedErr != nil || hostedRes.Outcome != "ok" {
			_ = g.quality.Sample(ctx, req.ServiceID, "voided", hostedRes.LatencyMS)
			msg := "hosted execution failed"
			if hostedErr != nil {
				msg = hostedErr.Error()
			}
			return nil, "", hostedRes.LatencyMS, nil, &Error{Code: "service_unavailable", Message: msg, HTTPStatus: 503}
		}
		result := hostedRes.Result
		if result == nil {
			result = map[string]any{}
		}
		return result, hostedRes.Units, hostedRes.LatencyMS, hostedRes.RunnerSig, nil
	}

	proxyURL := ""
	if ep.ProxyURL != nil {
		proxyURL = *ep.ProxyURL
	}
	proxyRes, proxyErr := CallProxy(ctx, proxyURL, req.Args, timeoutMS)
	if proxyErr != nil {
		_ = g.quality.Sample(ctx, req.ServiceID, "voided", proxyRes.LatencyMS)
		return nil, "", proxyRes.LatencyMS, nil, &Error{Code: "service_unavailable", Message: proxyErr.Error(), HTTPStatus: 503}
	}
	result, err := DecodeJSONResult(proxyRes.Body)
	if err != nil {
		_ = g.quality.Sample(ctx, req.ServiceID, "error", proxyRes.LatencyMS)
		return nil, "", proxyRes.LatencyMS, nil, &Error{Code: "service_unavailable", Message: err.Error(), HTTPStatus: 503}
	}
	return result, "1", proxyRes.LatencyMS, nil, nil
}

// lxpSettleError maps a layerx client failure to the rail's HTTP posture:
// rail unreachable is 503 payment_unavailable (never a free call), anything
// else re-challenges with a fresh 402 and a machine-readable reason.
func lxpSettleError(err error) *Error {
	if errors.Is(err, layerx.ErrUnavailable) {
		return &Error{Code: "payment_unavailable", Message: "payment rail unreachable", HTTPStatus: 503}
	}
	reason, _ := lxp.SettleReason(err)
	return &Error{Code: "payment_required", Message: reason, HTTPStatus: 402}
}

// replayLayerX rebuilds the stored response of an already-settled layerx-rail
// invocation — one idempotency key, exactly one charge.
func (g *Gateway) replayLayerX(ctx context.Context, row store.InvocationRow) (InvokeResponse, error) {
	rec, err := g.store.GetReceipt(ctx, row.ID)
	if err != nil {
		return InvokeResponse{}, &Error{Code: "conflict", Message: "duplicate idempotency key", HTTPStatus: 409}
	}
	latency := 0
	if row.LatencyMS != nil {
		latency = *row.LatencyMS
	}
	return InvokeResponse{
		InvocationID: row.ID,
		Outcome:      row.Outcome,
		Result:       map[string]any{"replayed": true},
		ChargedUSDX:  row.PriceUSDX,
		LatencyMS:    latency,
		Receipt: ReceiptSummary{
			Digest:     rec.Digest,
			GatewaySig: rec.GatewaySig,
			RunnerSig:  rec.RunnerSig,
		},
		LayerXSeq: row.LayerXSeq,
	}, nil
}
