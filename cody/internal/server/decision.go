// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"matrix/cody/internal/decide"
	"matrix/cody/internal/llm"
	"matrix/cody/internal/mode"
	"matrix/cody/internal/workspace"
)

// The Stack Decision Record gate (req 8). On a GREENFIELD Engineer/Architect
// run, codyd authors an SDR before any plan exists — 2-3 divergent candidate
// stacks generated hot and judged cold, screened by the anti-default lens — and
// pauses on the answer channel until the human approves or overrides it. Because
// this runs BEFORE plan authoring, wave 1 is structurally unreachable until the
// SDR resolves: there is no plan, hence no wave, until the human resolves the
// decision (req 8.3). Prototype skips the SDR entirely (req 8.4). The resolution
// is durable so a resumed run never re-asks.

// defaultStackDoctrine is the compact fallback the SDR cites when the full
// rules/stack-selection decision tables are not mounted (req 8.5, 16.1). When
// RulesDir is configured, Engine.stackDoctrine() reads the authored tables under
// rules/stack-selection/ and this const is the fail-open floor.
const defaultStackDoctrine = `App class -> default stack (deviation demands rationale):
- chat / realtime / collaborative -> React Router framework mode (Remix) on Node/TS
- content / marketing / mostly-static -> Astro (or Next) on Node/TS
- internal enterprise / forms-heavy admin -> Angular on Node/TS
- high-throughput backend / services -> Go
- correctness-critical / systems -> Rust
- ML-adjacent / data / scientific -> Python with uv
- classic web app with no strong signal -> Node/TS + Vite or Next + npm
The stack must fit the requirements profile; the same pick for every profile is the default this gate exists to reject.`

// stackDoctrine returns the stack-selection doctrine the SDR cites: the authored
// decision tables under {RulesDir}/stack-selection/ when mounted (req 16.1),
// else the compact defaultStackDoctrine floor.
func (e *Engine) stackDoctrine() string {
	if e.opts.RulesDir != "" {
		if doc := concatMarkdown(filepath.Join(e.opts.RulesDir, "stack-selection")); doc != "" {
			return doc
		}
	}
	return defaultStackDoctrine
}

// concatMarkdown reads every *.md under dir in filename order and concatenates
// them, returning "" if the directory is unreadable or holds no markdown.
func concatMarkdown(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.Write(data)
	}
	return strings.TrimSpace(b.String())
}

// storedStackDecision is the durable SDR resolution, written once the human
// resolves it so a resumed run adopts the same decision without re-asking.
type storedStackDecision struct {
	Record   *decide.StackRecord `json:"record"`
	Decision string              `json:"decision"` // "approve" | "override"
	Addendum string              `json:"addendum"`
	At       time.Time           `json:"at"`
}

func (e *Engine) stackDecisionPath(convID string) string {
	return filepath.Join(e.planDir(convID), "sdr.json")
}

// loadStackDecision returns the durable SDR resolution, or (nil, false).
func (e *Engine) loadStackDecision(convID string) (*storedStackDecision, bool) {
	data, err := os.ReadFile(e.stackDecisionPath(convID))
	if err != nil {
		return nil, false
	}
	var sd storedStackDecision
	if json.Unmarshal(data, &sd) != nil {
		return nil, false
	}
	return &sd, true
}

