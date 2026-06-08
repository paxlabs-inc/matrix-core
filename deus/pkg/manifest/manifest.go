// Package manifest defines the Deus service manifest schema, validation, and hashing.
package manifest

// Manifest is the machine-readable service description (docs/03-data-model.md §3.5).
type Manifest struct {
	SchemaVersion string      `json:"schema_version"`
	Slug          string      `json:"slug"`
	Kind          string      `json:"kind"`
	DisplayName   string      `json:"display_name"`
	Summary       string      `json:"summary"`
	Description   string      `json:"description,omitempty"`
	Tags          []string    `json:"tags,omitempty"`
	Owner         string      `json:"owner"`
	PayoutAddress string      `json:"payout_address"`
	Mode          string      `json:"mode"`
	Confidential  bool        `json:"confidential,omitempty"`
	Operations    []Operation `json:"operations"`
	Pricing       []Pricing   `json:"pricing"`
	Endpoint      *Endpoint   `json:"endpoint,omitempty"`
	SLA           *SLA        `json:"sla,omitempty"`
	Attestation   any         `json:"attestation,omitempty"`
}

// Operation describes one callable operation.
type Operation struct {
	Name             string         `json:"name"`
	Method           string         `json:"method"`
	InputSchema      map[string]any `json:"input_schema"`
	OutputSchema     map[string]any `json:"output_schema"`
	TimeoutMS        int            `json:"timeout_ms,omitempty"`
	MaxResponseBytes int            `json:"max_response_bytes,omitempty"`
}

// Pricing describes per-operation pricing.
type Pricing struct {
	Operation    string `json:"operation"`
	Model        string `json:"model"`
	Unit         string `json:"unit"`
	PriceWei     string `json:"price_wei"`
	MinChargeWei string `json:"min_charge_wei"`
}

// Endpoint holds proxy or hosted routing hints.
type Endpoint struct {
	ProxyURL string `json:"proxy_url,omitempty"`
}

// SLA holds service-level targets.
type SLA struct {
	TargetUptimeBPS int `json:"target_uptime_bps,omitempty"`
	P99LatencyMS    int `json:"p99_latency_ms,omitempty"`
}
