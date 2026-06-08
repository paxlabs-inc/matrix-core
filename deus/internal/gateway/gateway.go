// Package gateway implements the invoke pipeline (docs/06-execution-hosting.md §6.2).
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/paxlabs-inc/deus/internal/auth"
	"github.com/paxlabs-inc/deus/internal/channels"
	"github.com/paxlabs-inc/deus/internal/metering"
	"github.com/paxlabs-inc/deus/internal/pricing"
	"github.com/paxlabs-inc/deus/internal/quality"
	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/internal/streams"
	"github.com/paxlabs-inc/deus/internal/wallet"
	"github.com/paxlabs-inc/deus/pkg/pricingmath"
)

// HostingRouter resolves active hosted execution endpoints.
type HostingRouter interface {
	ActiveEndpoint(ctx context.Context, serviceID string) (string, error)
}

// Gateway orchestrates quote → policy → reserve → route → pay → receipt.
type Gateway struct {
	store    *store.Store
	pricing  *pricing.Service
	meter    *metering.Ledger
	wallet   wallet.Client
	signer   *receipts.Signer
	quality  *quality.Service
	channels *channels.Service
	vouchers *channels.VoucherService
	hosting  HostingRouter
	streams  *streams.Service
	chainID  int64
}

// Config wires gateway dependencies.
type Config struct {
	Store    *store.Store
	Pricing  *pricing.Service
	Meter    *metering.Ledger
	Wallet   wallet.Client
	Signer   *receipts.Signer
	Quality  *quality.Service
	Channels *channels.Service
	Vouchers *channels.VoucherService
	Hosting  HostingRouter
	Streams  *streams.Service
	ChainID  int64
}

// New constructs a Gateway.
func New(cfg Config) *Gateway {
	return &Gateway{
		store:    cfg.Store,
		pricing:  cfg.Pricing,
		meter:    cfg.Meter,
		wallet:   cfg.Wallet,
		signer:   cfg.Signer,
		quality:  cfg.Quality,
		channels: cfg.Channels,
		vouchers: cfg.Vouchers,
		hosting:  cfg.Hosting,
		streams:  cfg.Streams,
		chainID:  cfg.ChainID,
	}
}

// InvokeRequest is POST /v1/invoke/{service_id}.
type InvokeRequest struct {
	ServiceID        string
	Operation        string
	Args             map[string]any
	QuoteID          string
	PaymentRail      string
	StreamID         string
	IdempotencyKey   string
	CallerVoucherSig string
}

// InvokeResponse is a successful invoke result.
type InvokeResponse struct {
	InvocationID string
	Outcome      string
	Result       map[string]any
	ChargedWei   string
	LatencyMS    int
	Receipt      ReceiptSummary
	Voucher      *VoucherSummary
}

// VoucherSummary is a cumulative channel voucher (net rail).
type VoucherSummary struct {
	ChannelID       string `json:"channel_id"`
	CumulativeWei   string `json:"cumulative_wei"`
	Nonce           int64  `json:"nonce"`
	LastReceiptHash string `json:"last_receipt_hash"`
	Digest          string `json:"digest"`
	NeedsSignature  bool   `json:"needs_signature"`
	VoucherID       string `json:"voucher_id,omitempty"`
}

// ReceiptSummary is the inline receipt envelope.
type ReceiptSummary struct {
	Digest     string  `json:"digest"`
	GatewaySig string  `json:"gateway_sig"`
	RunnerSig  *string `json:"runner_sig"`
}

