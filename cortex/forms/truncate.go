// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// UTF-8-safe truncation to a token budget. Pairs with memory.CountTokens
// so a string returned by TruncateToTokens always satisfies
// memory.CountTokens(out) ≤ maxTokens.
//
// Why this matters: Render emits short and medium by template substitution.
// A long Statement or Rationale field can blow past the budget even though
// the template scaffold is small. Truncation is the only way to keep
// per-type renders byte-budget-stable across user input distributions.

package forms

import (
	"unicode/utf8"

	"matrix/cortex/memory"
)

// Ellipsis is the suffix appended when truncation occurs. One byte ("…" is
// 3 bytes in UTF-8 = 1 token under bytes/4) was avoided so that very tight
// budgets still leave room for content. Using "..." would consume 3 bytes
// = 1 token which is a smaller fraction of medium's 200-token budget; we
// pick "…" anyway because it reads cleaner in CLI output and the cost is
// 1 token either way under bytes/4 ceiling.
const Ellipsis = "…"

// TruncateToTokens trims s so that memory.CountTokens(out) ≤ maxTokens. If
// truncation occurs, an Ellipsis is appended and the trim point is moved
// inward to make room for it without exceeding the budget. UTF-8 boundaries
// are respected — a multi-byte rune is never split mid-codepoint.
//
// maxTokens ≤ 0 returns the empty string.
func TruncateToTokens(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	if memory.CountTokens(s) <= maxTokens {
		return s
	}
	// Target byte budget. Under ceil division, any string of length
	// ≤ maxBytes is guaranteed to count as ≤ maxTokens tokens.
	maxBytes := maxTokens * memory.BytesPerToken
	ellBytes := len(Ellipsis)
	if maxBytes <= ellBytes {
		// Budget too small even for the ellipsis. Hand back a UTF-8-safe
		// prefix of the ellipsis itself; this only fires for pathological
		// callers (maxTokens=0 already handled above).
		return safePrefix(Ellipsis, maxBytes)
	}
	prefix := safePrefix(s, maxBytes-ellBytes)
	return prefix + Ellipsis
}

// safePrefix returns the longest UTF-8 prefix of s with length ≤ maxBytes.
// Walks runes from the start so a multi-byte codepoint that would straddle
// the cap is dropped entirely.
func safePrefix(s string, maxBytes int) string {
	if maxBytes <= 0 || s == "" {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	// Step rune-by-rune until adding the next rune would exceed maxBytes.
	end := 0
	for end < len(s) {
		_, size := utf8.DecodeRuneInString(s[end:])
		if end+size > maxBytes {
			break
		}
		end += size
	}
	return s[:end]
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
