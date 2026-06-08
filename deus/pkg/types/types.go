// Package types holds shared Deus wire types (docs/05-api.md).
package types

// ServiceStatus is the listing lifecycle state.
type ServiceStatus string

const (
	StatusDraft    ServiceStatus = "draft"
	StatusActive   ServiceStatus = "active"
	StatusPaused   ServiceStatus = "paused"
	StatusDelisted ServiceStatus = "delisted"
)

// ServiceSummary is a compact listing view for discovery responses.
type ServiceSummary struct {
	ID           string        `json:"id"`
	Slug         string        `json:"slug"`
	Kind         string        `json:"kind"`
	Mode         string        `json:"mode"`
	DisplayName  string        `json:"display_name"`
	Summary      string        `json:"summary"`
	Status       ServiceStatus `json:"status"`
	QualityScore string        `json:"quality_score,omitempty"`
	ManifestHash string        `json:"manifest_hash"`
}

// HealthResponse is returned by /internal/healthz.
type HealthResponse struct {
	OK       bool   `json:"ok"`
	Postgres bool   `json:"postgres"`
	Chain    bool   `json:"chain"`
	Version  string `json:"version"`
}

// ErrorEnvelope matches docs/05-api.md error model (minimal Phase 0).
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody holds a single API error.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
