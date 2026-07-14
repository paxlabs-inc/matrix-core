// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// premise.go — Mechanism 1 of the epistemic core (epistemic-core req.4): the
// premise ledger. When a run's plan forms, its load-bearing factual premises
// are extracted into run state, each carrying provenance — cited (self-model |
// cortex | tool-evidence | user, with the citation) or assumption — so
// assumption is visible state instead of invisible confidence. The ledger
// renders resident in the fixed tail slot (premiseTail) and transitions
// (discharge to cited, refute) re-render the step they occur.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"matrix/neo/internal/llm"
)

type premiseStatus string

const (
	premiseAssumption premiseStatus = "assumption"
	premiseCited      premiseStatus = "cited"
	premiseRefuted    premiseStatus = "refuted"
)

// Premise is one load-bearing factual premise of the current plan.
type Premise struct {
	ID        int
	Statement string
	Status    premiseStatus
	Source    string // self-model | cortex | tool-evidence | user (when cited)
	Citation  string // the citation backing a cited premise
	SelfRef   bool   // a claim about the agent's own capabilities/surfaces
	Revised   bool   // a refuted premise whose forced plan revision has run
	// Hypothesis marks a probe-strategy expectation (Mechanism 3): its
	// refutation is answered by the per-strategy mismatch meter and its own
	// revision bound (req.6.3), not the plan-premise refusal path (req.5.2) —
	// one missed probe is information, not a known-false plan.
	Hypothesis bool
}

// premiseLedger is the run-state ledger. It is plain agent-core state (no new
// cortex memory types — design non-goal); durable learnings still flow through
// the existing writeback paths.
type premiseLedger struct {
	items  []*Premise
	nextID int
}

func (l *premiseLedger) add(p Premise) *Premise {
	l.nextID++
	p.ID = l.nextID
	item := &p
	l.items = append(l.items, item)
	return item
}

// ungroundedSelf returns standing self-referential assumptions — the arena
// failure class: acting on an ungrounded premise about one's own architecture.
func (l *premiseLedger) ungroundedSelf() []*Premise {
	if l == nil {
		return nil
	}
	var out []*Premise
	for _, p := range l.items {
		if p.Status == premiseAssumption && p.SelfRef {
			out = append(out, p)
		}
	}
	return out
}

// assumptions returns all standing (unchecked) assumptions.
func (l *premiseLedger) assumptions() []*Premise {
	if l == nil {
		return nil
	}
	var out []*Premise
	for _, p := range l.items {
		if p.Status == premiseAssumption {
			out = append(out, p)
		}
	}
	return out
}

// unrevisedRefuted returns refuted PLAN premises whose forced revision has not
// happened yet (probe hypotheses are excluded: their refutation is metered,
// not refusal-gated).
func (l *premiseLedger) unrevisedRefuted() []*Premise {
	if l == nil {
		return nil
	}
	var out []*Premise
	for _, p := range l.items {
		if p.Status == premiseRefuted && !p.Revised && !p.Hypothesis {
			out = append(out, p)
		}
	}
	return out
}

// selfClaimPattern detects self-referential capability claims deterministically
// (identity-scrub posture: high-precision patterns, fail toward checking).
// These are the claims that MUST resolve from the resident self-model, never
// from generation (req.5.3 / Mechanism 2).
var selfClaimPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bmy own (api|endpoint|server|interface|route)`),
	regexp.MustCompile(`(?i)\bI (?:have|expose|serve|provide|offer) (?:an?|the) [^.\n]*\b(api|endpoint|interface)\b`),
	regexp.MustCompile(`(?i)\bmy (openai-compatible|rest|http|websocket|grpc) (api|endpoint|interface)`),
	regexp.MustCompile(`(?i)\bI can (?:wire|hook|connect|expose|integrate)[^.\n]*\b(?:directly )?through my own\b`),
	regexp.MustCompile(`(?i)\bI (?:support|speak|implement) the [^.\n]*\b(protocol|api|spec)\b`),
}

// detectSelfClaims returns the sentences of text that carry a deterministic
// self-referential capability claim.
func detectSelfClaims(text string) []string {
	var out []string
	for _, sentence := range splitSentences(text) {
		for _, re := range selfClaimPatterns {
			if re.MatchString(sentence) {
				out = append(out, strings.TrimSpace(sentence))
				break
			}
		}
	}
	return out
}

func splitSentences(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		for _, s := range strings.SplitAfter(line, ". ") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// premiseExtractionSystem is the cheap-model half of the hybrid extractor.
const premiseExtractionSystem = `You extract the load-bearing FACTUAL PREMISES a plan relies on — the statements that, if false, make the plan wrong. Not goals, not steps: premises.

Return ONLY a JSON array (no prose, no fences):
[{"statement":"...","basis":"user|tool-evidence|memory|self|assumption","citation":"..."}]

basis rules — be strict, default to "assumption":
- "user": the user stated it in this conversation (citation = their words)
- "tool-evidence": a tool result already established it (citation = the result)
- "memory": recalled durable memory supports it (citation = the memory)
- "self": a claim about the agent's OWN capabilities/architecture/surfaces
- "assumption": everything else — anything not directly supported

