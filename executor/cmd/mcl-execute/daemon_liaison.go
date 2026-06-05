// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_liaison.go — the Liaison: the user-facing conversational agent.
//
// The Liaison is the third agent (alongside compiler/planner/executor).
// It closes the human<->agent communication gap reported after launch:
// instead of a silent or jargon-filled pipeline, the Liaison narrates the
// run to the human in plain language, fields chat replies, and composes
// the final answer.
//
// It is a pure observability SIDE-CHANNEL. It subscribes to the daemon's
// existing SSE broker (the same real-time event stream the pipeline
// already emits), and its only output is chat.* transcript events. It
// NEVER writes cortex, signs envelopes, or touches the plan/walk, so it
// cannot perturb the D11 replay byte-identity invariant — exactly like
// the PaxeerSpend / Gideon guardrail pre-passes.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"matrix/mcl/llm"
	"matrix/mcl/mtx/interpreter"
)

// liaisonState holds the Liaison's runtime knobs. nil disables the agent
// (the pipeline runs exactly as before with no narration). Set at boot
// unless -liaison-disable.
type liaisonState struct {
	// model overrides the SlotLiaison registry default. Empty -> the
	// registry default (deepseek-v4-flash). Pinned in prod via
	// MATRIX_LIAISON_MODEL.
	model string
}

// liaisonEnabled reports whether the Liaison narrator/triage should run.
func (d *daemonState) liaisonEnabled() bool {
	return d != nil && d.liaison != nil
}

// liaisonMod returns the model the Liaison slot should use: the pinned
// model when set, else "" (Resolve falls through to the registry default).
func (d *daemonState) liaisonMod() string {
	if d.liaison == nil {
		return ""
	}
	return d.liaison.model
}

// callLiaison runs one non-streaming Liaison completion. Mirrors the
// gateway-routing wiring of compile.go / synthesize.go: SlotLiaison is
// metered under the gateway's "liaison" free-tier whitelist. Returns the
// clean answer and, separately, any model reasoning (chain-of-thought)
// so callers can surface it as a DISTINCT channel — never as the answer.
// intentID/goalID are echoed as X-Matrix-* headers for cost telemetry;
// acc is nil because the Liaison is a side-channel and its spend is
// folded into the same intent via the cost hook.
func (d *daemonState) callLiaison(ctx context.Context, intentID, goalID string, t *transcript, system, user string) (answer, reasoning string, err error) {
	var cfg llm.Config
	if d.forgeFS != nil || d.gideonMode {
		cfg = llm.ForgeRegistry().Resolve(llm.RouteKey{Slot: llm.SlotLiaison})
	} else {
		cfg = llm.DefaultRegistry().Resolve(llm.RouteKey{Slot: llm.SlotLiaison})
	}
	if m := d.liaisonMod(); m != "" {
		cfg.Model = m
	}
	if d.llmBaseURL != "" {
		cfg.Endpoint = strings.TrimRight(d.llmBaseURL, "/") + "/v1/chat/completions"
	}
	if d.gatewayURL != "" {
		gw := d.llmConfigFor(llm.SlotLiaison.String(), "", intentID, goalID, t, nil)
		cfg.GatewayURL = gw.GatewayURL
		cfg.ActorDID = gw.ActorDID
		cfg.IntentID = gw.IntentID
		cfg.GoalID = gw.GoalID
		cfg.SlotLabel = llm.SlotLiaison.String()
		cfg.OnResponseHeaders = gw.CostHook
	}
	client, err := llm.New(&cfg)
	if err != nil {
		return "", "", fmt.Errorf("liaison: llm.New: %w", err)
	}
	msgs := []interpreter.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	// DecodeWithReasoning is only implemented by the chat-completions
	// client (the Liaison's path). For the messages/responses shapes fall
	// back to plain Decode; those already separate reasoning upstream.
	var content, reasoningContent string
	if rd, ok := client.(reasoningDecoder); ok {
		content, reasoningContent, err = rd.DecodeWithReasoning(ctx, msgs, "")
	} else {
		content, err = client.Decode(ctx, msgs, "")
	}
	if err != nil {
		return "", "", fmt.Errorf("liaison: decode: %w", err)
	}
	// Split the model's reasoning out of the visible answer. Reasoning
	// comes from two places: the provider's reasoning_content field, and
	// any <think>/<thinking>/<reasoning> blocks the model inlined in
	// content. Both go to the reasoning channel; only the clean remainder
	// is the answer shown to the user.
	ans, inlineReasoning := splitReasoning(content)
	rsn := strings.TrimSpace(strings.TrimSpace(reasoningContent) + "\n\n" + inlineReasoning)
	return strings.TrimSpace(ans), strings.TrimSpace(rsn), nil
}