// Invoke runs the full direct-rail pipeline.
func (g *Gateway) Invoke(ctx context.Context, caller auth.Caller, req InvokeRequest) (InvokeResponse, error) {
	if req.IdempotencyKey == "" {
		return InvokeResponse{}, &Error{Code: "invalid_request", Message: "idempotency_key required"}
	}
	rail := strings.ToLower(strings.TrimSpace(req.PaymentRail))
	if rail == "" {
		rail = "direct"
	}
	if rail == "stream" {
		return g.invokeStream(ctx, caller, req)
	}
	if rail == "net" {
		return g.invokeNet(ctx, caller, req)
	}
	if rail != "direct" {
		return InvokeResponse{}, &Error{Code: "invalid_request", Message: "unsupported payment rail", HTTPStatus: 400}
	}

	svc, err := g.store.GetServiceByID(ctx, req.ServiceID)
	if err != nil {
		return InvokeResponse{}, &Error{Code: "not_found", Message: "service not found", HTTPStatus: 404}
	}
	if svc.Status != "active" {
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: "service not active", HTTPStatus: 503}
	}
	switch svc.Mode {
	case "proxy":
		return g.invokeProxy(ctx, caller, req, svc)
	case "hosted":
		return g.invokeHosted(ctx, caller, req, svc)
	default:
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: "unsupported service mode", HTTPStatus: 503}
	}
}

