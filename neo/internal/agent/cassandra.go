// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"fmt"
	"sort"
	"strings"

	"matrix/neo/internal/llm"
)

// cassandra.go — Cassandra 2.0, the silent-voice controller (Neo).
//
// The proof-of-work completion gate (task_complete adjudication, completion.go,
// the matrix/cassandra adjudicator wiring) is RETIRED. In its place Cassandra is
// a silent, first-person feedback controller: after an assistant turn is
// appended to a.working it may edit that message's OWN Content in place — folding
// in the epistemic primitives Doubt / Questioning / Curiosity / Assurance /
// Urge-to-verify — so the model reads the edit as its own emerging thought on the
// next turn (reasoning is not persisted across turns, so the assistant channel is
// the self). It is a two-sided damping controller (doubt damps over-confidence /
// looping; assurance damps thrashing / over-verification) and doubles as a
// self-healing loop killer. It is SILENT when healthy: at most one mod per step,
// loop/stall-gated, starting at step >= min_step, and a clean run is byte-
// identical to the controller disabled.
//
// Three hard guardrails: (1) content-only — it mutates ONLY the Content of an
// assistant-role message, never ToolCalls/Role/Name and never a tool/user/system
// message; (2) metacognition-only — additive first-person framing, never editing
// facts/numbers/addresses/code/tool arguments; (3) dual-record — every mod records
// {original_content, cassandra_mod, trigger} to an audit side-channel so ground
// truth is always recoverable. It never fabricates a completion and never reaches
// a user-facing surface (the 3 Non-Negotiables).

// auditEventMod is the single Cassandra 2.0 audit event: one silent modification
// of a prior assistant message. It rides the existing AuditObserver side-channel
// (engine.publishAudit -> broker) as a pure observability signal — it signs
// nothing, writes no cortex on the happy path, and is a no-op when no observer is
// wired (CLI / tests).
const auditEventMod = "cassandra.mod"

// AuditEvent is one Cassandra observation surfaced to the harness so an operator
// can audit the controller's silent behavior without the user ever seeing it.
// Optional; a nil AuditObserver discards them (CLI / tests).
type AuditEvent struct {
	Type   string
	Fields map[string]interface{}
}

// AuditObserver receives every Cassandra audit event as it happens. nil disables
// surfacing; the agent loop stays oblivious to the presentation layer (mirrors
// ToolObserver).
type AuditObserver func(AuditEvent)

func (a *Agent) emitAudit(typ string, fields map[string]interface{}) {
	if a.auditObserver == nil {
		return
	}
	a.auditObserver(AuditEvent{Type: typ, Fields: fields})
}

// ============================ Cassandra 2.0 controller ============================
//
// The controller reads behavioral signals the loop already computes (repeat
// count, cyclic A→B→A→B, semantic repeat, tool breadth, whether the turn is
// closing) plus two light local detectors (premature/unverified closure), maps
// the drift to a SIDE (doubt vs assurance) and an epistemic primitive, composes a
// parameterized first-person line, and folds it into the just-appended assistant
// message's Content in place. It corrects toward the productive middle: doubt /
// urge-to-verify damps over-confidence and looping; assurance / curiosity damps
// thrashing and over-verification. It never fabricates a completion.

// modTrigger is the behavioral drift that armed a modification.
type modTrigger string

const (
	trigLoop            modTrigger = "loop"             // repeat count reached the loop threshold
	trigCyclic          modTrigger = "cyclic"           // rotating A→B→A→B, no new tool
	trigSemanticRepeat  modTrigger = "semantic_repeat"  // same operation, reworded arguments
	trigPrematureClose  modTrigger = "premature_close"  // bare final answer while still mid-repeat
	trigUnverifiedClose modTrigger = "unverified_close" // closing an action task with zero tool evidence
	trigThrash          modTrigger = "thrash"           // cyclic verify-type calls, nothing new
	trigOverVerify      modTrigger = "over_verify"      // re-running the same check, unchanged result
	trigOscillation     modTrigger = "oscillation"      // cyclic mutate-type calls (flip-flopping)

	// Premise-provenance triggers (epistemic-core req.4.3): doubt keyed on the
	// ledger's state — claims judged at birth, not prose at death.
	trigRefutedPremise  modTrigger = "refuted_premise"  // acting past a refuted premise without revising
	trigUngroundedClose modTrigger = "ungrounded_close" // closing while the plan rests on unchecked self-assumptions
)

