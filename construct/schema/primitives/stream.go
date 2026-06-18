// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package primitives

// StreamChunk is one appended fragment of a stream. Chunks carry their own
// monotonic Seq so a progressive (patched) stream stays ordered and
// idempotent on replay.
type StreamChunk struct {
	// Seq orders the chunk within the stream.
	Seq uint64 `json:"seq"`
	// Text is the chunk content (bytes/line/event rendered as text).
	Text string `json:"text"`
	// Channel optionally distinguishes substreams (e.g. "stdout"|"stderr").
	Channel string `json:"channel,omitempty"`
}

// Stream is an append-only temporal byte/line/event sequence (axis: stream).
// It covers terminal output, logs, a raw reasoning trace (frozen
// [vocabulary.stream]). Chunks are appended progressively via
// construct.surface.patch; Closed marks the stream complete.
type Stream struct {
	// Source labels the origin (e.g. "terminal"|"log"|"reasoning").
	Source string `json:"source,omitempty"`
	// Title is an optional human header for the stream.
	Title string `json:"title,omitempty"`
	// Chunks are the appended fragments, in Seq order.
	Chunks []StreamChunk `json:"chunks,omitempty"`
	// Closed is set once the stream will receive no further chunks.
	Closed bool `json:"closed,omitempty"`
}
