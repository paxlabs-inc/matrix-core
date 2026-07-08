// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package worker

import (
	"fmt"
	"sort"
	"strings"

	"matrix/cody/internal/llm"
)

// cassandra.go — Cassandra 2.0, the silent-voice controller (Cody worker).
//
// Ported from Neo (neo/internal/agent/cassandra.go). The proof-of-work
// goal-vs-outcome LLM adjudication (gate.Adjudicate over matrix/cassandra) is
// RETIRED from Cody; in its place Cassandra is a silent, first-person feedback
// controller INSIDE the worker loop: after an assistant turn is appended to
// w.messages it may edit that message's OWN Content in place — folding in the
// epistemic primitives Doubt / Questioning / Curiosity / Assurance /
// Urge-to-verify — so the model reads the edit as its own emerging thought on
// the next step (reasoning is not persisted across turns, so the assistant
// channel is the self). It is a two-sided damping controller (doubt damps
// over-confidence / looping; assurance damps thrashing / over-verification)
// and doubles as a self-healing loop killer that fires BEFORE the hard
// loopDetector stop. It is SILENT when healthy: at most one mod per step,
// drift-gated, starting at step >= min_step, and a clean run is byte-identical
// to the controller disabled.
//
// Three hard guardrails: (1) content-only — it mutates ONLY the Content of an
// assistant-role message, never ToolCalls/Role/Name and never a tool/user/
// system message; (2) metacognition-only — additive first-person framing,
// never editing facts/numbers/code/tool arguments; (3) dual-record — every mod
// records {original_content, cassandra_mod, trigger} to an audit side-channel
// so ground truth is always recoverable. It never fabricates a completion and
// never reaches a user-facing surface (the 3 Non-Negotiables). The worker's
// structural turn-in refusals (verify-before-done, read-full, running jobs)
// are loop discipline, not Cassandra, and are unchanged.

// auditEventMod is the single Cassandra 2.0 audit event: one silent
// modification of a prior assistant message. It rides the worker's optional
// Audit side-channel (wired by codyd to the run's event stream) as a pure
// observability signal — it is a no-op when no observer is set.
const auditEventMod = "cassandra.mod"

// modTrigger is the behavioral drift that armed a modification.
type modTrigger string

const (
	trigLoop            modTrigger = "loop"             // repeat count reached the loop threshold
	trigCyclic          modTrigger = "cyclic"           // rotating A→B→A→B, no new tool
	trigPrematureClose  modTrigger = "premature_close"  // bare message while still mid-repeat
	trigUnverifiedClose modTrigger = "unverified_close" // bare message with zero tool work on an action sheet
	trigThrash          modTrigger = "thrash"           // cyclic verify-type calls, nothing new
	trigOscillation     modTrigger = "oscillation"      // cyclic mutate-type calls (flip-flopping)
)

// modSide is the two-sided damping direction: doubt lowers unwarranted
// confidence / breaks loops; assurance stops thrashing / over-verification.
type modSide string

const (
	sideDoubt     modSide = "doubt"
	sideAssurance modSide = "assurance"
)

// cassandraMod is the dual-record for one silent modification (guardrail 3):
// {original_content, cassandra_mod, trigger, side, step, target}. It is the
// audit ground truth of what the worker actually said versus what Cassandra
// folded in.
type cassandraMod struct {
	Step     int
	Target   int // index into w.messages (an assistant-role message)
	Original string
	Mod      string
	Trigger  modTrigger
	Side     modSide
}

// cassandraSignals is the per-step behavioral read the controller classifies.
// It is derived from the loop's live batch against the controller's own
// committed history, so the controller reads REAL behavior one step before the
// hard loopDetector stop.
type cassandraSignals struct {
	step             int
	closing          bool           // this step is a bare message (no tool calls)
	calls            []llm.ToolCall // this step's tool batch (empty on a bare message)
	effectiveRepeats int            // no-progress repeat count INCLUDING this step's batch
	cyclic           bool           // rotating A→B→A→B cycle that introduced no new tool
	workDone         bool           // at least one tool batch ran earlier this run
}

// stallWindow bounds the recent-signature ring the cyclic detector scans.
const stallWindow = 6

