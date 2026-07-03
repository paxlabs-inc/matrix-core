// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"matrix/cody/internal/llm"
)

// StackProfile is the requirements profile the stack is chosen FOR — the facts
// that make one stack fit better than another. A pick that ignores this profile
// is exactly the default the anti-default lens rejects.
type StackProfile struct {
	AppClass         string `json:"app_class"`
	Audience         string `json:"audience"`
	DeploymentTarget string `json:"deployment_target"`
	TeamContext      string `json:"team_context"`
}

// StackCandidate is one genuinely divergent option considered in the record.
type StackCandidate struct {
	Name      string `json:"name"`
	Rationale string `json:"rationale"`
	// Deviates flags a pick that departs from rules/stack-selection doctrine;
	// when true DeviationRationale must state why (req 8.5).
	Deviates           bool   `json:"deviates_from_doctrine,omitempty"`
	DeviationRationale string `json:"deviation_rationale,omitempty"`
}

// StackRecord is the durable Stack Decision Record: the requirements profile,
// 2-3 divergent candidate stacks with per-candidate rationale citing doctrine,
// and the pick. Authored HOT (N divergent records generated, judged COLD) and
// then screened by the anti-default lens before the human resolves it.
type StackRecord struct {
	Profile    StackProfile     `json:"profile"`
	Candidates []StackCandidate `json:"candidates"`
	Pick       int              `json:"pick"`
	Rationale  string           `json:"rationale"`
}

// Chosen returns the picked candidate (clamped into range), or a zero value if
// the record carries no candidates.
func (r *StackRecord) Chosen() StackCandidate {
	if len(r.Candidates) == 0 {
		return StackCandidate{}
	}
	i := r.Pick
	if i < 0 || i >= len(r.Candidates) {
		i = 0
	}
	return r.Candidates[i]
}

// Validate rejects a record that could not have been a real decision: fewer
// than two divergent candidates is not a choice, and a deviation without stated
// rationale violates doctrine (req 8.5).
func (r *StackRecord) Validate() error {
	if len(r.Candidates) < 2 {
		return fmt.Errorf("stack record needs >= 2 divergent candidates, got %d", len(r.Candidates))
	}
	for i, c := range r.Candidates {
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("candidate %d has no stack name", i)
		}
		if c.Deviates && strings.TrimSpace(c.DeviationRationale) == "" {
			return fmt.Errorf("candidate %d deviates from doctrine but states no rationale", i)
		}
	}
	if r.Pick < 0 || r.Pick >= len(r.Candidates) {
		r.Pick = 0
	}
	return nil
}

// stackSystem casts the model as the stack decision author. The doctrine is
// injected so the record can cite it; the anti-default posture is stated up
// front so the generation itself resists the path of least resistance.
func stackSystem(doctrine string) string {
	var b strings.Builder
	b.WriteString(`You are Cody prime's stack architect authoring a Stack Decision Record for a GREENFIELD build. Choose the stack a senior engineer would for THESE requirements — never the path of least resistance.

Derive the requirements profile from the request, propose 2-3 GENUINELY DIVERGENT candidate stacks (not three flavors of the same default), give each a fit rationale citing the doctrine below, and pick the best-fitting one. A deviation from the doctrine is allowed but MUST state its rationale.

Output ONLY a JSON object, no prose, no code fences:
{
  "profile": {"app_class": "...", "audience": "...", "deployment_target": "...", "team_context": "..."},
  "candidates": [
    {"name": "concrete stack (language + framework + tooling)", "rationale": "why it fits THIS profile, citing doctrine", "deviates_from_doctrine": false, "deviation_rationale": ""}
  ],
  "pick": 0,
  "rationale": "one or two sentences: why this candidate for THIS profile and not the others"
}

`)
	b.WriteString("DOCTRINE (rules/stack-selection — cite it, deviations demand rationale):\n")
	b.WriteString(strings.TrimSpace(doctrine))
	return b.String()
}

// stackCriteria is what the cold judge weighs candidate RECORDS against.
const stackCriteria = "The record whose pick best fits its own requirements profile (app class, audience, deployment target, team context) as a senior engineer would choose; reject a record whose pick is the generic default regardless of the profile, or whose candidates are not genuinely divergent."

