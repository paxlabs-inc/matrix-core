// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package decide is Cody's decision-phase primitive: generate N divergent
// candidates HOT (at the mode's decision temperature) and pick one COLD (an
// adjudication that is instructed to reject the default path of least
// resistance). Divergence happens by construction — temperature plus a
// candidate count — not by asking one lukewarm sample to be bold. The stack
// decision (SDR), the design-language decision (DLR), and plan-shape authoring
// all run through this seam; worker implementation never does (it runs cold).
package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"matrix/cody/internal/llm"
)

// Candidate is one generated option, in generation order.
type Candidate struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// Decision is the judge's verdict: the picked candidate index and why.
type Decision struct {
	Pick      int    `json:"pick"`
	Rationale string `json:"rationale"`
}

// Generate calls the HOT client n times with the same system+user prompt;
// divergence comes from the decision temperature the client is built at. It
// returns the candidates in call order. n < 1 is treated as 1. An empty
// (whitespace-only) candidate is dropped — a blank generation is not an option.
func Generate(ctx context.Context, hot *llm.Client, system, user string, n int) ([]Candidate, error) {
	if hot == nil {
		return nil, errors.New("decide: nil client")
	}
	if n < 1 {
		n = 1
	}
	out := make([]Candidate, 0, n)
	for i := 0; i < n; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res, err := hot.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{
			llm.SystemMessage(system),
			llm.UserMessage(user),
		}})
		if err != nil {
			return nil, fmt.Errorf("decide: generate candidate %d/%d: %w", i+1, n, err)
		}
		text := strings.TrimSpace(res.Message.Content)
		if text == "" {
			continue
		}
		out = append(out, Candidate{Index: len(out), Text: text})
	}
	if len(out) == 0 {
		return nil, errors.New("decide: no non-empty candidates generated")
	}
	return out, nil
}

// judgeSystem casts the model as the decision adjudicator. The anti-default
// lens is explicit: a pick that would be identical regardless of the brief is
// the failure mode this whole phase exists to prevent.
const judgeSystem = `You are Cody prime's decision adjudicator. You are given a decision brief, the criteria a choice must satisfy, and N candidate options. Pick the SINGLE best-fitting candidate for THIS brief.

Reject the path of least resistance: if a candidate would be the obvious default regardless of the brief's specifics, prefer a better-fitting alternative. Judge fit against the brief and criteria, never novelty for its own sake.

Output ONLY a JSON object, no prose, no code fences:
{"pick": <0-based index of the chosen candidate>, "rationale": "one or two sentences: why this candidate and not the others"}`

// Judge asks the COLD client to pick the best candidate for the brief against
// the criteria. With zero candidates it errors; with one it short-circuits to
// that candidate (nothing to adjudicate). A judge response that cannot be
// parsed, or an out-of-range pick, fails SAFE to candidate 0 with an honest
// rationale — the decision phase must never wedge on an adjudicator hiccup (the
// proven Cassandra fail-open posture), while still having really run.
func Judge(ctx context.Context, cold *llm.Client, brief, criteria string, cands []Candidate) (Decision, error) {
	if len(cands) == 0 {
		return Decision{}, errors.New("decide: no candidates to judge")
	}
	if len(cands) == 1 {
		return Decision{Pick: 0, Rationale: "single candidate"}, nil
	}
	if cold == nil {
		return Decision{Pick: 0, Rationale: "no adjudicator available; defaulted to the first candidate"}, nil
	}
	var u strings.Builder
	if b := strings.TrimSpace(brief); b != "" {
		fmt.Fprintf(&u, "DECISION BRIEF:\n%s\n\n", b)
	}
	if c := strings.TrimSpace(criteria); c != "" {
		fmt.Fprintf(&u, "CRITERIA THE CHOICE MUST SATISFY:\n%s\n\n", c)
	}
	fmt.Fprintf(&u, "CANDIDATES (%d) — pick exactly one by index:\n", len(cands))
	for _, c := range cands {
		fmt.Fprintf(&u, "\n--- candidate %d ---\n%s\n", c.Index, c.Text)
	}
	res, err := cold.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{
		llm.SystemMessage(judgeSystem),
		llm.UserMessage(u.String()),
	}})
	if err != nil {
		return Decision{Pick: 0, Rationale: "adjudicator error; defaulted to the first candidate: " + err.Error()}, nil
	}
	dec, ok := parseDecision(res.Message.Content)
	if !ok || dec.Pick < 0 || dec.Pick >= len(cands) {
		return Decision{Pick: 0, Rationale: "adjudicator verdict unusable; defaulted to the first candidate"}, nil
	}
	return dec, nil
}

// parseDecision extracts the first balanced JSON object from the judge output
// and unmarshals the verdict.
func parseDecision(raw string) (Decision, bool) {
	obj := firstJSONObject(raw)
	if obj == "" {
		return Decision{}, false
	}
	var d Decision
	if err := json.Unmarshal([]byte(obj), &d); err != nil {
		return Decision{}, false
	}
	return d, true
}

// firstJSONObject returns the first balanced {...} span in s, string- and
// escape-aware, or "". (Mirrors the planner's tolerant extractor so a judge
// that wraps its JSON in prose or fences is still read.)
func firstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case inStr:
			if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