// buildCassandraSignals derives the signal bundle for this step's batch
// against the committed history, then commits the batch so the next step
// compares against it.
func (w *Worker) buildCassandraSignals(step int, msg llm.Message, closing bool) cassandraSignals {
	calls := msg.ToolCalls
	sig := batchSignature(calls)
	introducedNewTool := false
	for _, c := range calls {
		if _, seen := w.casDistinct[c.Function.Name]; !seen {
			introducedNewTool = true
			break
		}
	}
	cyclic := len(calls) > 0 && !introducedNewTool && sigInWindow(w.casRecentSigs, sig)
	exact := len(calls) > 0 && sig == w.casPrevSig
	eff := w.casRepeats
	if exact || cyclic {
		eff++
	}
	out := cassandraSignals{
		step:             step,
		closing:          closing,
		calls:            calls,
		effectiveRepeats: eff,
		cyclic:           cyclic,
		workDone:         len(w.casDistinct) > 0,
	}
	// Commit this batch as the new history.
	if len(calls) > 0 {
		if exact || cyclic {
			w.casRepeats = eff
		} else {
			w.casRepeats = 0
		}
		w.casRecentSigs = append(w.casRecentSigs, sig)
		if len(w.casRecentSigs) > stallWindow {
			w.casRecentSigs = w.casRecentSigs[len(w.casRecentSigs)-stallWindow:]
		}
		w.casPrevSig = sig
		for _, c := range calls {
			w.casDistinct[c.Function.Name] = struct{}{}
		}
	}
	return out
}

// cassandraStep is the per-step controller entry point. It is a no-op before
// min_step, when the per-run mod budget is spent, when the per-trigger
// cooldown is active, or when no drift trigger fires (the healthy case → the
// window is byte-identical to the controller disabled). On a fire it edits the
// just-appended assistant message's Content in place, records the dual-record,
// and emits the cassandra.mod audit event. Returns whether it modified the
// window.
func (w *Worker) cassandraStep(sig cassandraSignals) bool {
	if sig.step < w.casMinStep() || w.casModsThisRun >= w.casMaxMods() {
		return false
	}
	trig, side := w.cassandraClassify(sig)
	if trig == "" {
		return false // healthy: no drift → zero modifications
	}
	if last, ok := w.casCooldown[trig]; ok && sig.step-last < w.casCooldownSteps() {
		return false
	}
	mod := cassandraTemplate(trig, sig)
	if strings.TrimSpace(mod) == "" {
		return false
	}
	if !w.cassandraEdit(len(w.messages)-1, mod, trig, side, sig.step) {
		return false
	}
	w.casCooldown[trig] = sig.step
	w.casModsThisRun++
	return true
}

// cassandraEdit folds a first-person metacognitive line into the Content of
// the assistant-role message at index target, IN PLACE. Guardrail 1
// (content-only) is enforced structurally: it asserts RoleAssistant and
// mutates ONLY .Content — never ToolCalls / Role / Name, and never a tool /
// user / system message. The fold is additive (the original text is preserved
// and the mod is layered AFTER it as the worker's next thought), so guardrail
// 2 (metacognition-only, never a rewrite of facts) holds by construction. The
// dual-record + audit event (guardrail 3) are written BEFORE the mutation.
// Returns false (no-op) if the target is out of range or is not an assistant
// message.
func (w *Worker) cassandraEdit(target int, mod string, trig modTrigger, side modSide, step int) bool {
	if target < 0 || target >= len(w.messages) {
		return false
	}
	if w.messages[target].Role != llm.RoleAssistant {
		return false // guardrail 1: only an assistant Content may be edited
	}
	original := w.messages[target].Content
	// Dual-record BEFORE mutating (guardrail 3).
	w.casRecord = append(w.casRecord, cassandraMod{
		Step: step, Target: target, Original: original, Mod: mod, Trigger: trig, Side: side,
	})
	// Fold the mod AFTER the original content (original preserved). When the
	// assistant turn had no content (straight to tools), the mod becomes the
	// content — the worker's emerging thought alongside its tool calls.
	folded := mod
	if strings.TrimSpace(original) != "" {
		folded = original + "\n\n" + mod
	}
	w.messages[target].Content = folded
	// Pure observability side-channel: no-op without an observer.
	if w.opts.Audit != nil {
		w.opts.Audit(auditEventMod, map[string]interface{}{
			"step":             step,
			"trigger":          string(trig),
			"side":             string(side),
			"target_index":     target,
			"original_content": original,
			"cassandra_mod":    mod,
		})
	}
	return true
}

// cassandraClassify maps the drift to (trigger, side), selecting the side that
// pushes the run toward the productive middle (two-sided damping, never
// one-sided doubt-spam). A strong loop is always doubt (step back, try a
// different approach); milder cyclic repetition splits by tool KIND — cyclic
// reads/checks are thrashing (assurance: commit and move on), cyclic mutations
// are oscillation (assurance: stop flip-flopping), and anything else cyclic is
// doubt. On a bare-message step the relevant drift is about finishing
// prematurely. Returns ("", "") when the run is healthy.
func (w *Worker) cassandraClassify(sig cassandraSignals) (modTrigger, modSide) {
	if sig.closing {
		if sig.effectiveRepeats >= w.casLoopThreshold() {
			return trigLoop, sideDoubt
		}
		if sig.workDone && sig.effectiveRepeats >= 1 {
			return trigPrematureClose, sideDoubt
		}
		if !sig.workDone {
			// A worker sheet is always an action task: a bare message with
			// zero tool work is drifting away from doing and verifying it.
			return trigUnverifiedClose, sideDoubt
		}
		return "", ""
	}
	// A genuine loop (many repeats) always warrants doubt — this is the
	// loop-kill trigger that fires one step before the hard loopDetector stop.
	if sig.effectiveRepeats >= w.casLoopThreshold() {
		return trigLoop, sideDoubt
	}
	// Milder cyclic repetition: split by tool kind (two-sided damping).
	if sig.cyclic {
		switch batchKind(sig.calls) {
		case kindVerify:
			return trigThrash, sideAssurance
		case kindMutate:
			return trigOscillation, sideAssurance
		default:
			return trigCyclic, sideDoubt
		}
	}
	return "", ""
}