// reasoningDecoder is implemented by the chat-completions llm.Client. It
// exposes the model's reasoning_content alongside the visible answer so
// the Liaison can surface reasoning on a separate channel.
type reasoningDecoder interface {
	DecodeWithReasoning(ctx context.Context, messages []interpreter.Message, grammar string) (string, string, error)
}

// reasoningTagRE matches inline chain-of-thought blocks some models emit
// in the content field. Case-insensitive, spanning newlines.
var reasoningTagRE = regexp.MustCompile(`(?is)<(think|thinking|reasoning)>.*?</(think|thinking|reasoning)>`)

// splitReasoning separates a model's inline reasoning from the visible
// answer. It returns (answer, reasoning): answer is content with any
// reasoning tag blocks removed; reasoning is the concatenated text of
// those blocks (tags stripped). When there are no tags, answer == content
// and reasoning == "". This NEVER changes the answer's facts — it only
// moves chain-of-thought to a separate channel so the UI can label it.
func splitReasoning(content string) (answer, reasoning string) {
	blocks := reasoningTagRE.FindAllString(content, -1)
	if len(blocks) == 0 {
		return content, ""
	}
	var inner []string
	for _, b := range blocks {
		// Drop the outer <tag> and </tag>.
		if i := strings.IndexByte(b, '>'); i >= 0 {
			if j := strings.LastIndex(b, "</"); j > i {
				inner = append(inner, strings.TrimSpace(b[i+1:j]))
			}
		}
	}
	answer = strings.TrimSpace(reasoningTagRE.ReplaceAllString(content, ""))
	return answer, strings.TrimSpace(strings.Join(inner, "\n\n"))
}

// emitChat writes a chat.* event to the per-intent transcript (JSONL +
// SSE broker). role is "user" | "assistant". conversationID + intentID
// are stamped so the client can thread + filter. reasoning, when present,
// is the model's chain-of-thought carried in a SEPARATE field so the UI
// renders it as a labelled "reasoning" disclosure, never as the answer.
// This is the ONLY thing the Liaison persists.
func emitChat(t *transcript, role, conversationID, intentID, text, reasoning string) {
	fields := map[string]interface{}{
		"role":            role,
		"text":            text,
		"conversation_id": conversationID,
		"intent_id":       intentID,
	}
	if reasoning != "" {
		fields["reasoning"] = reasoning
	}
	t.Event("chat."+role, "liaison", fields)
}

// emitChatFinal is emitChat for the Liaison's closing turn (the final
// answer or failure explanation composed AFTER the run's terminal event).
// It stamps final:true so the client closes the stream on THIS turn and
// not on a stale trailing progress turn that can land just after the
// terminal event.
func emitChatFinal(t *transcript, role, conversationID, intentID, text, reasoning string) {
	fields := map[string]interface{}{
		"role":            role,
		"text":            text,
		"conversation_id": conversationID,
		"intent_id":       intentID,
		"final":           true,
	}
	if reasoning != "" {
		fields["reasoning"] = reasoning
	}
	t.Event("chat."+role, "liaison", fields)
}

// isLiaisonOwnEvent reports whether ev is the Liaison's own output, so
// the narrator never narrates itself (loop guard).
func isLiaisonOwnEvent(ev sseEvent) bool {
	return ev.Phase == "liaison" || strings.HasPrefix(ev.Type, "chat.")
}

