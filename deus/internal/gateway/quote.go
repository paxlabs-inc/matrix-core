package gateway

import (
	"context"
	"time"

	"github.com/paxlabs-inc/deus/internal/auth"
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

// QuoteResponse is the signed quote returned to agents.
type QuoteResponse struct {
	QuoteID        string
	ServiceID      string
	Operation      string
	UnitPriceWei   string
	MaxUnits       string
	MaxTotalWei    string
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
	plan, charge, err := g.pricing.Quote(ctx, req.ServiceID, req.Operation, units)
	if err != nil {
		return QuoteResponse{}, &Error{Code: "invalid_request", Message: err.Error(), HTTPStatus: 400}
	}
	expires := time.Now().UTC().Add(quoteTTL)
	fields := receipts.QuoteFields{
		ServiceID:      req.ServiceID,
		EndpointID:     ep.ID,
		PricingVersion: plan.Version,
		UnitPriceWei:   plan.UnitPriceWei,
		MaxUnits:       units,
		Caller:         caller.DID,
		ExpiresAt:      expires,
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
		MaxTotalWei:    pricingmath.FormatWei(charge),
		PricingVersion: plan.Version,
		ExpiresAt:      expires,
		EIP712: EIP712Sig{
			Domain:    "DeusQuote",
			Digest:    digest,
			Signature: sig,
		},
	}, nil
}