// AuthorStack authors a Stack Decision Record the decision way: generate
// `candidates` divergent records HOT, keep the ones that parse and validate,
// and pick the best-fitting one COLD (req 10.2). hot is the decision-temperature
// client; cold is the adjudication client. Returns the chosen record.
func AuthorStack(ctx context.Context, hot, cold *llm.Client, brief, doctrine string, candidates int) (*StackRecord, error) {
	if hot == nil {
		return nil, errors.New("decide: nil decision client")
	}
	if candidates < 1 {
		candidates = 1
	}
	system := stackSystem(doctrine)
	cands, err := Generate(ctx, hot, system, brief, candidates)
	if err != nil {
		return nil, fmt.Errorf("decide: author stack: %w", err)
	}
	var valid []*StackRecord
	var judgeable []Candidate
	for _, c := range cands {
		rec, err := parseStack(c.Text)
		if err != nil {
			continue
		}
		if err := rec.Validate(); err != nil {
			continue
		}
		judgeable = append(judgeable, Candidate{Index: len(valid), Text: c.Text})
		valid = append(valid, rec)
	}
	if len(valid) == 0 {
		return nil, fmt.Errorf("decide: model produced no valid stack record across %d candidate(s)", len(cands))
	}
	if len(valid) == 1 {
		return valid[0], nil
	}
	dec, err := Judge(ctx, cold, "Greenfield build brief:\n"+brief, stackCriteria, judgeable)
	if err != nil {
		return valid[0], nil // fail-safe to the first valid record
	}
	pick := dec.Pick
	if pick < 0 || pick >= len(valid) {
		pick = 0
	}
	return valid[pick], nil
}

// antiDefaultSystem casts the model as the anti-default adjudicator: the gate
// lens whose entire job is to reject a stack choice that would be identical
// regardless of the requirements (req 8.2).
const antiDefaultSystem = `You are Cody prime's anti-default stack adjudicator. You are given a Stack Decision Record: a requirements profile and the chosen stack with its rationale. Your ONE job is to reject a choice that would be the same regardless of the requirements.

Reject (accept=false) when: the pick is the generic default (e.g. React/Next + npm for anything, Express for every backend) and its rationale does not turn on the profile's specifics; or the profile is filled with boilerplate that could describe any project; or the candidates were not genuinely divergent. Accept (accept=true) only when the pick is defensibly the best FIT for THIS profile.

Output ONLY a JSON object, no prose, no code fences:
{"accept": <true|false>, "reason": "one sentence: why this pick does or does not turn on the requirements"}`

// antiDefaultVerdict is the gate lens result.
type antiDefaultVerdict struct {
	Accept bool   `json:"accept"`
	Reason string `json:"reason"`
}

// ScreenStackAntiDefault runs the anti-default lens over a chosen record. It
// returns "" when the record passes, or a concrete rejection reason. A judge
// transport error fails OPEN (empty) — consistent with the proven Cassandra
// posture: the human still resolves the record, so a hiccup never wedges the
// phase, but an explicit reject verdict is honored.
func ScreenStackAntiDefault(ctx context.Context, cold *llm.Client, rec *StackRecord) string {
	if cold == nil || rec == nil {
		return ""
	}
	chosen := rec.Chosen()
	var u strings.Builder
	fmt.Fprintf(&u, "REQUIREMENTS PROFILE:\napp class: %s\naudience: %s\ndeployment target: %s\nteam context: %s\n\n",
		rec.Profile.AppClass, rec.Profile.Audience, rec.Profile.DeploymentTarget, rec.Profile.TeamContext)
	fmt.Fprintf(&u, "CHOSEN STACK: %s\nPICK RATIONALE: %s\n\nCANDIDATES CONSIDERED:\n", chosen.Name, strings.TrimSpace(rec.Rationale+" "+chosen.Rationale))
	for i, c := range rec.Candidates {
		fmt.Fprintf(&u, "%d. %s — %s\n", i, c.Name, c.Rationale)
	}
	res, err := cold.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{
		llm.SystemMessage(antiDefaultSystem),
		llm.UserMessage(u.String()),
	}})
	if err != nil {
		return "" // fail open
	}
	obj := firstJSONObject(res.Message.Content)
	if obj == "" {
		return "" // unparseable verdict fails open
	}
	var v antiDefaultVerdict
	if json.Unmarshal([]byte(obj), &v) != nil {
		return ""
	}
	if v.Accept {
		return ""
	}
	reason := strings.TrimSpace(v.Reason)
	if reason == "" {
		reason = "the stack choice does not turn on the requirements profile"
	}
	return "anti-default lens rejected the stack decision: " + reason
}

// parseStack extracts the first balanced JSON object and unmarshals a record.
func parseStack(raw string) (*StackRecord, error) {
	obj := firstJSONObject(raw)
	if obj == "" {
		return nil, errors.New("decide: no JSON object in stack output")
	}
	var rec StackRecord
	if err := json.Unmarshal([]byte(obj), &rec); err != nil {
		return nil, fmt.Errorf("decide: parse stack record: %w", err)
	}
	return &rec, nil
}
