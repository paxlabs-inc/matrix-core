// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package primitives

// Trend is the optional direction-of-change of a metric.
type Trend string

const (
	TrendUp   Trend = "up"
	TrendDown Trend = "down"
	TrendFlat Trend = "flat"
)

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
	// Trend is the optional direction-of-change (up|down|flat).
	Trend Trend `json:"trend,omitempty"`
	// Threshold marks warning/limit bands for the value.
	Threshold *MetricThreshold `json:"threshold,omitempty"`
}