// cassandraTemplate composes the first-person mod from a parameterized
// template that references the actual repeated operation where available, so
// it reads as a specific realization rather than boilerplate. It is
// metacognition only: epistemic framing about the worker's OWN work, never a
// claim of fact and never a fabricated completion.
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
	case trigPrematureClose:
		return "Before I wrap this up — did I actually verify the load-bearing parts, or am I assuming? Let me run the verification and turn in with honest status."
	case trigUnverifiedClose:
		return "I'm drifting into narrating instead of doing. The sheet needs real work and real verification — let me use the tools, then turn in honestly."
	case trigThrash:
		if op != "" {
			return fmt.Sprintf("I've already confirmed %s more than once — it's solid. I'm second-guessing a settled result; let me commit and move on to what's actually unresolved.", op)
		}
		return "I've already confirmed this more than once — it's solid. I'm second-guessing a settled result; let me commit and move on."
	case trigOscillation:
		return "I keep flip-flopping on this — both options are fine, and the flip-flopping itself is the real cost. Let me pick one, note why, and move on."
	}
	return ""
}

// batchSignature renders a stable signature of one tool batch.
func batchSignature(calls []llm.ToolCall) string {
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, c.Function.Name+"("+c.Function.Arguments+")")
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// sigInWindow reports whether sig already appears in the recent-signature window.
func sigInWindow(win []string, sig string) bool {
	for _, s := range win {
		if s == sig {
			return true
		}
	}
	return false
}

// batchKindT classifies a tool batch for two-sided damping.
type batchKindT int

const (
	kindOther batchKindT = iota
	kindVerify
	kindMutate
)

// batchKind classifies a whole batch: mutating tools dominate a verify/other
// mix (a write is the salient action), else verify dominates other, else other.
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

// toolKind classifies one tool by its base name against the worker's tool
// surface: state-mutating (fs_write/fs_edit/fs_delete/exec/service/kill) vs
// read/verify (fs_read/grep/glob/fs_list/verify_run/job_output/skill_load) vs
// other (turn_in and unknowns).
func toolKind(name string) batchKindT {
	lb := strings.ToLower(name)
	for _, k := range []string{"write", "edit", "create", "append", "save", "patch", "delete", "remove", "deploy", "send", "exec", "service", "kill"} {
		if strings.Contains(lb, k) {
			return kindMutate
		}
	}
	for _, k := range []string{"read", "search", "fetch", "get", "list", "glob", "find", "grep", "status", "check", "verify", "output", "info", "view", "inspect", "skill"} {
		if strings.Contains(lb, k) {
			return kindVerify
		}
	}
	return kindOther
}

// casOpLabel renders a short humanized label for the repeated operation, drawn
// from the batch's distinct tool names, for parameterizing a template. "" when
// there is no batch (a bare-message step).
func casOpLabel(calls []llm.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	names := map[string]struct{}{}
	for _, c := range calls {
		names[strings.ReplaceAll(c.Function.Name, "_", " ")] = struct{}{}
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

// --- config accessors (resolve defaults when an option is unset) ---

func (w *Worker) casMinStep() int {
	if w.opts.CassandraMinStep > 0 {
		return w.opts.CassandraMinStep
	}
	return 2
}

func (w *Worker) casMaxMods() int {
	if w.opts.CassandraMaxMods > 0 {
		return w.opts.CassandraMaxMods
	}
	return 3
}

func (w *Worker) casCooldownSteps() int {
	if w.opts.CassandraCooldown > 0 {
		return w.opts.CassandraCooldown
	}
	return 2
}

// casLoopThreshold resolves the repeat count that arms the doubt side. A
// positive option wins; 0 (the default) derives loopMaxRepeats-1 so it fires
// one step before the hard loopDetector stop.
func (w *Worker) casLoopThreshold() int {
	if w.opts.CassandraLoopThreshold > 0 {
		return w.opts.CassandraLoopThreshold
	}
	t := loopMaxRepeats - 1
	if t < 1 {
		t = 1
	}
	return t
}