// modSide is the two-sided damping direction: doubt lowers unwarranted
// confidence / breaks loops; assurance stops thrashing / over-verification.
type modSide string

const (
	sideDoubt     modSide = "doubt"
	sideAssurance modSide = "assurance"
)

// cassandraMod is the dual-record for one silent modification (guardrail 3):
// {original_content, cassandra_mod, trigger, side, step, target}. It is the audit
// ground truth of what the agent actually said versus what Cassandra folded in.
type cassandraMod struct {
	Step     int
	Target   int // index into a.working (an assistant-role message)
	Original string
	Mod      string
	Trigger  modTrigger
	Side     modSide
}

// cassandraSignals is the per-step behavioral read the controller classifies. It
// is assembled from the Chat loop's existing state so the controller reads REAL
// behavior (task 2.3): it holds nothing the loop did not already compute.
type cassandraSignals struct {
	step             int
	closing          bool           // this turn ends on a bare answer (no tool calls)
	calls            []llm.ToolCall // this step's tool batch (empty on a bare-answer close)
	effectiveRepeats int            // no-progress repeat count INCLUDING this step's batch
	cyclic           bool           // rotating A→B→A→B cycle that introduced no new tool
	semanticRepeat   bool           // same operation as the previous batch, reworded
	workDone         bool           // at least one tool ran earlier this turn

	// Premise provenance (epistemic-core req.4.3): the controller judges
	// claims at birth, not prose at death. refutedPremises counts standing
	// refuted premises awaiting plan revision; ungroundedSelfPremises counts
	// unchecked self-referential assumptions the plan still rests on.
	refutedPremises        int
	ungroundedSelfPremises int
}

// buildCassandraSignals assembles the controller's signal bundle from an
// explicitly-supplied loop state — a thin projection over readStepSignals, the
// ONE place the repeat reads are computed (MORPHEUS req.5.1). The repeat read
// (repeats / recentSigs / prevSig / prevCalls) still reflects the PREVIOUS
// committed batch here — noteBatch commits the current batch later — so
// comparing the current batch against it yields the same repeat verdict
// noteBatch will, one step early. effectiveRepeats adds 1 when the current
// batch is itself a repeat, so the loop trigger can fire ONE step before the
// hard NoProgressStall.
func (a *Agent) buildCassandraSignals(step int, msg llm.Message, repeats int, recentSigs []string, prevSig string, prevCalls []llm.ToolCall, distinctToolSet map[string]struct{}, closing bool) cassandraSignals {
	return a.readStepSignals(step, msg.ToolCalls, repeats, recentSigs, prevSig, prevCalls, distinctToolSet, closing).cassandraView()
}

// cassandraStep is the per-step controller entry point. It is a no-op before
// min_step, when the per-turn mod budget is spent, when the per-trigger cooldown
// is active, or when no drift trigger fires (the healthy case → the window is
// byte-identical to the controller disabled). On a fire it edits the just-
// appended assistant message's Content in place, records the dual-record, and
// emits the cassandra.mod audit event. Returns whether it modified the window.
func (a *Agent) cassandraStep(sig cassandraSignals) bool {
	// Silent-when-healthy gate (req.4): no prior turn to edit before min_step, at
	// most max_mods_per_turn total.
	if sig.step < a.casMinStep() || a.turn.casModsThisTurn >= a.casMaxMods() {
		return false
	}
	trig, side := a.cassandraClassify(sig)
	if trig == "" {
		return false // healthy: no drift → zero modifications
	}
	// Per-trigger cooldown (req.4.4): never nag the same drift class every step.
	if a.turn.casCooldown == nil {
		a.turn.casCooldown = map[modTrigger]int{}
	}
	if last, ok := a.turn.casCooldown[trig]; ok && sig.step-last < a.casCooldownSteps() {
		return false
	}
	mod := cassandraTemplate(trig, sig)
	if strings.TrimSpace(mod) == "" {
		return false
	}
	if !a.cassandraEdit(len(a.working)-1, mod, trig, side, sig.step) {
		return false
	}
	a.turn.casCooldown[trig] = sig.step
	a.turn.casModsThisTurn++
	return true
}

