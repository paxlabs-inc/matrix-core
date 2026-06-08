package gateway

import (
	"context"
	"strings"
	"time"

	"github.com/paxlabs-inc/deus/internal/auth"
	"github.com/paxlabs-inc/deus/internal/channels"
	"github.com/paxlabs-inc/deus/internal/metering"
	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/pkg/pricingmath"
)

// OpenChannel opens a funded payment channel for the caller.
func (g *Gateway) OpenChannel(ctx context.Context, caller auth.Caller, capWei, fundTx string) (store.ChannelRow, error) {
	if g.channels == nil {
		return store.ChannelRow{}, &Error{Code: "internal_error", Message: "channels not configured", HTTPStatus: 503}
	}
	return g.channels.Open(ctx, channels.OpenInput{
		CallerDID:    caller.DID,
		CallerWallet: caller.Wallet,
		CapWei:       capWei,
		FundTx:       fundTx,
		Bearer:       caller.Bearer,
	})
}

// CosignVoucher persists a caller-signed cumulative voucher.
func (g *Gateway) CosignVoucher(ctx context.Context, caller auth.Caller, in channels.CosignInput) (string, error) {
	if g.vouchers == nil {
		return "", &Error{Code: "internal_error", Message: "vouchers not configured", HTTPStatus: 503}
	}
	in.CallerWallet = caller.Wallet
	return g.vouchers.Cosign(ctx, in)
}

func (g *Gateway) replayNet(ctx context.Context, row store.InvocationRow) (InvokeResponse, error) {
	res, err := g.replaySuccess(ctx, row)
	if err != nil {
		return res, err
	}
	if row.Outcome == "pending_voucher" {
		res.Voucher = &VoucherSummary{NeedsSignature: true}
	}
	return res, nil
}

func (g *Gateway) invokeNet(ctx context.Context, caller auth.Caller, req InvokeRequest) (InvokeResponse, error) {
	if g.channels == nil || g.vouchers == nil {
		return InvokeResponse{}, &Error{Code: "internal_error", Message: "net rail not configured", HTTPStatus: 503}
	}
	svc, err := g.store.GetServiceByID(ctx, req.ServiceID)
	if err != nil {
		return InvokeResponse{}, &Error{Code: "not_found", Message: "service not found", HTTPStatus: 404}
	}
	if svc.Status != "active" || svc.Mode != "proxy" {
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: "service unavailable", HTTPStatus: 503}
	}

	q, charge, err := g.validateQuote(ctx, caller, req)
	if err != nil {
		return InvokeResponse{}, err
	}
	chargeWei := pricingmath.FormatWei(charge)

	ch, err := g.channels.Active(ctx, caller.DID)
	if err != nil {
		return InvokeResponse{}, &Error{Code: "payment_required", Message: "open a payment channel first", HTTPStatus: 402}
	}
	if err := g.channels.Reserve(ctx, ch.ID, chargeWei); err != nil {
		return InvokeResponse{}, &Error{Code: "payment_required", Message: "insufficient channel balance", HTTPStatus: 402}
	}

	ep, err := g.store.EndpointByServiceOperation(ctx, req.ServiceID, req.Operation)
	if err != nil {
		_ = g.channels.Void(ctx, ch.ID, chargeWei)
		return InvokeResponse{}, &Error{Code: "invalid_request", Message: "unknown operation", HTTPStatus: 400}
	}
	argsHash, err := receipts.HashPayload(req.Args)
	if err != nil {
		_ = g.channels.Void(ctx, ch.ID, chargeWei)
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
		_ = g.channels.Void(ctx, ch.ID, chargeWei)
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	if row.Outcome == "ok" || row.Outcome == "pending_voucher" {
		return g.replayNet(ctx, row)
	}

	proxyURL := ""
	if ep.ProxyURL != nil {
		proxyURL = *ep.ProxyURL
	}
	proxyRes, proxyErr := CallProxy(ctx, proxyURL, req.Args, 5000)
	if proxyErr != nil {
		_ = g.meter.Void(ctx, row.ID)
		_ = g.channels.Void(ctx, ch.ID, chargeWei)
		_ = g.quality.Sample(ctx, req.ServiceID, "voided", proxyRes.LatencyMS)
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: proxyErr.Error(), HTTPStatus: 503}
	}
	result, err := DecodeJSONResult(proxyRes.Body)
	if err != nil {
		_ = g.meter.Void(ctx, row.ID)
		_ = g.channels.Void(ctx, ch.ID, chargeWei)
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: err.Error(), HTTPStatus: 503}
	}

	resultHash, err := receipts.HashPayload(result)
	if err != nil {
		_ = g.meter.Void(ctx, row.ID)
		_ = g.channels.Void(ctx, ch.ID, chargeWei)
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
		_ = g.channels.Void(ctx, ch.ID, chargeWei)
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	sigIn := strings.TrimSpace(req.CallerVoucherSig)
	inlineCosign := sigIn != ""

	ch, _ = g.store.GetChannelByID(ctx, ch.ID)
	pending, err := g.vouchers.BuildPending(ch, chargeWei, digest)
	if err != nil {
		_ = g.meter.Void(ctx, row.ID)
		_ = g.channels.Void(ctx, ch.ID, chargeWei)
		return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}

	if inlineCosign {
		if err := g.meter.Finalize(ctx, row.ID, "ok", resultHash, q.MaxUnits, chargeWei, "net", proxyRes.LatencyMS); err != nil {
			_ = g.channels.Void(ctx, ch.ID, chargeWei)
			return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
		}
	} else {
		if err := g.meter.FinalizePendingVoucher(ctx, row.ID, resultHash, q.MaxUnits, chargeWei, proxyRes.LatencyMS); err != nil {
			_ = g.channels.Void(ctx, ch.ID, chargeWei)
			return InvokeResponse{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
		}
	}
	_ = g.store.InsertReceipt(ctx, store.ReceiptRow{InvocationID: row.ID, Digest: digest, GatewaySig: sig})
	_ = g.quality.Sample(ctx, req.ServiceID, "ok", proxyRes.LatencyMS)

	voucher := &VoucherSummary{
		ChannelID:       pending.ChannelID,
		CumulativeWei:   pending.CumulativeWei,
		Nonce:           pending.Nonce,
		LastReceiptHash: pending.LastReceiptHash,
		Digest:          pending.Digest,
		NeedsSignature:  !inlineCosign,
	}
	outcome := "ok"
	if !inlineCosign {
		outcome = "pending_voucher"
	}

	if inlineCosign {
		vid, err := g.vouchers.Cosign(ctx, channels.CosignInput{
			ChannelID:       pending.ChannelID,
			CumulativeWei:   pending.CumulativeWei,
			ChargeWei:       chargeWei,
			Nonce:           pending.Nonce,
			LastReceiptHash: pending.LastReceiptHash,
			Digest:          pending.Digest,
			CallerSig:       sigIn,
			CallerWallet:    caller.Wallet,
		})
		if err != nil {
			return InvokeResponse{}, &Error{Code: "invalid_request", Message: err.Error(), HTTPStatus: 400}
		}
		voucher.NeedsSignature = false
		voucher.VoucherID = vid
	}

	return InvokeResponse{
		InvocationID: row.ID,
		Outcome:      outcome,
		Result:       result,
		ChargedWei:   chargeWei,
		LatencyMS:    proxyRes.LatencyMS,
		Receipt:      ReceiptSummary{Digest: digest, GatewaySig: sig},
		Voucher:      voucher,
	}, nil
}