func (e *Engine) saveStackDecision(convID string, sd *storedStackDecision) error {
	dir := e.planDir(convID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sd, "", "  ")
	if err != nil {
		return err
	}
	path := e.stackDecisionPath(convID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// resolveStackDecision authors, gates, and resolves the SDR for a greenfield
// Engineer/Architect run, returning the planning addendum (the approved stack,
// folded into the planner's request) and whether the run may proceed. It returns
// ("", false) only when the run was cancelled while paused. An already-resolved
// decision (durable) is adopted without re-asking.
func (e *Engine) resolveStackDecision(ctx context.Context, r *run, pol mode.Policy, model *workspace.Model, message string, hot, cold *llm.Client) (string, bool) {
	if sd, ok := e.loadStackDecision(r.convID); ok {
		return sd.Addendum, true
	}

	brief := stackBrief(message, model)
	doctrine := e.stackDoctrine()
	rec, err := decide.AuthorStack(ctx, hot, cold, brief, doctrine, pol.DecisionCandidates)
	if err != nil {
		// Authoring failed outright: surface honestly on the answer channel so the
		// human can steer rather than silently defaulting the stack.
		q := &question{Kind: "decision", Prompt: "I could not author a stack decision (" + err.Error() + "). Tell me which stack to use.", At: time.Now().UTC()}
		d, ok := e.awaitDirective(ctx, r, q, q.Prompt)
		if !ok {
			return "", false
		}
		return e.finalizeStackDecision(r, nil, d), true
	}

	// The anti-default lens can reject; a rejected record is re-authored once
	// with the rejection as pressure before the human sees it.
	if verdict := decide.ScreenStackAntiDefault(ctx, cold, rec); verdict != "" {
		if retry, rerr := decide.AuthorStack(ctx, hot, cold, brief+"\n\nA prior attempt was rejected: "+verdict+"\nAuthor a record whose pick genuinely turns on the requirements profile.", doctrine, pol.DecisionCandidates); rerr == nil {
			rec = retry
		}
	}

	e.publish(r, "decision.stack", stackFields(rec))

	q := &question{Kind: "decision", Prompt: stackPrompt(rec, pol.Register), At: time.Now().UTC()}
	d, ok := e.awaitDirective(ctx, r, q, q.Prompt)
	if !ok {
		return "", false
	}
	return e.finalizeStackDecision(r, rec, d), true
}

// finalizeStackDecision records the resolution durably, emits decision.resolved,
// restores the run to running, and returns the planning addendum.
func (e *Engine) finalizeStackDecision(r *run, rec *decide.StackRecord, d directive) string {
	decisionKind := "approve"
	if d.Verdict != nil && d.Verdict.Decision != "" {
		decisionKind = d.Verdict.Decision
	}
	addendum := stackAddendum(rec, d)
	_ = e.saveStackDecision(r.convID, &storedStackDecision{Record: rec, Decision: decisionKind, Addendum: addendum, At: time.Now().UTC()})

	e.publish(r, "decision.resolved", map[string]interface{}{"decision_kind": "stack", "resolution": decisionKind})

	r.setStatus("running")
	if led, err := e.readLedger(r.convID); err == nil {
		led.Status = "running"
		led.UpdatedAt = time.Now().UTC()
		_ = e.writeLedger(r.convID, led)
	}
	return addendum
}

// stackBrief renders the SDR authoring brief: the request plus what the empty
// workspace already tells us.
func stackBrief(message string, model *workspace.Model) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GREENFIELD BUILD REQUEST:\n%s\n", strings.TrimSpace(message))
	if s := strings.TrimSpace(model.Summary()); s != "" {
		fmt.Fprintf(&b, "\nWORKSPACE (currently greenfield):\n%s\n", s)
	}
	return b.String()
}

// stackAddendum folds the resolved decision into the planner's request so the
// plan is authored against the chosen (or overridden) stack.
func stackAddendum(rec *decide.StackRecord, d directive) string {
	// A user override wins over the authored pick.
	if d.Verdict != nil && d.Verdict.Decision == "override" {
		if len(d.Verdict.Payload) > 0 {
			return "\n\nSTACK DECISION (user override — build with EXACTLY this stack):\n" + string(d.Verdict.Payload)
		}
		if strings.TrimSpace(d.Text) != "" {
			return "\n\nSTACK DECISION (user override — build with EXACTLY this stack):\n" + strings.TrimSpace(d.Text)
		}
	}
	if strings.TrimSpace(d.Text) != "" && rec == nil {
		return "\n\nSTACK DECISION (user-specified — build with EXACTLY this stack):\n" + strings.TrimSpace(d.Text)
	}
	if rec == nil {
		return ""
	}
	chosen := rec.Chosen()
	return fmt.Sprintf("\n\nSTACK DECISION (approved — build with EXACTLY this stack, do not default away from it):\n%s\nWhy: %s", chosen.Name, strings.TrimSpace(rec.Rationale))
}

// stackPrompt renders the human-facing approval prompt in the mode's register.
func stackPrompt(rec *decide.StackRecord, register string) string {
	chosen := rec.Chosen()
	if register == mode.RegisterOutcome {
		return fmt.Sprintf("I'd like to build this with %s. Approve to go ahead, or tell me a different stack to use.", chosen.Name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Stack decision for review — profile: %s / %s / %s / %s.\n",
		rec.Profile.AppClass, rec.Profile.Audience, rec.Profile.DeploymentTarget, rec.Profile.TeamContext)
	fmt.Fprintf(&b, "Recommended: %s — %s\n", chosen.Name, strings.TrimSpace(rec.Rationale))
	b.WriteString("Approve to plan against it, or override with a different stack.")
	return b.String()
}

// stackFields renders the SDR as decision.stack event fields for the client.
func stackFields(rec *decide.StackRecord) map[string]interface{} {
	cands := make([]map[string]interface{}, 0, len(rec.Candidates))
	for _, c := range rec.Candidates {
		cands = append(cands, map[string]interface{}{
			"name": c.Name, "rationale": c.Rationale,
			"deviates": c.Deviates, "deviation_rationale": c.DeviationRationale,
		})
	}
	return map[string]interface{}{
		"profile": map[string]interface{}{
			"app_class": rec.Profile.AppClass, "audience": rec.Profile.Audience,
			"deployment_target": rec.Profile.DeploymentTarget, "team_context": rec.Profile.TeamContext,
		},
		"candidates": cands,
		"pick":       rec.Pick,
		"rationale":  rec.Rationale,
	}
}