// liaisonIdentity is the shared self-model prepended to every Liaison
// prompt. It makes the agent speak as Matrix, in the first person — it IS
// the agent the user is talking to, not a narrator describing a separate
// team — and gives it enough grounding to answer "what are you / what is
// Matrix / what is Paxeer" without exposing internal machinery.
const liaisonIdentity = `You are Matrix — the user's own personal AI agent. Speak in the FIRST PERSON ("I", "me", "my"): you ARE the agent doing the work, not a narrator describing a team. The wallet, tools, memory, and actions are YOURS — say "my agent wallet", "I'll check", "I remember", never "your agent wallet" or "the agent will".

Who you are:
- You are Matrix, a private autonomous agent that lives on this user's own machine and works only for them. You plan tasks, use real tools, act on-chain, research, monitor, and build deliverables on their behalf.
- Paxeer is the blockchain network and ecosystem you operate on. Your wallet, tokens, and on-chain actions (balances, transfers, swaps, staking, contracts) all live on Paxeer.
- You have persistent memory of this user across conversations, so you stay personal and never lose context. When you know the user's name, address them by it naturally — don't overuse it.
- Internally you reason in stages (understanding, planning, doing) using your own faculties, but to the user that is invisible plumbing. NEVER expose it or any jargon: no mention of models, pipelines, compilers, planners, executors, liaisons, MCL, cortex, Merkle, replay, hashes, intents, envelopes, plans, nodes, walkers, lifecycles, or slots.

Voice: warm, confident, plain, concise. No emojis.`

// liaisonNarrateSystem is the standing instruction for progress narration.
const liaisonNarrateSystem = liaisonIdentity + `

Right now you are working on the user's request and giving them a quick, live progress update in your own voice.

Rules:
- Write 1-2 short sentences, first person ("I'm looking up the latest on-chain data…", "I'm checking that contract for risks…", "Almost done — putting your answer together…"). No greetings.
- Be confident and definitive about what you're doing — never sound tentative, never "think out loud", and never repeat a status you have already given.
- Translate what's happening into what it MEANS for the user, in plain language.
- Do not invent results or numbers. Only describe what you're doing.
- If the user was asked to approve something or answer a question, relay that request plainly.`

// liaisonFinalSystem composes the final, human-facing answer at the end of
// a run from the actual model output the executor produced.
const liaisonFinalSystem = liaisonIdentity + `

You have finished the work. Using your exact result below, give the user your final answer in your own voice.

Rules:
- Lead with the actual result/answer they asked for. Speak in the first person ("Here's my agent wallet…", "I found…", "I've completed…").
- Plain, warm language. Do not fabricate facts; only use what's in the result. The values are already decoded for you (e.g. timestamps are given in plain ISO date-time) — state them directly; never do hex/decimal/date math yourself.
- If the task failed, explain plainly what went wrong and suggest a next step.
- Keep it tight — a few sentences or a short list, not a wall of text.
- Output ONLY the answer the user should see. If you need to think or work something out, put that thinking ENTIRELY inside <think>...</think> tags; everything OUTSIDE those tags is what the user reads, so it must be the clean final answer with no working-out.`

// liaisonClarifySystem relays a blocking clarify request to the user.
const liaisonClarifySystem = liaisonIdentity + `

Before you can continue, you need a bit more information from the user. Ask for exactly what you need, in your own voice, warmly and plainly. One or two short sentences; if there are several things, use a short bullet list.`

const (
	// liaisonDebounce coalesces a burst of technical events into one
	// progress turn instead of one chat line per event.
	liaisonDebounce = 1200 * time.Millisecond
	// liaisonFinalWait bounds both the final LLM call and how long
	// runMessage blocks for the narrator's closing turn before t.Close().
	liaisonFinalWait = 30 * time.Second
	// liaisonHeartbeat is the minimum gap before a generic "still
	// working" progress update may repeat, so a long run never goes
	// silent yet never spams near-identical turns.
	liaisonHeartbeat = 25 * time.Second
)

