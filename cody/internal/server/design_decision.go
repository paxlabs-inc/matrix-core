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
	"matrix/cody/internal/orchestrator"
	"matrix/cody/internal/workspace"
)

// The Design Language Record gate (req 9). A UI-bearing project authors a DLR
// before the first UI task: the visual language (style direction, typography,
// palette, spacing, motion, component idiom) generated hot and judged cold,
// screened deterministically for banned AI-tells and by the anti-default lens.
// In Engineer/Architect the DLR is a BLOCKING approve/override card on the
// answer channel (req 9.4); in Prototype it is an informational, non-blocking
// design-direction card. The resolved record binds every subsequent UI task
// sheet, and the gate rejects turn-ins that drift back to the banned defaults.
// The resolution is durable so a resumed run never re-asks.

// defaultDesignDoctrine is the compact fallback the DLR cites when
// rules/web/design-quality.md is not mounted. When RulesDir is configured,
// Engine.designDoctrine() reads the authored file and this const is the
// fail-open floor.
const defaultDesignDoctrine = `Anti-template policy — frontend must look intentional and specific, never a generic template. Banned: default card grids with no hierarchy; stock centered-headline + gradient-blob heroes; unmodified library defaults; flat no-depth layouts; uniform radius/spacing/shadow everywhere; safe gray-on-white with one decorative accent; dashboard-by-numbers with no point of view; default font stacks used without reason. Every surface should show clear hierarchy through scale contrast, intentional spacing rhythm, real depth/layering, deliberate typography pairing, semantic (not decorative) color, and designed hover/focus/active states. Pick a specific style direction (editorial, neo-brutalist, swiss, dark/light luxury, bento, retro-futurist, ...) — never "clean minimal" as a default. Do not auto-default to dark mode.`

// designDoctrine returns the design-quality doctrine the DLR cites: the authored
// rules/web/design-quality.md when RulesDir is mounted, else the compact
// defaultDesignDoctrine floor.
func (e *Engine) designDoctrine() string {
	if e.opts.RulesDir != "" {
		if data, err := os.ReadFile(filepath.Join(e.opts.RulesDir, "web", "design-quality.md")); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				return s
			}
		}
	}
	return defaultDesignDoctrine
}

// storedDesignDecision is the durable DLR resolution.
type storedDesignDecision struct {
	Record     *decide.DesignRecord `json:"record"`
	Decision   string               `json:"decision"` // "approve" | "override" | "informational"
	Constraint string               `json:"constraint"`
	At         time.Time            `json:"at"`
}

func (e *Engine) designDecisionPath(convID string) string {
	return filepath.Join(e.planDir(convID), "dlr.json")
}

// loadDesignDecision returns the durable DLR resolution, or (nil, false).
func (e *Engine) loadDesignDecision(convID string) (*storedDesignDecision, bool) {
	data, err := os.ReadFile(e.designDecisionPath(convID))
	if err != nil {
		return nil, false
	}
	var sd storedDesignDecision
	if json.Unmarshal(data, &sd) != nil {
		return nil, false
	}
	return &sd, true
}

