// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
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
	// Caption is an optional human caption.
	Caption string `json:"caption,omitempty"`
	// Regions are optional interactive hotspots over the blob.
	Regions []CanvasRegion `json:"regions,omitempty"`
}
