package gateway

import (
	"context"
	"time"

	"github.com/paxlabs-inc/deus/internal/auth"
	"github.com/paxlabs-inc/deus/internal/pricing"
	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/pkg/pricingmath"
)

const quoteTTL = 10 * time.Minute

// QuoteRequest is POST /v1/quote/{service_id}.
type QuoteRequest struct {
	ServiceID      string
	Operation      string
	EstimatedUnits string
}

// QuoteResponse is the signed quote returned to agents. USDX fields are set
// when the plan is USDX-denominated; wei fields when it carries the legacy
// denomination (both during the rail migration).
type QuoteResponse struct {
	QuoteID        string
	ServiceID      string
	Operation      string
	UnitPriceWei   string
	MaxUnits       string
	MaxTotalWei    string
	UnitPriceUSDX  string
	MaxTotalUSDX   string
	PricingVersion int
	ExpiresAt      time.Time
	EIP712         EIP712Sig
}

// EIP712Sig is the quote signature envelope.
type EIP712Sig struct {
	Domain    string `json:"domain"`
	Digest    string `json:"digest"`
	Signature string `json:"signature"`
}

// BuildQuote computes, signs, and persists a quote.
func (g *Gateway) BuildQuote(ctx context.Context, caller auth.Caller, req QuoteRequest) (QuoteResponse, error) {
	svc, err := g.store.GetServiceByID(ctx, req.ServiceID)
	if err != nil {
		return QuoteResponse{}, err
	}
	if svc.Status != "active" {
		return QuoteResponse{}, &Error{Code: "service_unavailable", Message: "service not active", HTTPStatus: 503}
	}
	ep, err := g.store.EndpointByServiceOperation(ctx, req.ServiceID, req.Operation)
	if err != nil {
		return QuoteResponse{}, &Error{Code: "invalid_request", Message: "unknown operation", HTTPStatus: 400}
	}
	units := req.EstimatedUnits
	if units == "" {
		units = "1"
	}
	plan, err := g.pricing.PlanForOperation(ctx, req.ServiceID, req.Operation)
	if err != nil {
		return QuoteResponse{}, &Error{Code: "invalid_request", Message: err.Error(), HTTPStatus: 400}
	}
	billable, err := pricing.UnitsFor(plan, units)
	if err != nil {
		return QuoteResponse{}, &Error{Code: "invalid_request", Message: err.Error(), HTTPStatus: 400}
	}
	var maxTotalWei string
	if plan.UnitPriceWei != "" {
		charge, cerr := pricingmath.Charge(plan.UnitPriceWei, plan.MinChargeWei, billable)
		if cerr != nil {
			return QuoteResponse{}, &Error{Code: "invalid_request", Message: cerr.Error(), HTTPStatus: 400}
		}
		maxTotalWei = pricingmath.FormatWei(charge)
	}
	var unitPriceUSDX, maxTotalUSDX string
	var unitPriceMicro int64
	if plan.HasUSDX() {
		chargeMicro, cerr := pricingmath.ChargeUSDX(plan.UnitPriceUSDX, plan.MinChargeUSDX, billable)
		if cerr != nil {
			return QuoteResponse{}, &Error{Code: "invalid_request", Message: cerr.Error(), HTTPStatus: 400}
		}
		unitPriceMicro, _ = pricingmath.ParseUSDX(plan.UnitPriceUSDX)
		unitPriceUSDX = pricingmath.FormatUSDX(unitPriceMicro)
		maxTotalUSDX = pricingmath.FormatUSDX(chargeMicro)
	}
	if maxTotalWei == "" && maxTotalUSDX == "" {
		return QuoteResponse{}, &Error{Code: "invalid_request", Message: "operation has no priced denomination", HTTPStatus: 400}
	}
	expires := time.Now().UTC().Add(quoteTTL)
	fields := receipts.QuoteFields{
		ServiceID:          req.ServiceID,
		EndpointID:         ep.ID,
		PricingVersion:     plan.Version,
		UnitPriceWei:       plan.UnitPriceWei,
		UnitPriceUSDXMicro: unitPriceMicro,
		MaxUnits:           units,
		Caller:             caller.DID,
		ExpiresAt:          expires,
	}
	digest, sig, err := g.signer.SignQuote(fields)
	if err != nil {
		return QuoteResponse{}, err
	}
	quoteID, err := g.store.InsertQuote(ctx, store.QuoteRow{
		ServiceID:      req.ServiceID,
		EndpointID:     ep.ID,
		PricingVersion: plan.Version,
		UnitPriceWei:   plan.UnitPriceWei,
		UnitPriceUSDX:  unitPriceUSDX,
		MaxUnits:       units,
		ExpiresAt:      expires,
		Signature:      sig,
		CallerDID:      caller.DID,
		Digest:         digest,
	})
	if err != nil {
		return QuoteResponse{}, err
	}
	return QuoteResponse{
		QuoteID:        quoteID,
		ServiceID:      req.ServiceID,
		Operation:      req.Operation,
		UnitPriceWei:   plan.UnitPriceWei,
		MaxUnits:       units,
		MaxTotalWei:    maxTotalWei,
		UnitPriceUSDX:  unitPriceUSDX,
		MaxTotalUSDX:   maxTotalUSDX,
		PricingVersion: plan.Version,
		ExpiresAt:      expires,
		EIP712: EIP712Sig{
			Domain:    "DeusQuote",
			Digest:    digest,
			Signature: sig,
		},
	}, nil
}