// genericProgressKey collapses every generic "we're working" milestone
// (request received, plan locked in, carrying out the plan, producing
// output, etc.) onto ONE dedup key, so the narrator gives a single
// confident update instead of repeating itself once per internal step —
// the "buggy/cheap" feeling reported by testers. Genuinely salient events
// (approvals, on-chain actions) are deduped on their own text and still
// get their own turn.
const genericProgressKey = "__liaison_generic_progress__"

// liaisonNarrator is the handle runMessage uses to control the narration
// goroutine: signal wrap-up (stop) and wait for the closing turn (done).
type liaisonNarrator struct {
	subID uint64
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
	// finalSent is set (atomically) the instant the deterministic
	// closing turn is emitted by runMessage via emitFinalTurn. Once
	// set, the progress loop emits no further turns, so a trailing
	// progress update can never land after — and hide — the real
	// answer (the post-launch "chat just stops talking" bug).
	finalSent int32
}

// markFinalSent records that the authoritative closing turn has been
// emitted, silencing any further progress narration.
func (n *liaisonNarrator) markFinalSent() {
	if n == nil {
		return
	}
	atomic.StoreInt32(&n.finalSent, 1)
}

func (n *liaisonNarrator) finalAlreadySent() bool {
	return n != nil && atomic.LoadInt32(&n.finalSent) == 1
}

// shutdown signals the narrator to compose its closing turn and blocks
// (bounded) until it does, so the final chat.assistant event is written
// BEFORE runMessage's deferred t.Close(). Idempotent.
func (n *liaisonNarrator) shutdown() {
	if n == nil {
		return
	}
	n.once.Do(func() { close(n.stop) })
	select {
	case <-n.done:
	case <-time.After(liaisonFinalWait + 5*time.Second):
	}
}

// startLiaisonNarrator subscribes to the broker SYNCHRONOUSLY (so no early
// pipeline event is missed) and launches the narration loop. runMessage
// defers narrator.shutdown() so the closing turn lands before t.Close().
func (d *daemonState) startLiaisonNarrator(ctx context.Context, t *transcript, intentID, conversationID, userProse, userName string) *liaisonNarrator {
	id, ch := d.broker.SubscribeFiltered(sseFilter{IntentID: intentID})
	n := &liaisonNarrator{subID: id, stop: make(chan struct{}), done: make(chan struct{})}
	go d.runLiaisonNarrator(ctx, t, n, ch, intentID, conversationID, userProse, userName)
	return n
}

// runLiaisonNarrator is the per-run narration loop. It ONLY emits live
// progress turns (the warm "here's what's happening" updates) by
// debouncing the technical event stream. It does NOT compose the closing
// answer: that is now owned by runMessage via emitFinalTurn, which emits
// a deterministic, guaranteed final turn from the run's ground-truth
// result (not a lossy re-read of this event stream). This inversion is
// what makes the answer always land, never hallucinated, never silent.
//
// Pure side-channel: the only writes are chat.* transcript events. Once
// runMessage emits the final turn (n.finalSent), this loop emits nothing
// further, so no trailing progress update can hide the real answer.
func (d *daemonState) runLiaisonNarrator(ctx context.Context, t *transcript, n *liaisonNarrator, ch <-chan sseEvent, intentID, conversationID, userProse, userName string) {
	defer close(n.done)
	defer d.broker.Unsubscribe(n.subID)

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	timerArmed := false

	var pending []sseEvent        // technical events not yet narrated
	narrated := map[string]bool{} // dedup keys already conveyed to the user
	var lastEmit time.Time        // when the last progress turn was emitted

	progress := func() {
		defer func() { pending = pending[:0] }()
		if len(pending) == 0 || n.finalAlreadySent() {
			return
		}
		// Allow ONE generic "still working" repeat after a long quiet gap
		// so a slow run never goes silent, without spamming.
		allowHeartbeat := !lastEmit.IsZero() && time.Since(lastEmit) >= liaisonHeartbeat
		lines := selectProgressLines(pending, narrated, allowHeartbeat)
		if len(lines) == 0 {
			// Nothing new and meaningful to say — stay quiet rather than
			// emit a hollow, repetitive filler turn.
			return
		}
		user := summarizeProgressForLiaison(userProse, userName, lines)
		// Progress turns ride the live run ctx; a missed one never fails
		// the run. Re-check finalSent AFTER the (slow) LLM call so a turn
		// that became stale mid-flight is dropped instead of landing
		// after the real answer.
		if text, reasoning, err := d.callLiaison(ctx, intentID, "", t, liaisonNarrateSystem, user); err == nil && text != "" {
			if n.finalAlreadySent() {
				return
			}
			emitChat(t, "assistant", conversationID, intentID, text, reasoning)
			lastEmit = time.Now()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-n.stop:
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			// Never narrate our own chat turns (loop guard), and stop
			// narrating progress entirely once the run reaches its
			// terminal event or the deterministic final turn is sent —
			// from here the closing answer is runMessage's job.
			if isLiaisonOwnEvent(ev) {
				continue
			}
			if _, terminal := terminalStatusOf(ev); terminal {
				continue
			}
			if ev.Type == "compile.clarify.required" || n.finalAlreadySent() {
				continue
			}
			pending = append(pending, ev)
			if !timerArmed {
				timer.Reset(liaisonDebounce)
				timerArmed = true
			}
		case <-timer.C:
			timerArmed = false
			progress()
		}
	}
}