// cassandraEdit folds a first-person metacognitive line into the Content of the
// assistant-role message at index target, IN PLACE. Guardrail 1 (content-only) is
// enforced structurally: it asserts RoleAssistant and mutates ONLY .Content —
// never ToolCalls / Role / Name, and never a tool / user / system message. The
// fold is additive (the original text is preserved and the mod is layered AFTER
// it as the agent's next thought), so guardrail 2 (metacognition-only, never a
// rewrite of facts) holds by construction. The dual-record + audit event
// (guardrail 3) are written BEFORE the mutation, and the durable cortex
// transcript is NOT rewritten (cmRecordAssistant already stored the original), so
// cortex stays ground truth (req.7.1). Returns false (no-op) if the target is out
// of range or is not an assistant message.
func (a *Agent) cassandraEdit(target int, mod string, trig modTrigger, side modSide, step int) bool {
	if target < 0 || target >= len(a.working) {
		return false
	}
	if a.working[target].Role != llm.RoleAssistant {
		return false // guardrail 1: only an assistant Content may be edited
	}
	original := a.working[target].Content
	// Dual-record BEFORE mutating (guardrail 3).
	a.turn.casRecord = append(a.turn.casRecord, cassandraMod{
		Step: step, Target: target, Original: original, Mod: mod, Trigger: trig, Side: side,
	})
	// Fold the mod AFTER the original content (original preserved). When the
	// assistant turn had no content (straight to tools), the mod becomes the
	// content — the agent's emerging thought alongside its tool calls.
	folded := mod
	if strings.TrimSpace(original) != "" {
		folded = original + "\n\n" + mod
	}
	a.working[target].Content = folded
	// Pure observability side-channel (task 4.1): no-op without an observer.
	a.emitAudit(auditEventMod, map[string]interface{}{
		"step":             step,
		"trigger":          string(trig),
		"side":             string(side),
		"target_index":     target,
		"original_content": original,
		"cassandra_mod":    mod,
	})
	return true
}

// cassandraClassify maps the drift to (trigger, side), selecting the side that
// pushes the run toward the productive middle (two-sided damping, never one-sided
// doubt-spam). A strong loop is always doubt (step back, try a different
// approach); milder repetition splits by tool KIND — repeated reads/checks are
// over-verification (assurance: commit and move on), repeated mutations that
// cycle are oscillation (assurance: stop flip-flopping), and anything else is
// doubt. On a closing turn the relevant drift is about finishing prematurely.
// Returns ("", "") when the run is healthy.
func (a *Agent) cassandraClassify(sig cassandraSignals) (modTrigger, modSide) {
	// Premise provenance outranks behavioral drift (req.4.3): acting past a
	// refuted premise is the sharpest doubt signal there is — the plan is
	// KNOWN wrong at a load-bearing point.
	if sig.refutedPremises > 0 {
		return trigRefutedPremise, sideDoubt
	}
	if sig.closing {
		// Closing while the plan still rests on unchecked self-referential
		// assumptions: the claim was never grounded, however finished the
		// prose sounds.
		if sig.ungroundedSelfPremises > 0 {
			return trigUngroundedClose, sideDoubt
		}
		// A close that bailed out of an active loop, or a close still carrying
		// repeat pressure, is doubt: did I actually finish, or just stop?
		if sig.effectiveRepeats >= a.casLoopThreshold() {
			return trigLoop, sideDoubt
		}
		if sig.workDone && sig.effectiveRepeats >= 1 {
			return trigPrematureClose, sideDoubt
		}
		if !sig.workDone && a.goalWantsAction() {
			return trigUnverifiedClose, sideDoubt
		}
		return "", ""
	}
	// A genuine loop (many repeats) always warrants doubt — this is the loop-kill
	// trigger that fires one step before the hard stall.
	if sig.effectiveRepeats >= a.casLoopThreshold() {
		return trigLoop, sideDoubt
	}
	// Milder repetition: split by tool kind (two-sided damping).
	if sig.semanticRepeat || sig.cyclic {
		switch batchKind(sig.calls) {
		case kindVerify:
			if sig.cyclic {
				return trigThrash, sideAssurance
			}
			return trigOverVerify, sideAssurance
		case kindMutate:
			if sig.cyclic {
				return trigOscillation, sideAssurance
			}
			return trigSemanticRepeat, sideDoubt
		default:
			if sig.cyclic {
				return trigCyclic, sideDoubt
			}
			return trigSemanticRepeat, sideDoubt
		}
	}
	return "", ""
}