func (e *Engine) saveDesignDecision(convID string, sd *storedDesignDecision) error {
	dir := e.planDir(convID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sd, "", "  ")
	if err != nil {
		return err
	}
	path := e.designDecisionPath(convID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// uiBearing reports whether this run should author a Design Language Record:
// the workspace already carries UI, or the authored plan has a UI task.
func uiBearing(plan *orchestrator.Plan, model *workspace.Model) bool {
	return model.UIBearing() || orchestrator.PlanHasUITask(plan)
}

// resolveDesignDecision authors, gates, and resolves the DLR for a UI-bearing
// run, returning the design-language constraint every UI sheet inherits and
// whether the run may proceed. It returns ("", false) only when the run was
// cancelled while paused (Engineer/Architect blocking card). Prototype never
// blocks. An already-resolved decision (durable) is adopted without re-asking.
func (e *Engine) resolveDesignDecision(ctx context.Context, r *run, pol mode.Policy, model *workspace.Model, plan *orchestrator.Plan, message string, hot, cold *llm.Client) (string, bool) {
	if sd, ok := e.loadDesignDecision(r.convID); ok {
		return sd.Constraint, true
	}

	brief := designBrief(message, model, plan)
	doctrine := e.designDoctrine()
	rec, err := decide.AuthorDesign(ctx, hot, cold, brief, doctrine, pol.DecisionCandidates)
	if err != nil {
		// Prototype never blocks the run on a design hiccup — it proceeds
		// without a DLR constraint (the UI sheets still carry the screenshot
		// requirement). Engineer/Architect surface it on the answer channel.
		if pol.Mode == mode.Prototype {
			return "", true
		}
		q := &question{Kind: "decision", Prompt: "I could not author a design language (" + err.Error() + "). Tell me the visual direction to use.", At: time.Now().UTC()}
		d, ok := e.awaitDirective(ctx, r, q, q.Prompt)
		if !ok {
			return "", false
		}
		return e.finalizeDesignDecision(r, nil, d, "override"), true
	}

	// Screens: the deterministic banned-defaults lens first (req 9.2), then the
	// anti-default adjudication lens. A rejected record is re-authored once with
	// the rejection as pressure before the human sees it.
	reject := decide.ScreenDesignBanned(rec)
	if reject == "" {
		reject = decide.ScreenDesignAntiDefault(ctx, cold, rec)
	}
	if reject != "" {
		if retry, rerr := decide.AuthorDesign(ctx, hot, cold, brief+"\n\nA prior attempt was rejected: "+reject+"\nAuthor a design that is specific to THIS product and free of the banned AI-default tells.", doctrine, pol.DecisionCandidates); rerr == nil {
			rec = retry
		}
	}

	e.publish(r, "decision.design", designFields(rec, pol.Register))

	// Prototype: informational, non-blocking (req 9.4). Persist and proceed.
	if pol.Mode == mode.Prototype {
		constraint := rec.Summary()
		_ = e.saveDesignDecision(r.convID, &storedDesignDecision{Record: rec, Decision: "informational", Constraint: constraint, At: time.Now().UTC()})
		e.publish(r, "decision.resolved", map[string]interface{}{"decision_kind": "design", "resolution": "informational"})
		return constraint, true
	}

	// Engineer/Architect: blocking approve/override on the answer channel.
	q := &question{Kind: "decision", Prompt: designPrompt(rec, pol.Register), At: time.Now().UTC()}
	d, ok := e.awaitDirective(ctx, r, q, q.Prompt)
	if !ok {
		return "", false
	}
	decisionKind := "approve"
	if d.Verdict != nil && d.Verdict.Decision != "" {
		decisionKind = d.Verdict.Decision
	}
	return e.finalizeDesignDecision(r, rec, d, decisionKind), true
}

// finalizeDesignDecision records the resolution durably, emits decision.resolved,
// restores the run to running, and returns the design-language constraint.
func (e *Engine) finalizeDesignDecision(r *run, rec *decide.DesignRecord, d directive, decisionKind string) string {
	constraint := designConstraint(rec, d)
	_ = e.saveDesignDecision(r.convID, &storedDesignDecision{Record: rec, Decision: decisionKind, Constraint: constraint, At: time.Now().UTC()})
	e.publish(r, "decision.resolved", map[string]interface{}{"decision_kind": "design", "resolution": decisionKind})

	r.setStatus("running")
	if led, err := e.readLedger(r.convID); err == nil {
		led.Status = "running"
		led.UpdatedAt = time.Now().UTC()
		_ = e.writeLedger(r.convID, led)
	}
	return constraint
}

// designBrief renders the DLR authoring brief: the request, what the workspace
// tells us, and the UI surfaces the plan will build.
func designBrief(message string, model *workspace.Model, plan *orchestrator.Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "UI-BEARING BUILD REQUEST:\n%s\n", strings.TrimSpace(message))
	if s := strings.TrimSpace(model.Summary()); s != "" {
		fmt.Fprintf(&b, "\nWORKSPACE:\n%s\n", s)
	}
	if plan != nil {
		var uiTitles []string
		for _, t := range plan.Tasks {
			if orchestrator.IsUITask(t) {
				uiTitles = append(uiTitles, t.Title)
			}
		}
		if len(uiTitles) > 0 {
			fmt.Fprintf(&b, "\nUI SURFACES THIS BUILD WILL PRODUCE:\n- %s\n", strings.Join(uiTitles, "\n- "))
		}
	}
	return b.String()
}

// designConstraint folds the resolved decision into the binding constraint the
// UI sheets carry. A user override wins over the authored record.
func designConstraint(rec *decide.DesignRecord, d directive) string {
	if d.Verdict != nil && d.Verdict.Decision == "override" {
		if len(d.Verdict.Payload) > 0 {
			return "User-directed design language (build EXACTLY to this):\n" + string(d.Verdict.Payload)
		}
		if strings.TrimSpace(d.Text) != "" {
			return "User-directed design language (build EXACTLY to this):\n" + strings.TrimSpace(d.Text)
		}
	}
	if strings.TrimSpace(d.Text) != "" && rec == nil {
		return "User-directed design language (build EXACTLY to this):\n" + strings.TrimSpace(d.Text)
	}
	if rec == nil {
		return ""
	}
	return rec.Summary()
}

// designPrompt renders the human-facing approval prompt in the mode's register.
func designPrompt(rec *decide.DesignRecord, register string) string {
	if register == mode.RegisterOutcome {
		return fmt.Sprintf("I'd like to give this a %s look. Approve to go ahead, or tell me a different direction.", strings.TrimSpace(rec.StyleDirection))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Design language for review — %s / %s.\n", rec.Profile.ProductKind, rec.Profile.Surface)
	b.WriteString(rec.Summary() + "\n")
	b.WriteString("Approve to build against it, or override with a different design language.")
	return b.String()
}

// designFields renders the DLR as decision.design event fields for the client.
// In the outcome register the card is plain-language (Prototype's change-the-look
// affordance); the technical register carries the full record.
func designFields(rec *decide.DesignRecord, register string) map[string]interface{} {
	f := map[string]interface{}{
		"style_direction": rec.StyleDirection,
		"typography":      rec.Typography,
		"palette":         append([]string{}, rec.Palette...),
		"spacing":         rec.Spacing,
		"motion":          rec.Motion,
		"component_idiom": rec.ComponentIdiom,
		"rationale":       rec.Rationale,
		"profile": map[string]interface{}{
			"product_kind":     rec.Profile.ProductKind,
			"audience":         rec.Profile.Audience,
			"brand_adjectives": rec.Profile.BrandAdjectives,
			"surface":          rec.Profile.Surface,
		},
	}
	if register == mode.RegisterOutcome {
		f["blocking"] = false
		f["summary"] = strings.TrimSpace(rec.StyleDirection)
	} else {
		f["blocking"] = true
	}
	return f
}
