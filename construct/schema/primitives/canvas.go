// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package primitives

// MediaKind is the blob type a Canvas shows as-is.
type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaVideo MediaKind = "video"
	MediaAudio MediaKind = "audio"
	MediaPage  MediaKind = "page"
	MediaChart MediaKind = "chart"
)

// CanvasMedia describes the blob a canvas renders.
type CanvasMedia struct {
	// Kind is the blob type (image|video|audio|page|chart).
	Kind MediaKind `json:"kind"`
	// URL references the blob (a media URL, a rendered-page snapshot, etc.).
	URL string `json:"url,omitempty"`
	// MIME is the optional content type.
	MIME string `json:"mime,omitempty"`
	// Alt is the accessible text alternative.
	Alt string `json:"alt,omitempty"`
}

// ChartKind is the data-driven chart shape a Canvas renders from inline data
// (rather than a media URL). It lives squarely inside the frozen
// [vocabulary.canvas] "chart" charter — it gives the agent a way to project a
// chart as DATA the trusted renderer draws, never as arbitrary markup
// (invariant i2). Gauge/single-value charts are a Metric concern, not here.
type ChartKind string

const (
	ChartArea    ChartKind = "area"
	ChartBar     ChartKind = "bar"
	ChartLine    ChartKind = "line"
	ChartPie     ChartKind = "pie"
	ChartRadar   ChartKind = "radar"
	ChartScatter ChartKind = "scatter"
)

// ChartKindValues is the frozen-ordered set of chart kinds (codegen emit order).
var ChartKindValues = []ChartKind{ChartArea, ChartBar, ChartLine, ChartPie, ChartRadar, ChartScatter}

// ValidChartKind reports whether c is a known chart kind.
func ValidChartKind(c ChartKind) bool {
	switch c {
	case ChartArea, ChartBar, ChartLine, ChartPie, ChartRadar, ChartScatter:
		return true
	default:
		return false
	}
}

// ChartSeries is one plotted series; Key indexes into each ChartPoint.Values.
type ChartSeries struct {
	// Key matches a key in every ChartPoint.Values map.
	Key string `json:"key"`
	// Name is the optional display label for the series (defaults to Key).
	Name string `json:"name,omitempty"`
	// Color is an optional explicit colour; empty lets the renderer assign
	// from the trusted palette (single accent, no rainbow).
	Color string `json:"color,omitempty"`
}

// ChartPoint is one row of chart data: an optional categorical Label (the
// x-axis tick / pie-slice / radar-spoke name) plus the numeric value per
// series key. Numbers stay numeric so the renderer can scale axes itself.
type ChartPoint struct {
	Label  string             `json:"label,omitempty"`
	Values map[string]float64 `json:"values"`
}

// Chart is inline, data-driven chart content the renderer draws trustedly
// (area|bar|line|pie|radar|scatter). Present on a Canvas whose media kind is
// "chart"; richer than a static chart image because the data survives the wire
// and the chart stays legible/responsive at any size.
type Chart struct {
	// Kind selects the chart shape (area|bar|line|pie|radar|scatter).
	Kind ChartKind `json:"kind"`
	// Series are the plotted series; for pie/single-series charts this may be
	// a single entry (or empty, inferred from the first point's value keys).
	Series []ChartSeries `json:"series,omitempty"`
	// Points are the data rows.
	Points []ChartPoint `json:"points"`
	// XLabel / YLabel are optional axis captions.
	XLabel string `json:"x_label,omitempty"`
	YLabel string `json:"y_label,omitempty"`
	// Stacked stacks series on area/bar charts when true.
	Stacked bool `json:"stacked,omitempty"`
}

// CanvasRegion is an optional interactive hotspot over the blob, expressed in
// normalised 0..1 coordinates so the renderer is resolution-independent. A
// region may point at an Ask surface to make a part of the canvas actionable.
type CanvasRegion struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	// X, Y, W, H are normalised 0..1 box coordinates over the blob.
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
	// AskRef points at an Ask surface id when the region is actionable.
	AskRef string `json:"ask_ref,omitempty"`
}

// Canvas is a show-as-is visual/media blob with optional interactive regions
// (axis: spatial + media). It covers a rendered web page, image, chart, audio,
// video (frozen [vocabulary.canvas]).
type Canvas struct {
	// Media is the blob to display.
	Media CanvasMedia `json:"media"`
	// Chart is inline data-driven chart content, set when Media.Kind ==
	// "chart"; the renderer draws it from data rather than a static image URL.
	Chart *Chart `json:"chart,omitempty"`
	// Caption is an optional human caption.
	Caption string `json:"caption,omitempty"`
	// Regions are optional interactive hotspots over the blob.
	Regions []CanvasRegion `json:"regions,omitempty"`
}
