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

// QuoteResponse is POST /v1/quote/{id} response. USDX fields are present for
// USDX-denominated plans (the LayerX rail); wei fields for legacy plans.
type QuoteResponse struct {
	QuoteID        string    `json:"quote_id"`
	ServiceID      string    `json:"service_id"`
	Operation      string    `json:"operation"`
	UnitPriceWei   string    `json:"unit_price_wei,omitempty"`
	MaxUnits       string    `json:"max_units"`
	MaxTotalWei    string    `json:"max_total_wei,omitempty"`
	UnitPriceUSDX  string    `json:"unit_price_usdx,omitempty"`
	MaxTotalUSDX   string    `json:"max_total_usdx,omitempty"`
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

// charged_usdx, layerx_seq and ref cross-bind the execution receipt to the
// LayerX payment receipt riding the X-LayerX-Receipt header.

// InvokeRequest is POST /v1/invoke/{id}.
type InvokeRequest struct {
	Operation      string         `json:"operation"`
	Args           map[string]any `json:"args"`
	QuoteID        string         `json:"quote_id"`
	Payment        PaymentRail    `json:"payment"`
	IdempotencyKey string         `json:"idempotency_key"`
}

// PaymentRail names the settlement rail ("layerx" or empty; LXP is the only
// rail).
type PaymentRail struct {
	Rail string `json:"rail"`
}

// InvokeResponse is POST /v1/invoke/{id} success body.
type InvokeResponse struct {
	InvocationID string         `json:"invocation_id"`
	Outcome      string         `json:"outcome"`
	Result       map[string]any `json:"result"`
	ChargedUSDX  string         `json:"charged_usdx,omitempty"`
	LatencyMS    int            `json:"latency_ms"`
	Receipt      ReceiptSummary `json:"receipt"`
	LayerXSeq    int64          `json:"layerx_seq,omitempty"`
	Ref          string         `json:"ref,omitempty"`
}

// ReceiptSummary is inline receipt metadata.
type ReceiptSummary struct {
	Digest      string  `json:"digest"`
	GatewaySig  string  `json:"gateway_sig"`
	RunnerSig   *string `json:"runner_sig"`
	Attestation any     `json:"attestation"`
}

// InvocationResponse is GET /v1/invocations/{id}.
type InvocationResponse struct {
	ID         string         `json:"id"`
	ServiceID  string         `json:"service_id"`
	Outcome    string         `json:"outcome"`
	ChargedWei string         `json:"charged_wei"`
	LatencyMS  *int           `json:"latency_ms,omitempty"`
	Receipt    *ReceiptDetail `json:"receipt,omitempty"`
}

// ReceiptDetail is GET /v1/receipts/{id}.
type ReceiptDetail struct {
	InvocationID string  `json:"invocation_id"`
	Digest       string  `json:"digest"`
	GatewaySig   string  `json:"gateway_sig"`
	RunnerSig    *string `json:"runner_sig,omitempty"`
}

// UploadArtifactResponse is POST /v1/services/{id}/artifacts.
type UploadArtifactResponse struct {
	ArtifactKey string `json:"artifact_key"`
	URL         string `json:"url,omitempty"`
}

// DeployServiceRequest is POST /v1/services/{id}/deploy.
type DeployServiceRequest struct {
	ArtifactKey string `json:"artifact_key"`
	Runtime     string `json:"runtime"`
	AlwaysWarm  bool   `json:"always_warm"`
	Region      string `json:"region,omitempty"`
}

// DeployServiceResponse is POST /v1/services/{id}/deploy.
type DeployServiceResponse struct {
	DeploymentID string `json:"deployment_id"`
	Status       string `json:"status"`
	ExecEndpoint string `json:"exec_endpoint,omitempty"`
	Runtime      string `json:"runtime"`
}

// DeploymentResponse is GET /v1/services/{id}/deployments/{deployment_id}.
type DeploymentResponse struct {
	ID           string `json:"id"`
	ServiceID    string `json:"service_id"`
	Status       string `json:"status"`
	Runtime      string `json:"runtime"`
	ExecEndpoint string `json:"exec_endpoint,omitempty"`
	AlwaysWarm   bool   `json:"always_warm"`
}

// DeploymentListResponse is GET /v1/services/{id}/deployments.
type DeploymentListResponse struct {
	ServiceID   string               `json:"service_id"`
	Deployments []DeploymentResponse `json:"deployments"`
}

// CatalogItem is one published listing in the public catalog.
type CatalogItem struct {
	ID           string   `json:"id"`
	Slug         string   `json:"slug"`
	Kind         string   `json:"kind"`
	Mode         string   `json:"mode"`
	DisplayName  string   `json:"display_name"`
	Summary      string   `json:"summary"`
	Status       string   `json:"status"`
	ManifestHash string   `json:"manifest_hash"`
	QualityScore string   `json:"quality_score,omitempty"`
	UptimeBPS    int      `json:"uptime_bps,omitempty"`
	PriceWei     string   `json:"price_wei,omitempty"`
	Unit         string   `json:"unit,omitempty"`
	Tags         []string `json:"tags,omitempty"`
}

// CatalogResponse is GET /v1/catalog (public, paginated).
type CatalogResponse struct {
	Services []CatalogItem `json:"services"`
	Total    int           `json:"total"`
	Limit    int           `json:"limit"`
	Offset   int           `json:"offset"`
}

// ServiceStatusResponse is POST /v1/services/{id}/pause|delist.
type ServiceStatusResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// LogLine is one dashboard activity-log entry.
type LogLine struct {
	TS      time.Time `json:"ts"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// LogsResponse is GET /v1/services/{id}/logs.
type LogsResponse struct {
	Logs []LogLine `json:"logs"`
}

// AnalyticsPoint is one day in the analytics series.
type AnalyticsPoint struct {
	Date         string  `json:"date"`
	Invocations  int     `json:"invocations"`
	RevenueWei   string  `json:"revenue_wei"`
	RevenueUSDX  string  `json:"revenue_usdx"`
	AvgLatencyMS int     `json:"avg_latency_ms"`
	SuccessRate  float64 `json:"success_rate"`
}

// TopOperation is per-operation usage in analytics.
type TopOperation struct {
	Operation   string `json:"operation"`
	Invocations int    `json:"invocations"`
	RevenueWei  string `json:"revenue_wei"`
	RevenueUSDX string `json:"revenue_usdx"`
}

// ServiceAnalyticsResponse is GET /v1/services/{id}/analytics.
type ServiceAnalyticsResponse struct {
	ServiceID        string           `json:"service_id"`
	TotalInvocations int              `json:"total_invocations"`
	TotalRevenueWei  string           `json:"total_revenue_wei"`
	TotalRevenueUSDX string           `json:"total_revenue_usdx"`
	AvgLatencyMS     int              `json:"avg_latency_ms"`
	SuccessRate      float64          `json:"success_rate"`
	UptimeBPS        int              `json:"uptime_bps"`
	Series           []AnalyticsPoint `json:"series"`
	TopOperations    []TopOperation   `json:"top_operations"`
}

// MeResponse is GET /v1/me.
type MeResponse struct {
	DID         string `json:"did"`
	Wallet      string `json:"wallet,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

// MyService is one developer-owned listing with usage aggregates.
type MyService struct {
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	DisplayName  string `json:"display_name"`
	Status       string `json:"status"`
	Kind         string `json:"kind"`
	Mode         string `json:"mode"`
	Invocations  int    `json:"invocations"`
	RevenueWei   string `json:"revenue_wei"`
	RevenueUSDX  string `json:"revenue_usdx"`
	UptimeBPS    int    `json:"uptime_bps,omitempty"`
	QualityScore string `json:"quality_score,omitempty"`
}

// MyServicesResponse is GET /v1/me/services.
type MyServicesResponse struct {
	Services []MyService `json:"services"`
}

// SettlementSummary is one payout window in earnings.
type SettlementSummary struct {
	ID          string    `json:"id"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	AmountWei   string    `json:"amount_wei"`
	Status      string    `json:"status"`
	TxHash      string    `json:"tx_hash,omitempty"`
}

// LayerXEarnings is the LXP-rail earnings view: every settlement pays the
// developer's payee DID instantly on LayerX, so there is no pending/settled
// split and no deus payout machinery — withdrawal is a link-out to layerxd.
type LayerXEarnings struct {
	PayeeDID    string `json:"payee_did,omitempty"`
	EarnedUSDX  string `json:"earned_usdx"`
	Invocations int    `json:"invocations"`
	BalanceUSDX string `json:"balance_usdx,omitempty"`
	EscrowUSDX  string `json:"escrow_usdx,omitempty"`
	LayerXURL   string `json:"layerx"`
	WithdrawURL string `json:"withdraw_url"`
}

// EarningsResponse is GET /v1/me/earnings.
type EarningsResponse struct {
	TotalEarnedWei string              `json:"total_earned_wei"`
	PendingWei     string              `json:"pending_wei"`
	AvailableWei   string              `json:"available_wei"`
	PayoutAddress  string              `json:"payout_address,omitempty"`
	Settlements    []SettlementSummary `json:"settlements"`
	LayerX         *LayerXEarnings     `json:"layerx,omitempty"`
}

// SpendEntry is one service's share of caller spend.
type SpendEntry struct {
	ServiceID   string `json:"service_id"`
	DisplayName string `json:"display_name"`
	Invocations int    `json:"invocations"`
	TotalWei    string `json:"total_wei"`
	TotalUSDX   string `json:"total_usdx"`
}

// SpendResponse is GET /v1/me/spend.
type SpendResponse struct {
	TotalSpentWei string       `json:"total_spent_wei"`
	TotalSpentUSDX string      `json:"total_spent_usdx"`
	Entries       []SpendEntry `json:"entries"`
}