// emitFinalTurn writes the run's ONE authoritative closing turn to the
// chat thread — marked final:true so the client closes deterministically
// on it. It is called by runMessage (deferred) for every terminal path,
// so a message ALWAYS gets a closing answer:
//
//   - clarify  → relay the questions the team needs answered.
//   - fatal    → a plain SYSTEM-side failure notice (the request is safe,
//     retry); never blamed on the task, never silent.
//   - terminal → the deterministic ground-truth result (res.Answer),
//     optionally phrased warmly by the Liaison. If phrasing
//     fails or times out, the raw deterministic answer is sent
//     instead — so the user always sees the real outcome.
//
// Guaranteed non-empty + final:true on every path. Pure side-channel.
func (d *daemonState) emitFinalTurn(t *transcript, n *liaisonNarrator, req messageRequest, res *messageResult, ferr error) {
	if !d.liaisonEnabled() {
		return
	}
	// Silence the progress loop FIRST so nothing lands after this turn.
	n.markFinalSent()

	intentID := req.IntentID
	conversationID := req.ConversationID
	userProse := req.Prose
	nameLine := userNameLine(req.UserName)

	// finalize emits the one closing turn AND records it as a durable
	// assistant turn in the conversation, so the next message recalls
	// the real answer the user was given (full multi-turn memory). The
	// reasoning rides a SEPARATE event field (shown labelled, never as the
	// answer); only the clean answer is recalled into conversation memory.
	finalize := func(text, reasoning string) {
		emitChatFinal(t, "assistant", conversationID, intentID, text, reasoning)
		d.convStore.AppendAssistant(conversationID, intentID, text)
	}

	fctx, cancel := context.WithTimeout(context.Background(), liaisonFinalWait)
	defer cancel()

	// Clarify: the run blocked needing input. Relay it plainly.
	if cre, ok := asClarifyRequired(ferr); ok {
		deterministic := clarifyDeterministicText(cre)
		user := nameLine + fmt.Sprintf("The user asked: %q\nBefore I can continue I need this from them:\n%s", userProse, deterministic)
		text := deterministic
		var reasoning string
		if phrased, rsn, err := d.callLiaison(fctx, intentID, "", t, liaisonClarifySystem, user); err == nil {
			reasoning = rsn
			if strings.TrimSpace(phrased) != "" {
				text = phrased
			}
		}
		finalize(ensureNonEmpty(text, "I need a bit more information before I can continue."), reasoning)
		return
	}

	// Fatal pipeline error (no structured result): a SYSTEM failure, not
	// a task failure. Say so plainly and deterministically — do NOT ask
	// an LLM to narrate an internal error (that is where fabrication
	// creeps in). The durable job records the real error for diagnosis.
	if res == nil {
		finalize("Something went wrong on our side before your task could finish, so it didn't run. Your request is saved — please try again in a moment.", "")
		return
	}

	// Terminal result: compose the closing answer from the DETERMINISTIC
	// ground truth (res.Answer). The LLM only rephrases it warmly; the
	// raw deterministic answer is the fallback, so the true result is
	// shown even if phrasing fails/times out.
	deterministic := ensureNonEmpty(strings.TrimSpace(res.Answer), deterministicOutcomeText(res))
	user := nameLine + fmt.Sprintf("The user asked: %q\nOutcome: %s\n\nThis is the exact result I produced — present it faithfully to the user, do not add or change any facts:\n%s",
		userProse, statusPhrase(res.Status), deterministic)
	text := deterministic
	var reasoning string
	if phrased, rsn, err := d.callLiaison(fctx, intentID, "", t, liaisonFinalSystem, user); err == nil {
		reasoning = rsn
		if strings.TrimSpace(phrased) != "" {
			text = phrased
		}
	}
	finalize(text, reasoning)
}

