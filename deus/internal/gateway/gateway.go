// Package gateway implements the invoke pipeline (docs/06-execution-hosting.md §6.2).
package gateway

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/paxlabs-inc/deus/internal/auth"
	"github.com/paxlabs-inc/deus/internal/metering"
	"github.com/paxlabs-inc/deus/internal/pricing"
	"github.com/paxlabs-inc/deus/internal/quality"
	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/pkg/lxp"
	lxtypes "github.com/paxlabs-inc/layerx/pkg/types"
)

// HostingRouter resolves active hosted execution endpoints.
type HostingRouter interface {
	ActiveEndpoint(ctx context.Context, serviceID string) (string, error)
}

// Gateway orchestrates quote → challenge/verify → reserve → execute → settle
// → receipt over the LXP rail. Every payment is USDX payer-DID → payee-DID on
// the LayerX ledger; deus never custodies funds.
type Gateway struct {
	store           *store.Store
	pricing         *pricing.Service
	meter           *metering.Ledger
	signer          *receipts.Signer
	quality         *quality.Service
	hosting         HostingRouter
	lxp             *lxp.Server
	chainID         int64
	appwriteProject string
	appwriteKey     string
}

// Config wires gateway dependencies.
type Config struct {
	Store   *store.Store
	Pricing *pricing.Service
	Meter   *metering.Ledger
	Signer  *receipts.Signer
	Quality *quality.Service
	Hosting HostingRouter
	ChainID int64
	// LXP is the LayerX-rail protocol half (challenge/verify/settle) — the
	// only payment rail.
	LXP *lxp.Server
	// AppwriteProject/AppwriteKey authenticate the Appwrite executions API when
	// a hosted deployment routes through Paxeer Cloud (exec endpoint ends in
	// /executions). Empty for dev/runner endpoints.
	AppwriteProject string
	AppwriteKey     string
}

// New constructs a Gateway.
func New(cfg Config) *Gateway {
	return &Gateway{
		store:           cfg.Store,
		pricing:         cfg.Pricing,
		meter:           cfg.Meter,
		signer:          cfg.Signer,
		quality:         cfg.Quality,
		hosting:         cfg.Hosting,
		lxp:             cfg.LXP,
		chainID:         cfg.ChainID,
		appwriteProject: cfg.AppwriteProject,
		appwriteKey:     cfg.AppwriteKey,
	}
}

// InvokeRequest is POST /v1/invoke/{service_id}.
type InvokeRequest struct {
	ServiceID      string
	Operation      string
	Args           map[string]any
	QuoteID        string
	PaymentRail    string
	IdempotencyKey string
	// Payment is the decoded X-LayerX-Payment header (nil means unpaid — the
	// handler answers with a 402 challenge).
	Payment *lxp.Payment
}

// InvokeResponse is a successful invoke result.
type InvokeResponse struct {
	InvocationID string
	Outcome      string
	Result       map[string]any
	ChargedUSDX  string
	LatencyMS    int
	Receipt      ReceiptSummary
	// LayerX settlement cross-binding.
	LayerXSeq      int64
	Ref            string
	PaymentReceipt *lxtypes.Receipt
}

// ReceiptSummary is the inline receipt envelope.
type ReceiptSummary struct {
	Digest     string  `json:"digest"`
	GatewaySig string  `json:"gateway_sig"`
	RunnerSig  *string `json:"runner_sig"`
}

// Invoke runs the LXP invoke pipeline — the only payment rail.
func (g *Gateway) Invoke(ctx context.Context, caller auth.Caller, req InvokeRequest) (InvokeResponse, error) {
	if req.IdempotencyKey == "" {
		return InvokeResponse{}, &Error{Code: "invalid_request", Message: "idempotency_key required"}
	}
	if rail := strings.ToLower(strings.TrimSpace(req.PaymentRail)); rail != "" && rail != "layerx" {
		return InvokeResponse{}, &Error{Code: "invalid_request", Message: "unsupported payment rail (payments ride LXP on LayerX)", HTTPStatus: 400}
	}
	svc, err := g.store.GetServiceByID(ctx, req.ServiceID)
	if err != nil {
		return InvokeResponse{}, &Error{Code: "not_found", Message: "service not found", HTTPStatus: 404}
	}
	if svc.Status != "active" {
		return InvokeResponse{}, &Error{Code: "service_unavailable", Message: "service not active", HTTPStatus: 503}
	}
	return g.invokeLayerX(ctx, caller, req, svc)
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

// hostedRequestDigest binds the fields the gateway knows before execution into
// a stable digest the runner can co-sign. It is keccak256 over canonical JSON
// (same hashing as receipt payloads) and does not include the result hash,
// which is unknown until the runner returns.
func hostedRequestDigest(invocationID, serviceID, callerDID, argsHash string) string {
	d, err := receipts.HashPayload(map[string]any{
		"invocation_id": invocationID,
		"service_id":    serviceID,
		"caller_did":    callerDID,
		"args_hash":     argsHash,
	})
	if err != nil {
		return ""
	}
	return d
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
