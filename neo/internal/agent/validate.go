// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"regexp"
	"strings"
)

// The compaction validator implements the frozen spec's
// [transparency.authorship] "silent cheap pass": after the summary is authored
// it is validated against the active-session schema and — load-bearing — every
// high-entropy token (addresses, tx hashes, IDs, file paths, exact amounts) is
// confirmed to have survived VERBATIM. A paraphrased 0x… is a corrupted memory
// (invariant i3), so any dropped identifier is re-appended rather than lost.
//
// This is a deterministic pass on purpose: it is cheaper and far more reliable
// than asking a model to grade its own summary, and it makes the trust contract
// unit-testable without a network round-trip.

// highEntropyRes match the token classes the verbatim rule protects. Order is
// not significant; results are de-duplicated by value.
var highEntropyRes = []*regexp.Regexp{
	regexp.MustCompile(`0x[0-9a-fA-F]{6,}`),                                                               // hex addresses / hashes / selectors
	regexp.MustCompile(`\b[0-9a-fA-F]{40,}\b`),                                                            // bare hex (e.g. a Merkle root without 0x)
	regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), // UUID
	regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{26}\b`),                                                      // Crockford ULID (cortex memory IDs)
	regexp.MustCompile(`(?:/[A-Za-z0-9._-]+){2,}`),                                                        // absolute file paths
	regexp.MustCompile(`\b\d{7,}\b`),                                                                      // long exact numbers (wei, block heights, ids)
}

// schemaSections are the active-session template headers the summary should
// fill ([memory.schema.active_session]). Presence of the load-bearing few is a
// cheap signal the summary is well-formed.
var schemaSections = []string{"GOAL", "NEXT"}

// extractHighEntropyTokens returns the de-duplicated set of high-entropy tokens
// appearing in s, in first-seen order.
func extractHighEntropyTokens(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, re := range highEntropyRes {
		for _, m := range re.FindAllString(s, -1) {
			if len(m) < 6 || seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// summaryHasSchema reports whether the summary fills the load-bearing template
// headers (a weak summary is still kept — never discarded — but the caller may
// note it).
func summaryHasSchema(summary string) bool {
	up := strings.ToUpper(summary)
	for _, h := range schemaSections {
		if !strings.Contains(up, h) {
			return false
		}
	}
	return true
}

// validateSummary enforces the verbatim contract on a freshly authored summary:
// any high-entropy token present in the source transcript but missing from the
// summary is re-appended under a preserved-identifiers line, so compaction can
// never silently drop an address, hash, id, or path. Returns the repaired
// summary and whether it passed clean (schema present and nothing was missing).
func validateSummary(transcript, summary string) (string, bool) {
	summary = strings.TrimSpace(summary)
	tokens := extractHighEntropyTokens(transcript)

	var missing []string
	for _, tok := range tokens {
		if !strings.Contains(summary, tok) {
			missing = append(missing, tok)
		}
	}

	clean := summaryHasSchema(summary) && len(missing) == 0
	if len(missing) == 0 {
		return summary, clean
	}

	var b strings.Builder
	b.WriteString(summary)
	if summary != "" {
		b.WriteString("\n")
	}
	b.WriteString("ARTIFACTS (preserved verbatim): ")
	b.WriteString(strings.Join(missing, ", "))
	return b.String(), clean
}