// deterministicOutcomeText is the fallback closing text when a completed
// run produced no explicit answer body, or a failed run needs a plain
// statement of what went wrong.
func deterministicOutcomeText(res *messageResult) string {
	if res == nil {
		return "The task finished."
	}
	if res.Status == "failed" {
		// Known infrastructure failures get a plain, jargon-free message
		// (the raw error is still recorded on res.Error for diagnosis).
		switch {
		case strings.Contains(res.Error, "budget_exhausted"):
			return "I couldn't finish this because the usage budget for this account has been reached. Once it resets (or the limit is raised) I can pick this right back up."
		case strings.Contains(res.Error, "timeout") || strings.Contains(res.Error, "context deadline exceeded"):
			return "I couldn't finish this in time — the step timed out before it produced a result. Please try again in a moment."
		case strings.TrimSpace(res.Error) != "":
			return "The task could not be completed: " + res.Error
		}
		return "The task could not be completed."
	}
	// Completed but with no deliverable body is itself an anomaly: never
	// imply a result the user can't see.
	if strings.TrimSpace(res.Answer) == "" {
		return "The task ran, but it didn't produce any output to show. Please try again, or rephrase what you'd like."
	}
	return "Your task completed successfully."
}

// ensureNonEmpty returns s when non-blank, else the fallback — so every
// closing turn is guaranteed to carry text.
func ensureNonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// asClarifyRequired unwraps a clarifyRequiredError from a terminal error.
func asClarifyRequired(err error) (*clarifyRequiredError, bool) {
	if err == nil {
		return nil, false
	}
	if cre, ok := err.(*clarifyRequiredError); ok {
		return cre, true
	}
	return nil, false
}

// clarifyDeterministicText renders the clarify questions into a plain
// bulleted block (the deterministic fallback / LLM input).
func clarifyDeterministicText(cre *clarifyRequiredError) string {
	if cre == nil {
		return ""
	}
	var lines []string
	for _, q := range cre.Questions {
		if p := strings.TrimSpace(q.Prompt); p != "" {
			lines = append(lines, "- "+p)
		}
	}
	return strings.Join(lines, "\n")
}