func (g *Gateway) invokeProxy(ctx context.Context, caller auth.Caller, req InvokeRequest, svc store.ServiceRow) (InvokeResponse, error) {
	q, charge, err := g.validateQuote(ctx, caller, req)
	if err != nil {
		return InvokeResponse{}, err
	}

	grant, _ := g.store.ActiveGrantForCaller(ctx, caller.DID, req.ServiceID)
	if ok, msg := store.GrantAllows(grant, pricingmath.FormatWei(charge)); !ok {
		return InvokeResponse{}, &Error{Code: "policy_denied", Message: msg, HTTPStatus: 403}
	}
	if err := g.wallet.AuthorizeSpend(ctx, caller.Bearer, pricingmath.FormatWei(charge), req.ServiceID); err != nil {
		var pd *wallet.PolicyDenied
		if errors.As(err, &pd) {
			return InvokeResponse{}, &Error{
				Code: "policy_denied", Message: pd.Message, HTTPStatus: 403,
				Detail: map[string]any{"cap_wei": pd.CapWei, "quote_wei": pricingmath.FormatWei(charge)},
			}
		}
		return InvokeResponse{}, &Error{Code: "payment_required", Message: err.Error(), HTTPStatus: 402}
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
		PriceWei:       pricingmath.FormatWei(charge),
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
		_ = g.quality.Sample(ctx, req.ServiceID, "error", proxyRes.LatencyMS)
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: err.Error(), HTTPStatus: 503}
	}

	payout, err := g.store.DeveloperPayoutByService(ctx, req.ServiceID)
	if err != nil {
		_ = g.meter.Void(ctx, row.ID)
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	if _, err := g.wallet.Send(ctx, caller.Bearer, payout, pricingmath.FormatWei(charge)); err != nil {
		_ = g.meter.Void(ctx, row.ID)
		var pd *wallet.PolicyDenied
		if errors.As(err, &pd) {
			return InvokeResponse{}, &Error{Code: "policy_denied", Message: pd.Message, HTTPStatus: 403}
		}
		return InvokeResponse{}, &Error{Code: "payment_required", Message: err.Error(), HTTPStatus: 402}
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
		PriceWei:     pricingmath.FormatWei(charge),
		Units:        q.MaxUnits,
		Outcome:      "ok",
		Timestamp:    now,
	}
	digest, sig, err := g.signer.SignReceipt(rf)
	if err != nil {
		_ = g.meter.Void(ctx, row.ID)
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	if err := g.meter.Finalize(ctx, row.ID, "ok", resultHash, q.MaxUnits, pricingmath.FormatWei(charge), "direct", proxyRes.LatencyMS); err != nil {
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
		ChargedWei:   pricingmath.FormatWei(charge),
		LatencyMS:    proxyRes.LatencyMS,
		Receipt: ReceiptSummary{
			Digest:     digest,
			GatewaySig: sig,
		},
	}, nil
}

func (g *Gateway) invokeHosted(ctx context.Context, caller auth.Caller, req InvokeRequest, svc store.ServiceRow) (InvokeResponse, error) {
	if g.hosting == nil {
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: "hosting not configured", HTTPStatus: 503}
	}
	q, charge, err := g.validateQuote(ctx, caller, req)
	if err != nil {
		return InvokeResponse{}, err
	}

	grant, _ := g.store.ActiveGrantForCaller(ctx, caller.DID, req.ServiceID)
	if ok, msg := store.GrantAllows(grant, pricingmath.FormatWei(charge)); !ok {
		return InvokeResponse{}, &Error{Code: "policy_denied", Message: msg, HTTPStatus: 403}
	}
	if err := g.wallet.AuthorizeSpend(ctx, caller.Bearer, pricingmath.FormatWei(charge), req.ServiceID); err != nil {
		var pd *wallet.PolicyDenied
		if errors.As(err, &pd) {
			return InvokeResponse{}, &Error{
				Code: "policy_denied", Message: pd.Message, HTTPStatus: 403,
				Detail: map[string]any{"cap_wei": pd.CapWei, "quote_wei": pricingmath.FormatWei(charge)},
			}
		}
		return InvokeResponse{}, &Error{Code: "payment_required", Message: err.Error(), HTTPStatus: 402}
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
		PriceWei:       pricingmath.FormatWei(charge),
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

	execURL, err := g.hosting.ActiveEndpoint(ctx, req.ServiceID)
	if err != nil {
		_ = g.meter.Void(ctx, row.ID)
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: "hosted deployment not ready", HTTPStatus: 503}
	}

	timeoutMS, maxBytes := operationCaps(svc, req.Operation)
	hostedRes, hostedErr := CallHosted(ctx, execURL, HostedInvokeRequest{
		InvocationID: row.ID,
		Operation:    req.Operation,
		Args:         req.Args,
		CallerDID:    caller.DID,
		DeadlineMS:   timeoutMS,
	}, timeoutMS, maxBytes)
	if hostedErr != nil || hostedRes.Outcome != "ok" {
		_ = g.meter.Void(ctx, row.ID)
		_ = g.quality.Sample(ctx, req.ServiceID, "voided", hostedRes.LatencyMS)
		msg := "hosted execution failed"
		if hostedErr != nil {
			msg = hostedErr.Error()
		}
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: msg, HTTPStatus: 503}
	}
	result := hostedRes.Result
	if result == nil {
		result = map[string]any{}
	}

	payout, err := g.store.DeveloperPayoutByService(ctx, req.ServiceID)
	if err != nil {
		_ = g.meter.Void(ctx, row.ID)
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	if _, err := g.wallet.Send(ctx, caller.Bearer, payout, pricingmath.FormatWei(charge)); err != nil {
		_ = g.meter.Void(ctx, row.ID)
		var pd *wallet.PolicyDenied
		if errors.As(err, &pd) {
			return InvokeResponse{}, &Error{Code: "policy_denied", Message: pd.Message, HTTPStatus: 403}
		}
		return InvokeResponse{}, &Error{Code: "payment_required", Message: err.Error(), HTTPStatus: 402}
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
		PriceWei:     pricingmath.FormatWei(charge),
		Units:        hostedRes.Units,
		Outcome:      "ok",
		Timestamp:    now,
	}
	digest, sig, err := g.signer.SignReceipt(rf)
	if err != nil {
		_ = g.meter.Void(ctx, row.ID)
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	if err := g.meter.Finalize(ctx, row.ID, "ok", resultHash, hostedRes.Units, pricingmath.FormatWei(charge), "direct", hostedRes.LatencyMS); err != nil {
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	_ = g.store.InsertReceipt(ctx, store.ReceiptRow{
		InvocationID: row.ID,
		Digest:       digest,
		GatewaySig:   sig,
		RunnerSig:    hostedRes.RunnerSig,
	})
	_ = g.quality.Sample(ctx, req.ServiceID, "ok", hostedRes.LatencyMS)
	if dep, err := g.store.ActiveDeploymentForService(ctx, req.ServiceID); err == nil {
		_ = g.store.TouchDeploymentInvoked(ctx, dep.ID)
	}

	return InvokeResponse{
		InvocationID: row.ID,
		Outcome:      "ok",
		Result:       result,
		ChargedWei:   pricingmath.FormatWei(charge),
		LatencyMS:    hostedRes.LatencyMS,
		Receipt: ReceiptSummary{
			Digest:     digest,
			GatewaySig: sig,
			RunnerSig:  hostedRes.RunnerSig,
		},
	}, nil
}

func operationCaps(svc store.ServiceRow, operation string) (timeoutMS, maxBytes int) {
	timeoutMS = 5000
	maxBytes = 262144
	var m struct {
		Operations []struct {
			Name             string `json:"name"`
			TimeoutMS        int    `json:"timeout_ms"`
			MaxResponseBytes int    `json:"max_response_bytes"`
		} `json:"operations"`
	}
	_ = json.Unmarshal(svc.Manifest, &m)
	for _, op := range m.Operations {
		if op.Name == operation {
			if op.TimeoutMS > 0 {
				timeoutMS = op.TimeoutMS
			}
			if op.MaxResponseBytes > 0 {
				maxBytes = op.MaxResponseBytes
			}
			break
		}
	}
	return timeoutMS, maxBytes
}

func (g *Gateway) validateQuote(ctx context.Context, caller auth.Caller, req InvokeRequest) (store.QuoteRow, *big.Int, error) {
	if req.QuoteID == "" {
		return store.QuoteRow{}, nil, &Error{Code: "invalid_request", Message: "quote_id required", HTTPStatus: 400}
	}
	q, err := g.store.GetQuote(ctx, req.QuoteID)
	if err != nil {
		return store.QuoteRow{}, nil, &Error{Code: "not_found", Message: "quote not found", HTTPStatus: 404}
	}
	if q.ServiceID != req.ServiceID {
		return store.QuoteRow{}, nil, &Error{Code: "quote_expired", Message: "quote service mismatch", HTTPStatus: 409}
	}
	if q.CallerDID != caller.DID {
		return store.QuoteRow{}, nil, &Error{Code: "forbidden", Message: "quote caller mismatch", HTTPStatus: 403}
	}
	if time.Now().After(q.ExpiresAt) {
		return store.QuoteRow{}, nil, &Error{Code: "quote_expired", Message: "quote expired", HTTPStatus: 409}
	}
	fields := receipts.QuoteFields{
		ServiceID:      q.ServiceID,
		EndpointID:     q.EndpointID,
		PricingVersion: q.PricingVersion,
		UnitPriceWei:   q.UnitPriceWei,
		MaxUnits:       q.MaxUnits,
		Caller:         q.CallerDID,
		ExpiresAt:      q.ExpiresAt,
	}
	digest, _, err := g.signer.SignQuote(fields)
	if err != nil {
		return store.QuoteRow{}, nil, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	if err := g.signer.VerifyQuote(digest, q.Signature); err != nil {
		return store.QuoteRow{}, nil, &Error{Code: "quote_expired", Message: "invalid quote signature", HTTPStatus: 409}
	}
	plan, err := g.pricing.PlanForOperation(ctx, req.ServiceID, req.Operation)
	if err != nil {
		return store.QuoteRow{}, nil, &Error{Code: "invalid_request", Message: err.Error(), HTTPStatus: 400}
	}
	if plan.Version != q.PricingVersion {
		return store.QuoteRow{}, nil, &Error{Code: "quote_expired", Message: "pricing version mismatch", HTTPStatus: 409}
	}
	units, err := pricingmath.ParseUnits(q.MaxUnits)
	if err != nil {
		return store.QuoteRow{}, nil, &Error{Code: "invalid_request", Message: err.Error(), HTTPStatus: 400}
	}
	charge, err := pricingmath.Charge(q.UnitPriceWei, plan.MinChargeWei, units)
	if err != nil {
		return store.QuoteRow{}, nil, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	return q, charge, nil
}

func (g *Gateway) replaySuccess(ctx context.Context, row store.InvocationRow) (InvokeResponse, error) {
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
		ChargedWei:   row.PriceWei,
		LatencyMS:    latency,
		Receipt: ReceiptSummary{
			Digest:     rec.Digest,
			GatewaySig: rec.GatewaySig,
			RunnerSig:  rec.RunnerSig,
		},
	}, nil
}

// Error is a typed gateway failure mapped to API codes.
type Error struct {
	Code       string
	Message    string
	HTTPStatus int
	Detail     map[string]any
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}