// cassandraTemplate composes the first-person mod from a parameterized template
// that references the actual repeated operation where available (req.5.3), so it
// reads as a specific realization rather than boilerplate. It is metacognition
// only (req.3.2): epistemic framing about the agent's OWN work, never a claim of
// fact and never a fabricated completion (req.6.4).
func cassandraTemplate(trig modTrigger, sig cassandraSignals) string {
	op := casOpLabel(sig.calls)
	switch trig {
	case trigLoop:
		if op != "" {
			return fmt.Sprintf("Wait — this is about the %s time I've done %s and nothing has changed. Am I actually making progress, or just repeating myself? Let me stop, step back, and try a genuinely different approach.", ordinal(sig.effectiveRepeats+1), op)
		}
		return "Wait — I keep doing the same thing and nothing is changing. Am I actually making progress, or just repeating myself? Let me step back and try a genuinely different approach."
	case trigCyclic:
		return "I keep alternating between the same couple of moves — that's a loop, not progress. I need to break out and do something genuinely different."
	case trigSemanticRepeat:
		if op != "" {
			return fmt.Sprintf("I just reworded %s, but it's the same action — and rewording won't help. The approach itself is what's wrong; let me change it.", op)
		}
		return "I reworded the same action, but it's the same action — rewording won't help. The approach itself is what's wrong."
	case trigPrematureClose:
		return "Before I call this done — did I actually verify the load-bearing parts, or am I assuming? Let me check the one thing I haven't confirmed."
	case trigUnverifiedClose:
		return "I'm about to assert something I never actually checked. I should verify it with a tool, or say plainly that I didn't."
	case trigThrash:
		if op != "" {
			return fmt.Sprintf("I've already confirmed %s more than once — it's solid. I'm second-guessing a settled result; let me commit and move on to what's actually unresolved.", op)
		}
		return "I've already confirmed this more than once — it's solid. I'm second-guessing a settled result; let me commit and move on."
	case trigOverVerify:
		return "I already have this answer and it hasn't changed. Re-checking it again is avoidance — commit it and move on."
	case trigOscillation:
		return "I keep flip-flopping on this — both options are fine, and the flip-flopping itself is the real cost. Let me pick one, note why, and move on."
	case trigRefutedPremise:
		return "Hold on — a premise my plan rests on has been REFUTED (it's marked in my premise ledger). Anything I do that depends on it is built on a known falsehood. I need to revise the plan around the refuted premise before taking another dependent step."
	case trigUngroundedClose:
		return "Before I close: my plan still rests on an assumption about my OWN capabilities that I never checked against my capability surface. If that assumption is wrong, my answer is wrong. Let me verify it now — it's one lookup away."
	}
	return ""
}

// batchKindT classifies a tool batch for two-sided damping.
type batchKindT int

const (
	kindOther batchKindT = iota
	kindVerify
	kindMutate
)

