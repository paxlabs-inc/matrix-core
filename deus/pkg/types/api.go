package types

import "time"

// CreateServiceRequest is POST /v1/services.
type CreateServiceRequest struct {
	Manifest map[string]any `json:"manifest"`
}

// CreateServiceResponse is POST /v1/services response.
type CreateServiceResponse struct {
	ID           string           `json:"id"`
	Slug         string           `json:"slug"`
	Status       string           `json:"status"`
	ManifestHash string           `json:"manifest_hash"`
	Validation   ValidationResult `json:"validation"`
}

// ValidationResult is manifest validation output.
type ValidationResult struct {
	OK       bool     `json:"ok"`
	Warnings []string `json:"warnings"`
}

// ServiceResponse is GET /v1/services/{id}.
type ServiceResponse struct {
	ID           string         `json:"id"`
	Slug         string         `json:"slug"`
	Status       string         `json:"status"`
	Kind         string         `json:"kind"`
	Mode         string         `json:"mode"`
	DisplayName  string         `json:"display_name"`
	Summary      string         `json:"summary"`
	ManifestHash string         `json:"manifest_hash"`
	ChainID      *int64         `json:"chain_id,omitempty"`
	Manifest     map[string]any `json:"manifest,omitempty"`
}

// DiscoverRequest is POST /v1/discover.
type DiscoverRequest struct {
	Query   string            `json:"query"`
	Filters map[string]string `json:"filters"`
	Limit   int               `json:"limit"`
}

// DiscoverResponse is discovery output.
type DiscoverResponse struct {
	Results    []DiscoverResult `json:"results"`
	NextCursor *string          `json:"next_cursor"`
}

// DiscoverResult is one ranked listing.
type DiscoverResult struct {
	ID           string              `json:"id"`
	Slug         string              `json:"slug"`
	DisplayName  string              `json:"display_name"`
	Summary      string              `json:"summary"`
	Kind         string              `json:"kind"`
	QualityScore string              `json:"quality_score,omitempty"`
	UptimeBPS    int                 `json:"uptime_bps,omitempty"`
	Score        float64             `json:"score"`
	Operations   []DiscoverOperation `json:"operations"`
}

// DiscoverOperation is a priced operation summary for agents.
type DiscoverOperation struct {
	Name     string `json:"name"`
	PriceWei string `json:"price_wei"`
	Unit     string `json:"unit"`
}

// PublishServiceResponse is POST /v1/services/{id}/publish.
type PublishServiceResponse struct {
	ID           string `json:"id"`
	ChainID      uint64 `json:"chain_id"`
	Status       string `json:"status"`
	ManifestHash string `json:"manifest_hash"`
	TxHash       string `json:"tx_hash"`
}

// QuoteRequest is POST /v1/quote/{id}.
type QuoteRequest struct {
	Operation      string `json:"operation"`
	EstimatedUnits string `json:"estimated_units"`
}

// QuoteResponse is POST /v1/quote/{id} response.
type QuoteResponse struct {
	QuoteID        string    `json:"quote_id"`
	ServiceID      string    `json:"service_id"`
	Operation      string    `json:"operation"`
	UnitPriceWei   string    `json:"unit_price_wei"`
	MaxUnits       string    `json:"max_units"`
	MaxTotalWei    string    `json:"max_total_wei"`
	PricingVersion int       `json:"pricing_version"`
	ExpiresAt      time.Time `json:"expires_at"`
	EIP712         EIP712Sig `json:"eip712"`
}

// EIP712Sig is a signed digest envelope.
type EIP712Sig struct {
	Domain    string `json:"domain"`
	Digest    string `json:"digest"`
	Signature string `json:"signature"`
}

// InvokeRequest is POST /v1/invoke/{id}.
type InvokeRequest struct {
	Operation      string         `json:"operation"`
	Args           map[string]any `json:"args"`
	QuoteID        string         `json:"quote_id"`
	Payment        PaymentRail    `json:"payment"`
	IdempotencyKey string         `json:"idempotency_key"`
}

// PaymentRail selects settlement path.
type PaymentRail struct {
	Rail     string `json:"rail"`
	StreamID string `json:"stream_id,omitempty"`
}

// InvokeResponse is POST /v1/invoke/{id} success body.
type InvokeResponse struct {
	InvocationID string         `json:"invocation_id"`
	Outcome      string         `json:"outcome"`
	Result       map[string]any `json:"result"`
	ChargedWei   string         `json:"charged_wei"`
	LatencyMS    int            `json:"latency_ms"`
	Receipt      ReceiptSummary `json:"receipt"`
}

// ReceiptSummary is inline receipt metadata.
type ReceiptSummary struct {
	Digest     string  `json:"digest"`
	GatewaySig string  `json:"gateway_sig"`
	RunnerSig  *string `json:"runner_sig"`
	Attestation any    `json:"attestation"`
}

// InvocationResponse is GET /v1/invocations/{id}.
type InvocationResponse struct {
	ID          string         `json:"id"`
	ServiceID   string         `json:"service_id"`
	Outcome     string         `json:"outcome"`
	ChargedWei  string         `json:"charged_wei"`
	LatencyMS   *int           `json:"latency_ms,omitempty"`
	Receipt     *ReceiptDetail `json:"receipt,omitempty"`
}

// ReceiptDetail is GET /v1/receipts/{id}.
type ReceiptDetail struct {
	InvocationID string  `json:"invocation_id"`
	Digest       string  `json:"digest"`
	GatewaySig   string  `json:"gateway_sig"`
	RunnerSig    *string `json:"runner_sig,omitempty"`
}