Usually 1-4 premises. Return [] when the plan rests on no factual premise.`

// epistemicPremisesOn reports whether Mechanism 1 is active.
func (a *Agent) epistemicPremisesOn() bool {
	return a.cfg.EpistemicPremises
}

// premiseObservePlan runs at the plan-formation seam: the first committing
// assistant turn of a run (and again on plan change via premisePlanChanged).
// It extracts the plan's premises (deterministic self-claim detector + cheap-
// model extraction), seeds the ledger, and renders it resident. Best-effort on
// the model half: extraction failure degrades to the deterministic premises,
// never blocks the turn.
func (a *Agent) premiseObservePlan(ctx context.Context, planText string) {
	if !a.epistemicPremisesOn() || a.turn.ledger != nil {
		return
	}
	a.turn.ledger = &premiseLedger{}
	seen := map[string]struct{}{}

	// Deterministic layer: self-referential capability claims are premises by
	// construction, and always start as assumptions (they must resolve from
	// the resident self-model, never from plausibility).
	for _, claim := range detectSelfClaims(planText) {
		key := strings.ToLower(claim)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		a.turn.ledger.add(Premise{Statement: claim, Status: premiseAssumption, SelfRef: true})
	}

	// Model layer: cheap extraction of the remaining load-bearing premises.
	for _, ext := range a.extractPremisesModel(ctx, planText) {
		key := strings.ToLower(strings.TrimSpace(ext.Statement))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		a.turn.ledger.add(ext)
	}
	a.renderPremiseTail()
}

// premisePlanChanged resets the ledger for a revised plan (refute → revision
// path, req.4.1 "and on plan change"): standing refuted premises are marked
// revised, and the next committing turn re-extracts.
func (a *Agent) premisePlanChanged() {
	if a.turn.ledger == nil {
		return
	}
	for _, p := range a.turn.ledger.unrevisedRefuted() {
		p.Revised = true
	}
	a.turn.ledger = nil
	a.turn.premiseTail = ""
}

// extractPremisesModel is the cheap-model extraction pass. Returns nil on any
// transport/parse failure (honest degradation to the deterministic layer).
func (a *Agent) extractPremisesModel(ctx context.Context, planText string) []Premise {
	client := a.cheap
	if client == nil {
		client = a.main
	}
	if client == nil || strings.TrimSpace(planText) == "" {
		return nil
	}
	res, err := client.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			llm.SystemMessage(premiseExtractionSystem),
			llm.UserMessage("Plan:\n" + planText),
		},
	})
	if err != nil || res == nil {
		return nil
	}
	raw := strings.TrimSpace(res.Message.Content)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	var rows []struct {
		Statement string `json:"statement"`
		Basis     string `json:"basis"`
		Citation  string `json:"citation"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &rows); err != nil {
		return nil
	}
	var out []Premise
	for _, r := range rows {
		p := Premise{Statement: strings.TrimSpace(r.Statement), Citation: strings.TrimSpace(r.Citation)}
		if p.Statement == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(r.Basis)) {
		case "user":
			p.Status, p.Source = premiseCited, "user"
		case "tool-evidence":
			p.Status, p.Source = premiseCited, "tool-evidence"
		case "memory":
			p.Status, p.Source = premiseCited, "cortex"
		case "self":
			// A self-claim never rides the extractor's word: it starts as an
			// assumption and must discharge against the resident self-model
			// (fail toward checking — Mechanism 2).
			p.Status, p.SelfRef = premiseAssumption, true
		default:
			p.Status = premiseAssumption
		}
		out = append(out, p)
	}
	return out
}

// premiseDischarge marks a premise cited (the check succeeded) and re-renders
// the resident ledger the step it occurs.
func (a *Agent) premiseDischarge(p *Premise, source, citation string) {
	if p == nil {
		return
	}
	p.Status = premiseCited
	p.Source = source
	p.Citation = citation
	a.renderPremiseTail()
}

// premiseRefute marks a premise refuted (the check contradicted it) and
// re-renders. A refuted premise forces a plan-revision step before further
// dependent dispatches (enforced at the gate, task 3.1).
func (a *Agent) premiseRefute(p *Premise, citation string) {
	if p == nil {
		return
	}
	p.Status = premiseRefuted
	if citation != "" {
		p.Citation = citation
	}
	a.renderPremiseTail()
}

// renderPremiseTail renders the ledger into its fixed tail slot (req.4.2).
// Empty ledger renders nothing (a clean turn is byte-identical to disabled).
func (a *Agent) renderPremiseTail() {
	if a.turn.ledger == nil || len(a.turn.ledger.items) == 0 {
		a.turn.premiseTail = ""
		return
	}
	var b strings.Builder
	b.WriteString("\nPremise ledger (what your current plan rests on — never act on an unchecked ASSUMPTION that one lookup could verify):\n")
	for _, p := range a.turn.ledger.items {
		switch p.Status {
		case premiseCited:
			fmt.Fprintf(&b, "- P%d [cited: %s — %s] %s\n", p.ID, p.Source, citationLine(p.Citation), p.Statement)
		case premiseRefuted:
			fmt.Fprintf(&b, "- P%d [REFUTED — %s] %s (revise the plan before acting on anything that depended on this)\n", p.ID, citationLine(p.Citation), p.Statement)
		default:
			label := "ASSUMPTION — unverified"
			if p.SelfRef {
				label = "ASSUMPTION about yourself — check it against your capability surface before acting on it"
			}
			fmt.Fprintf(&b, "- P%d [%s] %s\n", p.ID, label, p.Statement)
		}
	}
	a.turn.premiseTail = b.String()
}

func citationLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if s == "" {
		return "no citation"
	}
	return truncate(s, 160)
}
