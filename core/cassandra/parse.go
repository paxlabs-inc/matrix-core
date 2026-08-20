// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cassandra

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parse.go — verdict free-form JSON parsing ([adjudicator].free_form). The
// auditor returns a JSON object (not the plan grammar), possibly wrapped in
// reasoning prose or markdown fences, so we extract the first balanced object
// and unmarshal it through a tolerant DTO that also accepts the legacy
// criticVerdict shape ({"complete", "missing", "rationale"}).

// verdictDTO is the wire shape parsed from the auditor. Pointers distinguish
// "field omitted" from "field present and zero" so omitted fields can be
// inferred coherently rather than defaulting to a misleading value.
type verdictDTO struct {
	Grounded         *bool    `json:"grounded"`
	Coverage         string   `json:"coverage"`
	Complete         *bool    `json:"complete"` // legacy criticVerdict field
	Missing          []string `json:"missing"`
	UnverifiedClaims []string `json:"unverified_claims"`
	Assumptions      []string `json:"assumptions"`
	OpenUnknowns     []string `json:"open_unknowns"`
	Certainty        *float64 `json:"certainty"`
	Rationale        string   `json:"rationale"`
}

// recognized reports whether the parsed object carries at least one verdict
// field. It lets ParseVerdict skip a stray brace group (e.g. a JSON snippet a
// reasoning model embedded in prose) and find the real verdict object.
func (dto verdictDTO) recognized() bool {
	return dto.Grounded != nil ||
		dto.Complete != nil ||
		dto.Certainty != nil ||
		strings.TrimSpace(dto.Coverage) != "" ||
		dto.Missing != nil ||
		dto.UnverifiedClaims != nil ||
		dto.Assumptions != nil ||
		dto.OpenUnknowns != nil ||
		strings.TrimSpace(dto.Rationale) != ""
}

// toVerdict maps the DTO to a normalized Verdict. Omitted fields are inferred:
// coverage falls back to the legacy "complete" flag or the Missing list;
// grounded falls back to "no unverified claims". Coherence guards are applied
// via Normalize.
func (dto verdictDTO) toVerdict() *Verdict {
	v := &Verdict{
		Missing:          dto.Missing,
		UnverifiedClaims: dto.UnverifiedClaims,
		Assumptions:      dto.Assumptions,
		OpenUnknowns:     dto.OpenUnknowns,
		Rationale:        strings.TrimSpace(dto.Rationale),
	}

	switch strings.ToLower(strings.TrimSpace(dto.Coverage)) {
	case "full":
		v.Coverage = CoverageFull
	case "partial":
		v.Coverage = CoveragePartial
	default:
		// Coverage omitted: derive from the legacy complete flag when the
		// auditor supplied one. Otherwise there is NO affirmative completeness
		// signal, so fail TOWARD REFUSAL — an absent coverage signal is never
		// read as full (g4 / req 7.1). The mere absence of a missing list is
		// silence, not a claim of completion.
		switch {
		case dto.Complete != nil && *dto.Complete:
			v.Coverage = CoverageFull
		case dto.Complete != nil && !*dto.Complete:
			v.Coverage = CoveragePartial
		default:
			v.Coverage = CoveragePartial
		}
	}

	if dto.Grounded != nil {
		v.Grounded = *dto.Grounded
	} else {
		// Grounded omitted: no affirmative grounding signal, so fail TOWARD
		// REFUSAL (grounded=false) rather than inferring grounded from the mere
		// absence of a listed hallucination surface (g5 / req 7.1). A non-empty
		// unverified list would force false anyway via g2.
		v.Grounded = false
	}

	if dto.Certainty != nil {
		v.Certainty = *dto.Certainty
	}

	v.Normalize()
	return v
}

// ParseVerdict extracts the verdict object from raw auditor output. It scans
// every balanced {...} object in order (an auditor occasionally wraps the
// verdict in reasoning prose or a trailing note that may itself contain
// braces) and returns the first that BOTH parses as JSON AND carries at least
// one recognized verdict field, so a stray brace group never shadows the real
// verdict. The returned Verdict is always Normalize()d.
//
// A content-free or unparseable output yields an error; the MCL/Neo gate is
// expected to FAIL OPEN on that (a critic hiccup must never convert a clean
// run into a failure — cassandra.frozen.kvx [principles].fail_open_reversible).
func ParseVerdict(raw string) (*Verdict, error) {
	objs := candidateObjects(stripFences(raw))
	if len(objs) == 0 {
		return nil, fmt.Errorf("no JSON object found in auditor output (raw: %s)", truncateForErr(raw, 200))
	}
	var lastErr error
	for _, obj := range objs {
		var dto verdictDTO
		if err := json.Unmarshal([]byte(obj), &dto); err != nil {
			lastErr = err
			continue
		}
		if !dto.recognized() {
			continue // a stray {...} with none of our fields; keep scanning
		}
		return dto.toVerdict(), nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("unmarshal verdict: %w (json: %s)", lastErr, truncateForErr(objs[0], 200))
	}
	return nil, fmt.Errorf("no JSON object with verdict fields found in auditor output (raw: %s)", truncateForErr(raw, 200))
}

// candidateObjects returns the balanced top-level {...} objects in s, in order.
// Brace counting ignores braces inside JSON string literals and respects
// backslash escapes; a nested object is part of its enclosing object, not a
// separate candidate. Scanning stops at the first unbalanced tail (no complete
// object can follow an unclosed one).
func candidateObjects(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		if s[i] != '{' {
			i++
			continue
		}
		obj, end := balancedFrom(s, i)
		if end <= i {
			break // unbalanced from here: no complete object remains
		}
		out = append(out, obj)
		i = end
	}
	return out
}

// balancedFrom returns the balanced object starting at s[start]=='{' and the
// index just past its closing brace. On an unbalanced tail it returns
// ("", start). Brace counting is string- and escape-aware.
func balancedFrom(s string, start int) (string, int) {
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], i + 1
			}
		}
	}
	return "", start
}

// stripFences removes a leading ```json / ``` code fence wrapper if present,
// leaving the inner content. It is intentionally lenient: if no fence is found
// the input is returned unchanged.
func stripFences(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence line (``` or ```json).
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	} else {
		return s
	}
	// Drop a trailing closing fence.
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// truncateForErr bounds a string for inclusion in an error message.
func truncateForErr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
