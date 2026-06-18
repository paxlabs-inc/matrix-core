// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Payload robustness (memory capabilities v2, Phase 4.2). Neo's transcript
// messages are plain text, so an inline image arrives as a base64 data URI
// embedded in a user message or a tool result. Two problems follow:
//
//   - A base64 blob is ~4 chars per 3 bytes, so the bytes/4 token heuristic
//     over-counts a single thumbnail by 5-10x versus what a vision model
//     actually bills (~1.6k tokens per tile). Left uncorrected, one small image
//     can trip a forced compaction every turn. imageAwareTokens charges a FLAT
//     per-image cost instead.
//   - Old image payloads are dead weight once they have been discussed. Under a
//     critical (413 / byte-cap) threshold the older turns' blobs are replaced
//     with a short placeholder, keeping the surrounding tool text.
//
// Pure Neo RAM-tier hygiene: no cortex bytes, no replay surface.
package agent

import (
	"strings"

	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
)

// flatImageTokens is the fixed token budget charged for one inline image
// payload instead of counting its base64 length.
const flatImageTokens = 1600

// imagePlaceholder replaces a stripped inline image payload — short, explicit,
// and carrying no base64 weight.
const imagePlaceholder = "[inline image omitted to fit context]"

// dataURIMarker begins an inline base64 data URI: data:<media-type>;base64,<blob>.
const dataURIMarker = "data:"

// base64Marker is the token separating the media type from the payload.
const base64Marker = ";base64,"

// maxMediaTypeSpan bounds how far after "data:" the ";base64," marker may sit
// for the run to count as a data URI (a real media type is short, e.g.
// "image/png"); anything longer is treated as ordinary prose containing the
// word "data:".
const maxMediaTypeSpan = 64

// minImageBlob is the smallest base64 payload treated as a strippable inline
// image. Short data URIs in prose (a tiny icon, an example) are left verbatim;
// only genuine payloads (a few hundred+ bytes) are charged flat / stripped.
const minImageBlob = 256

// base64Char reports whether b is part of a base64 payload (standard +
// url-safe alphabet, padding, and the line breaks a wrapped blob may contain).
func base64Char(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '+', b == '/', b == '=', b == '-', b == '_':
		return true
	case b == '\n', b == '\r':
		return true
	default:
		return false
	}
}

// stripDataURIs replaces every inline base64 image data URI in s with
// imagePlaceholder and returns the rewritten string plus the count replaced.
// Single allocation-light pass; a short data URI (blob < minImageBlob) or a
// "data:" that is not a base64 URI is left untouched. Deterministic.
func stripDataURIs(s string) (string, int) {
	if !strings.Contains(s, dataURIMarker) {
		return s, 0
	}
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	i := 0
	for i < len(s) {
		j := strings.Index(s[i:], dataURIMarker)
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		j += i
		b.WriteString(s[i:j]) // text before the marker

		semi := strings.Index(s[j:], base64Marker)
		if semi < 0 || semi > maxMediaTypeSpan {
			// Not a base64 data URI (or the media type is implausibly long):
			// emit the marker literally and continue scanning past it.
			b.WriteString(dataURIMarker)
			i = j + len(dataURIMarker)
			continue
		}
		blobStart := j + semi + len(base64Marker)
		k := blobStart
		for k < len(s) && base64Char(s[k]) {
			k++
		}
		if k-blobStart < minImageBlob {
			// Too short to be a real image payload — keep it verbatim.
			b.WriteString(s[j:k])
			i = k
			continue
		}
		b.WriteString(imagePlaceholder)
		n++
		i = k
	}
	return b.String(), n
}

// imageAwareTokens estimates the token cost of s, charging a flat per-image
// budget for inline base64 image payloads instead of their (wildly larger)
// base64 character length.
func imageAwareTokens(s string) int {
	stripped, n := stripDataURIs(s)
	return memory.EstimateTokens(stripped) + n*flatImageTokens
}

// stripImagesIn replaces inline image payloads in the given messages in place
// and returns the number stripped.
func stripImagesIn(msgs []llm.Message) int {
	n := 0
	for i := range msgs {
		if msgs[i].Content == "" {
			continue
		}
		s, k := stripDataURIs(msgs[i].Content)
		if k > 0 {
			msgs[i].Content = s
			n += k
		}
	}
	return n
}

// keepRecentUserTurns is how many of the most recent user turns (and their
// following assistant/tool messages) compaction preserves VERBATIM; only older
// turns are summarized. Keeping a verbatim tail protects immediate working
// context from summary lossiness.
const keepRecentUserTurns = 3

// splitForCompaction divides the working transcript into an older section to
// summarize and a recent tail to keep verbatim, split at a user-message
// boundary so a tool-result is never orphaned from its assistant tool-call.
// Returns older=nil when there are too few user turns to carve a clean split
// (the caller then degrades rather than summarizing recent context away). The
// recent tail is copied so later appends never alias the older backing array.
func splitForCompaction(msgs []llm.Message, keepUserTurns int) (older, recent []llm.Message) {
	if keepUserTurns <= 0 || len(msgs) == 0 {
		return msgs, nil
	}
	seen := 0
	boundary := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleUser {
			seen++
			if seen == keepUserTurns {
				boundary = i
				break
			}
		}
	}
	if boundary <= 0 {
		return nil, msgs
	}
	rec := make([]llm.Message, len(msgs)-boundary)
	copy(rec, msgs[boundary:])
	return msgs[:boundary], rec
}

// stripOldImages drops inline image payloads from all but the most recent
// turns, returning the count stripped. Recent turns keep their images (the
// model may still be working with them); only older, already-discussed payloads
// are replaced. Used by the byte-cap backstop and the 413 recovery path as a
// cheaper, less lossy first step before a full compaction.
func (a *Agent) stripOldImages() int {
	older, recent := splitForCompaction(a.working, keepRecentUserTurns)
	if older == nil {
		// No clean split — strip everything but the final message so the very
		// latest payload (most likely the one in play) survives.
		cut := len(a.working) - 1
		if cut < 0 {
			cut = 0
		}
		return stripImagesIn(a.working[:cut])
	}
	_ = recent
	return stripImagesIn(older)
}