// batchKind classifies a whole batch: mutating tools dominate a verify/other mix
// (a write is the salient action), else verify dominates other, else other.
func batchKind(calls []llm.ToolCall) batchKindT {
	verify, mutate, other := 0, 0, 0
	for _, c := range calls {
		switch toolKind(c.Function.Name) {
		case kindMutate:
			mutate++
		case kindVerify:
			verify++
		default:
			other++
		}
	}
	if mutate > 0 && mutate >= verify && mutate >= other {
		return kindMutate
	}
	if verify > 0 && verify >= other {
		return kindVerify
	}
	return kindOther
}

// toolKind classifies one tool by its (namespace-stripped) base name: state-
// mutating (write/edit/delete/deploy/send/...) vs read/verify (read/search/
// status/check/...) vs other.
func toolKind(name string) batchKindT {
	base := name
	if i := strings.LastIndex(base, "__"); i >= 0 {
		base = base[i+2:]
	}
	lb := strings.ToLower(base)
	for _, k := range []string{"write", "edit", "create", "append", "save", "patch", "delete", "remove", "deploy", "send", "transfer", "swap"} {
		if strings.Contains(lb, k) {
			return kindMutate
		}
	}
	for _, k := range []string{"read", "search", "fetch", "get", "list", "glob", "find", "grep", "status", "check", "test", "info", "view", "cat", "inspect"} {
		if strings.Contains(lb, k) {
			return kindVerify
		}
	}
	return kindOther
}

// casOpLabel renders a short humanized label for the repeated operation, drawn
// from the batch's distinct tool names, for parameterizing a template. "" when
// there is no batch (a bare-answer close).
func casOpLabel(calls []llm.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	names := map[string]struct{}{}
	for _, c := range calls {
		b := c.Function.Name
		if i := strings.LastIndex(b, "__"); i >= 0 {
			b = b[i+2:]
		}
		names[strings.ReplaceAll(b, "_", " ")] = struct{}{}
	}
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	if len(list) == 1 {
		return "the same " + list[0]
	}
	return "the same set of steps"
}

// goalWantsAction reports whether the active goal reads like a task that requires
// tool work (deploy/send/build/search/…), used to gate unverified_close so it
// never fires on a pure-conversation turn (a greeting or an explanation is a valid
// bare answer with no tool evidence).
func (a *Agent) goalWantsAction() bool {
	g := strings.ToLower(a.activeGoal)
	for _, v := range []string{"deploy", "send ", "transfer", "swap", "build", "create", "generate", "fetch", "download", "search", "browse", "run ", "execute", "install", "make me", "render"} {
		if strings.Contains(g, v) {
			return true
		}
	}
	return false
}

// ordinal renders 1→"1st", 2→"2nd", 3→"3rd", 4→"4th", … for a readable count.
func ordinal(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return fmt.Sprintf("%dth", n)
	}
	switch n % 10 {
	case 1:
		return fmt.Sprintf("%dst", n)
	case 2:
		return fmt.Sprintf("%dnd", n)
	case 3:
		return fmt.Sprintf("%drd", n)
	default:
		return fmt.Sprintf("%dth", n)
	}
}

// --- config accessors (resolve defaults when a field is unset) ---

func (a *Agent) casMinStep() int {
	if a.cfg.CassandraMinStep > 0 {
		return a.cfg.CassandraMinStep
	}
	return 2
}

func (a *Agent) casMaxMods() int {
	if a.cfg.CassandraMaxModsPerTurn > 0 {
		return a.cfg.CassandraMaxModsPerTurn
	}
	return 3
}

func (a *Agent) casCooldownSteps() int {
	if a.cfg.CassandraCooldownSteps > 0 {
		return a.cfg.CassandraCooldownSteps
	}
	return 2
}

// casLoopThreshold resolves the repeat count that arms the doubt side. A positive
// config value wins; 0 (the default) derives NoProgressStall-1 so it fires one
// step before the hard stall and tracks the stall bound when unset.
func (a *Agent) casLoopThreshold() int {
	if a.cfg.CassandraLoopThreshold > 0 {
		return a.cfg.CassandraLoopThreshold
	}
	t := a.cfg.NoProgressStall - 1
	if t < 1 {
		t = 1
	}
	return t
}
