package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/paxlabs-inc/deus/internal/auth"
	"github.com/paxlabs-inc/deus/internal/metering"
	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/streams"
	"github.com/paxlabs-inc/deus/internal/store"
)

func (g *Gateway) invokeStream(ctx context.Context, caller auth.Caller, req InvokeRequest) (InvokeResponse, error) {
	if g.streams == nil {
		return InvokeResponse{}, &Error{Code: "internal_error", Message: "stream rail not configured", HTTPStatus: 503}
	}
	if strings.TrimSpace(req.StreamID) == "" {
		return InvokeResponse{}, &Error{Code: "invalid_request", Message: "stream_id required for stream rail", HTTPStatus: 400}
	}

	svc, err := g.store.GetServiceByID(ctx, req.ServiceID)
	if err != nil {
		return InvokeResponse{}, &Error{Code: "not_found", Message: "service not found", HTTPStatus: 404}
	}
	if svc.Status != "active" {
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: "service not active", HTTPStatus: 503}
	}
	if svc.Mode != "proxy" {
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: "stream rail supports proxy mode in phase 6", HTTPStatus: 503}
	}

	streamRow, err := g.streams.GetOwned(ctx, caller, req.StreamID)
	if err != nil {
		var se *streams.Error
		if errors.As(err, &se) {
			return InvokeResponse{}, &Error{Code: se.Code, Message: se.Message, HTTPStatus: se.HTTPStatus}
		}
		return InvokeResponse{}, &Error{Code: "not_found", Message: "stream not found", HTTPStatus: 404}
	}
	if streamRow.ServiceID != req.ServiceID {
		return InvokeResponse{}, &Error{Code: "invalid_request", Message: "stream service mismatch", HTTPStatus: 400}
	}
	if streamRow.Status != "open" {
		return InvokeResponse{}, &Error{Code: "payment_required", Message: "stream closed", HTTPStatus: 402}
	}

	q, _, err := g.validateQuote(ctx, caller, req)
	if err != nil {
		return InvokeResponse{}, err
	}
	plan, err := g.pricing.PlanForOperation(ctx, req.ServiceID, req.Operation)
	if err != nil {
		return InvokeResponse{}, &Error{Code: "invalid_request", Message: err.Error(), HTTPStatus: 400}
	}

	delta, accrued, err := g.streams.MeterDelta(ctx, streamRow)
	if err != nil {
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	chargeWei := streams.ApplyMinCharge(delta, plan.MinChargeWei)

	capWei, _ := new(big.Int).SetString(streamRow.CapWei, 10)
	accWei, _ := new(big.Int).SetString(accrued, 10)
	if capWei != nil && accWei != nil && accWei.Cmp(capWei) > 0 {
		return InvokeResponse{}, &Error{Code: "payment_required", Message: "stream cap exceeded", HTTPStatus: 402}
	}

	grant, _ := g.store.ActiveGrantForCaller(ctx, caller.DID, req.ServiceID)
	if ok, msg := store.GrantAllows(grant, chargeWei); !ok {
		return InvokeResponse{}, &Error{Code: "policy_denied", Message: msg, HTTPStatus: 403}
	}

	ep, err := g.store.EndpointByServiceOperation(ctx, req.ServiceID, req.Operation)
	if err != nil {
		return InvokeResponse{}, &Error{Code: "invalid_request", Message: "unknown operation", HTTPStatus: 400}
	}
	argsHash, err := receipts.HashPayload(req.Args)
	if err != nil {
		return InvokeResponse{}, &Error{Code: "invalid_request", Message: err.Error(), HTTPStatus: 400}
	}

	row, err := g.meter.Reserve(ctx, metering.ReserveInput{
		IdempotencyKey: req.IdempotencyKey,
		ServiceID:      req.ServiceID,
		EndpointID:     ep.ID,
		CallerDID:      caller.DID,
		CallerWallet:   caller.Wallet,
		QuoteID:        req.QuoteID,
		Units:          q.MaxUnits,
		PriceWei:       chargeWei,
		PricingVersion: q.PricingVersion,
		ArgsHash:       argsHash,
	})
	if err != nil {
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	if row.Outcome == "ok" {
		return g.replaySuccess(ctx, row)
	}
	if row.Outcome == "voided" {
		return InvokeResponse{}, &Error{Code: "conflict", Message: "prior invocation voided", HTTPStatus: 409}
	}

	proxyURL := ""
	if ep.ProxyURL != nil {
		proxyURL = *ep.ProxyURL
	}
	timeoutMS := 5000
	var m struct {
		Operations []struct {
			Name      string `json:"name"`
			TimeoutMS int    `json:"timeout_ms"`
		} `json:"operations"`
	}
	_ = json.Unmarshal(svc.Manifest, &m)
	for _, op := range m.Operations {
		if op.Name == req.Operation && op.TimeoutMS > 0 {
			timeoutMS = op.TimeoutMS
		}
	}

	proxyRes, proxyErr := CallProxy(ctx, proxyURL, req.Args, timeoutMS)
	if proxyErr != nil {
		_ = g.meter.Void(ctx, row.ID)
		_ = g.quality.Sample(ctx, req.ServiceID, "voided", proxyRes.LatencyMS)
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: proxyErr.Error(), HTTPStatus: 503}
	}
	result, err := DecodeJSONResult(proxyRes.Body)
	if err != nil {
		_ = g.meter.Void(ctx, row.ID)
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: err.Error(), HTTPStatus: 503}
	}

	resultHash, err := receipts.HashPayload(result)
	if err != nil {
		_ = g.meter.Void(ctx, row.ID)
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	now := time.Now().UTC()
	rf := receipts.ReceiptFields{
		InvocationID: row.ID,
		ServiceID:    req.ServiceID,
		Caller:       caller.DID,
		ArgsHash:     argsHash,
		ResultHash:   resultHash,
		PriceWei:     chargeWei,
		Units:        q.MaxUnits,
		Outcome:      "ok",
		Timestamp:    now,
	}
	digest, sig, err := g.signer.SignReceipt(rf)
	if err != nil {
		_ = g.meter.Void(ctx, row.ID)
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	if err := g.meter.Finalize(ctx, row.ID, "ok", resultHash, q.MaxUnits, chargeWei, proxyRes.LatencyMS); err != nil {
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	if err := g.streams.RecordMeter(ctx, streamRow.ID, accrued); err != nil {
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	_ = g.store.InsertReceipt(ctx, store.ReceiptRow{
		InvocationID: row.ID,
		Digest:       digest,
		GatewaySig:   sig,
	})
	_ = g.quality.Sample(ctx, req.ServiceID, "ok", proxyRes.LatencyMS)

	return InvokeResponse{
		InvocationID: row.ID,
		Outcome:      "ok",
		Result:       result,
		ChargedWei:   chargeWei,
		LatencyMS:    proxyRes.LatencyMS,
		Receipt: ReceiptSummary{
			Digest:     digest,
			GatewaySig: sig,
		},
	}, nil
}