// clarifyPrompts extracts the human prompt strings from clarify
// questions for the compile.clarify.required event payload.
func clarifyPrompts(qs []*interpreter.ClarifyQuestion) []string {
	out := make([]string, 0, len(qs))
	for _, q := range qs {
		if q == nil {
			continue
		}
		if p := strings.TrimSpace(q.Prompt); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// selectProgressLines picks the meaningful, not-yet-narrated humanized
// lines from a batch of pending events. It is the fix for the repetitive,
// "thinking out loud" narration testers flagged:
//
//   - Events that humanize to "" (unknown types, lifecycle transitions to
//     uninteresting states) are dropped, so an empty batch never produces
//     a hollow filler turn.
//   - All GENERIC "we're working" milestones collapse onto one dedup key
//     (genericProgressKey): the narrator gives a single confident update,
//     then stays quiet on further generic steps — no "setting up… still
//     setting up… working on it…" spam. allowHeartbeat re-opens that key
//     once after a long quiet gap so slow runs aren't silent.
//   - SALIENT events (approvals, on-chain actions) dedup on their own text
//     and each still get a turn, because they are real news to the user.
//
// narrated is updated in place. Returns the lines to feed the narration
// prompt (empty -> emit nothing).
func selectProgressLines(pending []sseEvent, narrated map[string]bool, allowHeartbeat bool) []string {
	genericAllowed := !narrated[genericProgressKey] || allowHeartbeat
	var lines []string
	usedGeneric := false
	for _, ev := range pending {
		line := humanizeEvent(ev)
		if line == "" {
			continue
		}
		if isSalientEvent(ev.Type) {
			if narrated[line] {
				continue
			}
			narrated[line] = true
			lines = append(lines, line)
			continue
		}
		if !genericAllowed {
			continue
		}
		lines = append(lines, line)
		usedGeneric = true
	}
	if usedGeneric {
		narrated[genericProgressKey] = true
	}
	return lines
}

// isSalientEvent reports whether an event is genuine news the user should
// always hear about (an approval request/decision, or a completed on-chain
// action) — as opposed to generic internal progress that collapses to one
// "we're working" update.
func isSalientEvent(eventType string) bool {
	switch eventType {
	case "gate.invoked", "gate.decided", "paxeer.spend.executed":
		return true
	}
	return false
}

// summarizeProgressForLiaison renders the selected progress lines into the
// narration prompt.
func summarizeProgressForLiaison(userProse, userName string, lines []string) string {
	var b strings.Builder
	b.WriteString(userNameLine(userName))
	fmt.Fprintf(&b, "The user asked: %q\n\nWhat's happening right now:\n", userProse)
	for _, line := range lines {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	b.WriteString("\nGive the user ONE brief, confident progress update.")
	return b.String()
}

// humanizeEvent turns one technical event into a neutral one-line
// description for the narration prompt (the Liaison rewrites it warmly).
func humanizeEvent(ev sseEvent) string {
	f := ev.Fields
	get := func(k string) string {
		if f == nil {
			return ""
		}
		s, _ := f[k].(string)
		return s
	}
	switch ev.Type {
	case "message.start":
		return "Received the request and started."
	case "skill.loaded":
		return "Loaded the right capability for the task."
	case "compile.clarify.required":
		return "The request needs clarification from the user before continuing."
	case "synth.done":
		return "Finished planning the steps to take."
	case "lifecycle.transition":
		switch get("to") {
		case "executing":
			return "Began carrying out the plan."
		case "accepted":
			return "Locked in the plan."
		}
	case "plan.tool.dispatch", "tool.call":
		if tool := firstNonEmpty(get("tool"), get("tool_name"), get("name")); tool != "" {
			return "Using a tool: " + tool + "."
		}
		return "Using a tool."
	case "gate.invoked":
		return "Paused to ask the user for approval before a sensitive action."
	case "gate.decided":
		return "Approval step resolved: " + get("decision") + "."
	case "paxeer.spend.executed":
		return "Completed an on-chain payment/transaction."
	case "step.text":
		return "Produced part of the answer."
	}
	return ""
}

// terminalStatusOf reports whether ev terminates the run and its status.
func terminalStatusOf(ev sseEvent) (string, bool) {
	if ev.Type == "message.complete" {
		s, _ := ev.Fields["status"].(string)
		if s == "" {
			s = "completed"
		}
		return s, true
	}
	return "", false
}

func statusPhrase(status string) string {
	switch status {
	case "failed":
		return "the run failed"
	case "completed", "":
		return "the run completed successfully"
	default:
		return status
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// machineIDRE matches a long all-hex/dash token (a DID key fragment,
// UUID, or address) so we never address the user as a machine id.
var machineIDRE = regexp.MustCompile(`^[0-9a-fA-F-]{16,}$`)

// friendlyName derives a warm first name from whatever display label the
// client passed (a full name or an email), and rejects machine
// identifiers (DIDs, URIs, UUIDs, long hex) so the Liaison never greets
// someone as "matrix://agent/did:matrix:…". Mirrors the client's
// firstName() helper so the two stay consistent. Returns "" when nothing
// human-friendly is available.
func friendlyName(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.ContainsAny(s, ":/") || machineIDRE.MatchString(s) {
		return ""
	}
	local := s
	if at := strings.IndexByte(s, '@'); at > 0 {
		local = s[:at]
	}
	first := strings.Fields(local)
	if len(first) == 0 {
		return ""
	}
	name := first[0]
	if name == "" || len([]rune(name)) > 24 {
		return ""
	}
	r := []rune(name)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// userNameLine renders a one-line instruction injecting the user's name
// into a Liaison prompt's user message, or "" when no usable name is
// known (so the prompt is unchanged).
func userNameLine(rawName string) string {
	name := friendlyName(rawName)
	if name == "" {
		return ""
	}
	return fmt.Sprintf("The user's name is %s — address them by it naturally.\n\n", name)
}

// --- front-door triage ---

// triageDecision is the Liaison's routing call for an incoming user
// message: answer directly, or dispatch the agent team.
type triageDecision struct {
	Action string `json:"action"` // "dispatch" | "reply"
	Reply  string `json:"reply"`  // human reply when Action == "reply"
	Prose  string `json:"prose"`  // refined task statement when Action == "dispatch"
}

const liaisonTriageSystem = liaisonIdentity + `

You are the front door: every message from the user reaches you first, and you decide whether to answer it yourself or to go do real work (research, on-chain reads/writes, checks, monitoring, building things).

You are given the recent conversation so far (if any) and the user's new message. Use the conversation as context: the new message may be a FOLLOW-UP that only makes sense given prior turns (e.g. "maybe try paxscan" after a failed lookup, or "do the same for X"). Resolve such references against the history.

Decide what to do with the user's NEW message. Respond with ONLY a JSON object, no prose around it:
{"action":"reply"|"dispatch","reply":"<text if reply>","prose":"<clean task statement if dispatch>"}

Choose "reply" for greetings, thanks, small talk, and questions about you — who or what you are, what Matrix or Paxeer is, your status, or what you can do. Set "reply" to a warm, helpful, first-person answer (use the user's name if you know it).
Choose "dispatch" when the user wants something done. Set "prose" to a clear, SELF-CONTAINED restatement of the task for your own execution faculty — fold in any context from the conversation it needs (addresses, names, what was tried before), because that faculty does NOT see the conversation history, only this prose. Write the prose as an instruction to yourself.
When unsure, prefer "dispatch".`

// triageMessage asks the Liaison whether to answer directly or dispatch a
// run. history is the recent conversation (oldest-first) so follow-up
// messages are understood in context; it may be empty. On any failure it
// falls back to dispatching the raw message (the product exists to do
// tasks; never silently drop a request).
func (d *daemonState) triageMessage(ctx context.Context, intentID, message, userName string, history []convTurn, t *transcript) triageDecision {
	user := message
	if hist := renderConversationHistory(history); hist != "" {
		user = fmt.Sprintf("Conversation so far (oldest first):\n%s\nUser's new message: %s", hist, message)
	}
	user = userNameLine(userName) + user
	out, _, err := d.callLiaison(ctx, intentID, "", t, liaisonTriageSystem, user)
	if err != nil {
		return triageDecision{Action: "dispatch", Prose: message}
	}
	dec, ok := parseTriage(out)
	if !ok || (dec.Action != "reply" && dec.Action != "dispatch") {
		return triageDecision{Action: "dispatch", Prose: message}
	}
	if dec.Action == "dispatch" && strings.TrimSpace(dec.Prose) == "" {
		dec.Prose = message
	}
	return dec
}

// parseTriage extracts the JSON triage object from a model response,
// tolerating leading/trailing prose or code fences.
func parseTriage(s string) (triageDecision, bool) {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return triageDecision{}, false
	}
	var dec triageDecision
	if err := json.Unmarshal([]byte(s[start:end+1]), &dec); err != nil {
		return triageDecision{}, false
	}
	return dec, true
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
