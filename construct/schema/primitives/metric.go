// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package primitives

// Trend is the optional direction-of-change of a metric.
type Trend string

const (
	TrendUp   Trend = "up"
	TrendDown Trend = "down"
	TrendFlat Trend = "flat"
)

// MetricDisplay selects how a metric renders its magnitude: as plain text, a
// linear fill bar, or a radial gauge. Empty lets the renderer choose (a bar
// when a threshold is present, else plain) — preserving prior behaviour.
type MetricDisplay string

const (
	MetricDisplayPlain MetricDisplay = "plain"
	MetricDisplayBar   MetricDisplay = "bar"
	MetricDisplayGauge MetricDisplay = "gauge"
)

// MetricDisplayValues is the frozen-ordered set of metric display modes.
var MetricDisplayValues = []MetricDisplay{MetricDisplayPlain, MetricDisplayBar, MetricDisplayGauge}

// ValidMetricDisplay reports whether d is a known display mode (empty allowed:
// renderer chooses).
func ValidMetricDisplay(d MetricDisplay) bool {
	switch d {
	case "", MetricDisplayPlain, MetricDisplayBar, MetricDisplayGauge:
		return true
	default:
		return false
	}
}

// MetricThreshold marks where a metric crosses from nominal into warning or
// limit territory, so the renderer can colour a gauge/bar without inventing
// thresholds itself.
type MetricThreshold struct {
	// Warn is the value at which the metric enters a cautionary band.
	Warn float64 `json:"warn,omitempty"`
	// Limit is the hard ceiling/floor for the metric.
	Limit float64 `json:"limit,omitempty"`
	// Direction indicates whether crossing UP ("above") or DOWN ("below") the
	// threshold is the concern.
	Direction string `json:"direction,omitempty"`
}

// Metric is a named value with optional magnitude / trend / threshold / unit
// (axis: quantity). Value is a string so precision and formatting (hex, big
// decimals, currency) survive the wire intact; Magnitude is the optional
// numeric projection for bars/gauges.
type Metric struct {
	// Label names the quantity (e.g. "Block height", "Spend cap").
	Label string `json:"label"`
	// Value is the display value, kept as a string to preserve precision/units.
	Value string `json:"value"`
	// Unit is the optional unit suffix (e.g. "PAX", "ms").
	Unit string `json:"unit,omitempty"`
	// Magnitude is the optional numeric value for bar/gauge rendering.
	Magnitude float64 `json:"magnitude,omitempty"`
	// Scale is an optional human-readable magnitude scale hint (e.g. "K",
	// "M", "B", "thousands") the renderer may show alongside the value, so a
	// large number reads at a glance without the agent pre-formatting Value.
	Scale string `json:"scale,omitempty"`
	// Trend is the optional direction-of-change (up|down|flat).
	Trend Trend `json:"trend,omitempty"`
	// Display selects the render treatment (plain|bar|gauge); empty lets the
	// renderer choose from the threshold.
	Display MetricDisplay `json:"display,omitempty"`
	// Threshold marks warning/limit bands for the value.
	Threshold *MetricThreshold `json:"threshold,omitempty"`
}
