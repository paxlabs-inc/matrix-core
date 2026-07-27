// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/tools"
)

// subagentTurnTimeout bounds a single sub-agent's whole run, independent of the
// parent turn's budget, so one stuck sub-agent can't hold the swarm open.
const subagentTurnTimeout = 12 * time.Minute

// subagentMaxAttempts is how many times a sub-agent's whole task is attempted
// before giving up: one initial run plus bounded recovery retries. A sub-agent
// whose loop hard-fails (a model/transport error that survives the in-loop
// retry ladder) used to die on the spot with an empty report; instead it gets a
// fresh window and another go, so a transient provider hiccup no longer wastes
// the whole sub-agent.
const subagentMaxAttempts = 2

// subagentMaxTransientAttempts is the retry ceiling for a leg that died on a
// PROVIDER-transient failure (rate-limited / unreachable upstream), higher than
// the default so a transient MiMo storm under concurrency — several legs hitting
// the provider at once — no longer kills a researcher after a single recovery
// try. A deterministic provider rejection still dies fast (a retry sends the same
// body), and a leg that produced ANY usable text is still never retried.
const subagentMaxTransientAttempts = 4

// staggerStep spreads concurrent leg starts so N legs don't hit the provider in
// the same instant and self-amplify a transient rate-limit storm — cheaper to not
// trigger the storm than to recover from it.
const staggerStep = 150 * time.Millisecond

// swarmActiveKey marks a context as already running inside a swarm, so a
// sub-agent that somehow reaches spawn_subagents is refused (no recursion /
// fork-bombs). Sub-agents are never advertised the tool, so this is a backstop.
type swarmActiveKey struct{}

func withSwarmActive(ctx context.Context) context.Context {
	return context.WithValue(ctx, swarmActiveKey{}, true)
}

func swarmActive(ctx context.Context) bool {
	v, _ := ctx.Value(swarmActiveKey{}).(bool)
	return v
}

// subResult is one sub-agent's outcome, collected for the aggregated digest
// returned to the parent agent's spawn_subagents tool call.
type subResult struct {
	index   int
	name    string
	persona string
	text    string
	ok      bool
}

// runSwarm is the SwarmFunc wired into the tool manager. It fans a set of
// task-scoped sub-agents out to run CONCURRENTLY — each its own headless agent
// loop over a fresh, isolated context window with the restricted (full Natural,
// no money, no recursion) tool surface — streams their progress onto the parent
// conversation's event stream as a live Agent Swarm, and returns an aggregated,
// model-readable digest of their findings. Heavy tool work stays in the
// sub-agents' windows; only the distilled results return to the parent's.
func (e *Engine) runSwarm(ctx context.Context, specs []tools.SubagentSpec) (string, error) {
	r := runFromContext(ctx)
	if r == nil {
		return "", fmt.Errorf("sub-agents need an active conversation")
	}
	if swarmActive(ctx) {
		// A sub-agent reached here: refuse rather than recurse.
		return "", fmt.Errorf("sub-agents cannot spawn their own sub-agents")
	}

	swarmID := synthSwarmID(r.id)
	meta := make([]map[string]interface{}, len(specs))
	for i, s := range specs {
		meta[i] = map[string]interface{}{
			"index":   i + 1,
			"name":    s.Name,
			"persona": s.Persona,
			"task":    s.Task,
		}
	}
	e.broker.publish(r.id, "swarm.started", "neo", map[string]interface{}{
		"intent_id":       r.id,
		"conversation_id": r.convID,
		"swarm_id":        swarmID,
		"count":           len(specs),
		"agents":          meta,
	})
	for i, s := range specs {
		e.publishSubagent(r, swarmID, i+1, "subagent.created", map[string]interface{}{
			"name":    s.Name,
			"persona": s.Persona,
			"task":    s.Task,
		})
		e.publishSubagent(r, swarmID, i+1, "subagent.status", map[string]interface{}{
			"name":   s.Name,
			"status": "running",
		})
	}

	// Bounded concurrency: run at most MaxConcurrentSubagents at once; the rest
	// queue. Failures are isolated — one sub-agent erroring never fails the
	// swarm; its honest partial is collected and reported (no false success).
	limit := e.cfg.MaxConcurrentSubagents
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	results := make([]subResult, len(specs))
	var wg sync.WaitGroup

	// Resolve the shared self-model ONCE for the whole swarm (the alignment
	// contract, self-model task 4.3, req.9.1): every sub-agent inherits the SAME
	// structural self-summary + how-I-fail patterns, so they act as one aligned
	// mind on scoped slices rather than divergent blank helpers.
	selfModel := e.subagentSelfModelBrief(ctx)

	for i, s := range specs {
		wg.Add(1)
		go func(idx int, spec tools.SubagentSpec) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[idx] = subResult{index: idx + 1, name: spec.Name, persona: spec.Persona, text: "cancelled before starting", ok: false}
				e.publishSubagent(r, swarmID, idx+1, "subagent.status", map[string]interface{}{"name": spec.Name, "status": "failed", "summary": "cancelled"})
				return
			}
			results[idx] = e.runOneSubagent(ctx, r, swarmID, idx+1, spec, selfModel)
		}(i, s)
	}
	wg.Wait()

	e.broker.publish(r.id, "swarm.completed", "neo", map[string]interface{}{
		"intent_id":       r.id,
		"conversation_id": r.convID,
		"swarm_id":        swarmID,
		"count":           len(specs),
	})

	return aggregateResults(results), nil
}

// runOneSubagent builds and runs a single headless sub-agent to completion,
// streaming its workspace activity onto the parent stream tagged with its
// index, and returns its distilled report. A hard failure with no usable output
// is retried with a fresh window (bounded by subagentMaxAttempts) so a
// transient provider error doesn't kill the sub-agent outright.
func (e *Engine) runOneSubagent(ctx context.Context, r *run, swarmID string, index int, spec tools.SubagentSpec, selfModel string) subResult {
	// A fresh config with the sub-agent's name + smaller step budget.
	cfg := e.cfg
	cfg.AgentName = spec.Name
	if e.cfg.SubagentStepBudget > 0 {
		cfg.StepBudget = e.cfg.SubagentStepBudget
	}

	// Stagger this leg's start so concurrent legs don't hit the provider in the
	// same instant and self-amplify a transient rate-limit storm. Bounded so a
	// high index can't over-delay; honors parent cancellation.
	if !swarmStagger(ctx, index) {
		return subResult{index: index, name: spec.Name, persona: spec.Persona, text: "cancelled before starting", ok: false}
	}

	var (
		text    string
		ok      bool
		lastErr error
	)
	for attempt := 1; attempt <= subagentMaxTransientAttempts; attempt++ {
		// Each attempt is a brand-new headless agent over a clean window, so a
		// retry never inherits the corrupted state that failed the last one.
		rep := &captureReporter{engine: e, run: r, swarmID: swarmID, index: index, name: spec.Name}
		// Background sub-agents run WITHOUT extended reasoning (subMain): only
		// the user-facing Neo loop and the core MCL pipeline think. Fall back to
		// the main client if no dedicated sub-agent client was wired.
		subMain := e.subMain
		if subMain == nil {
			subMain = e.main
		}
		sub := agent.New(agent.Options{
			Config:        cfg,
			Main:          subMain,
			Cheap:         e.cheap,
			Tools:         e.tools,
			Pager:         e.pager, // shared cortex READ lane; no consolidator (no write-back noise)
			Runtime:       e.runtime,
			Reporter:      rep,
			Observer:      func(ev agent.ToolEvent) { e.surfaceSubagentStep(r, swarmID, index, spec.Name, ev) },
			Persona:       spec.Persona,
			SelfModel:     selfModel,
			RestrictTools: true,
		})

		cctx, cancel := context.WithTimeout(withSwarmActive(ctx), subagentTurnTimeout)
		err := sub.Chat(cctx, spec.Task)
		cancel()

		text = strings.TrimSpace(rep.final())
		ok = err == nil
		lastErr = err
		if ok {
			break
		}
		// Classify the leg's terminal error to pick its retry ceiling: a
		// provider-transient failure (storm) gets more tries with hard backoff, a
		// deterministic rejection dies fast, everything else keeps the default.
		if !shouldRetrySubagent(attempt, subagentEffectiveMax(err), err, text != "", ctx.Err()) {
			break
		}
		// Hard failure with no usable output: announce the retry, back off
		// (harder for a provider storm so N legs don't re-collide the instant they
		// retry), honoring parent cancellation, then rebuild a fresh window.
		e.publishSubagent(r, swarmID, index, "subagent.status", map[string]interface{}{
			"name":    spec.Name,
			"status":  "retrying",
			"attempt": attempt + 1,
			"reason":  clip(friendlyErr(err), 200),
		})
		if !swarmBackoff(ctx, attempt, subagentFailureTransient(err)) {
			break
		}
	}

	if lastErr != nil && text == "" {
		text = "couldn't finish — " + friendlyErr(lastErr)
	}
	if text == "" {
		text = "(no findings returned)"
	}

	status := "done"
	if !ok {
		status = "failed"
	}
	e.publishSubagent(r, swarmID, index, "subagent.status", map[string]interface{}{
		"name":    spec.Name,
		"status":  status,
		"summary": clip(text, 600),
		"ok":      ok,
	})
	return subResult{index: index, name: spec.Name, persona: spec.Persona, text: text, ok: ok}
}

// shouldRetrySubagent decides whether a failed sub-agent attempt warrants a
// fresh retry. Retry ONLY when the agent hard-failed (err != nil) and produced
// NO usable output, the parent context is still alive (ctxErr == nil), and
// attempts remain. A clean finish or any partial findings are accepted as-is —
// we never burn a retry on a sub-agent that already returned something useful,
// and a cancelled/timed-out parent stops immediately.
func shouldRetrySubagent(attempt, maxAttempts int, err error, haveText bool, ctxErr error) bool {
	return err != nil && !haveText && ctxErr == nil && attempt < maxAttempts
}

// swarmBackoff sleeps a bounded, attempt-scaled interval before a sub-agent
// retry, returning false if the parent context is cancelled during the wait. A
// provider storm (hard=true) backs off harder and longer so N recovering legs
// don't re-collide on the recovering upstream the instant they retry.
func swarmBackoff(ctx context.Context, attempt int, hard bool) bool {
	step := 500 * time.Millisecond
	maxD := 3 * time.Second
	if hard {
		step = 1500 * time.Millisecond
		maxD = 8 * time.Second
	}
	d := time.Duration(attempt) * step
	if d > maxD {
		d = maxD
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// swarmStagger delays leg `index` by (index-1)*staggerStep before its first
// attempt so concurrent legs don't start in lockstep, capped so a high index
// can't over-delay. Returns false if the parent context is cancelled during the
// wait. Leg 1 never waits.
func swarmStagger(ctx context.Context, index int) bool {
	if index <= 1 {
		return true
	}
	d := time.Duration(index-1) * staggerStep
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// subagentEffectiveMax classifies a leg's terminal error to pick its retry
// ceiling: a deterministic provider rejection won't change on retry (die fast — a
// single attempt); a provider-transient failure (rate-limited / unreachable) gets
// the higher transient ceiling with hard backoff; everything else keeps the
// default recovery budget.
func subagentEffectiveMax(err error) int {
	switch {
	case errors.Is(err, llm.ErrProviderRejected):
		return 1
	case subagentFailureTransient(err):
		return subagentMaxTransientAttempts
	default:
		return subagentMaxAttempts
	}
}

// subagentFailureTransient reports whether a leg's error is a provider-transient
// failure — an upstream that is rate-limiting or unreachable, where a fresh leg
// after a hard backoff can succeed (the transient MiMo storm the extra attempts
// exist to ride out).
func subagentFailureTransient(err error) bool {
	return errors.Is(err, llm.ErrRateLimited) || errors.Is(err, llm.ErrProviderUnavailable)
}

// surfaceSubagentStep mirrors surfaceTool for a sub-agent: the same animated
// workspace step (terminal / browser / editor / …), tagged with the swarm +
// agent index so the client routes it into that sub-agent's own window.
func (e *Engine) surfaceSubagentStep(r *run, swarmID string, index int, name string, ev agent.ToolEvent) {
	if r == nil {
		return
	}
	step := describeStep(ev)
	step["intent_id"] = r.id
	step["conversation_id"] = r.convID
	step["swarm_id"] = swarmID
	step["agent_index"] = index
	step["agent_name"] = name
	e.broker.publish(r.id, "subagent.step", "neo", step)
}

// publishSubagent emits a swarm lifecycle event tagged with the swarm + agent
// index, carrying the supplied fields.
func (e *Engine) publishSubagent(r *run, swarmID string, index int, typ string, fields map[string]interface{}) {
	f := map[string]interface{}{
		"intent_id":       r.id,
		"conversation_id": r.convID,
		"swarm_id":        swarmID,
		"agent_index":     index,
	}
	for k, v := range fields {
		f[k] = v
	}
	e.broker.publish(r.id, typ, "neo", f)
}

// subagentMaxInheritedPatterns caps how many how-I-fail patterns a sub-agent
// inherits, so the alignment context stays compact against the sub-agent's
// smaller window while still carrying the recurring failure modes to avoid.
const subagentMaxInheritedPatterns = 5

// subagentSelfModelBrief renders the shared self-model each sub-agent inherits
// at spawn (self-model task 4.3, req.9.1): the structural self-summary (how the
// agent is built) plus the most salient how-I-fail patterns (what to avoid). It
// is resolved once per swarm from the shared cortex pager. Returns "" when no
// pager is wired or the self-model is empty (a fresh install before the
// self-graph is loaded) — the sub-agent then runs on its persona + bounds alone,
// never blocked on the self-model.
func (e *Engine) subagentSelfModelBrief(ctx context.Context) string {
	if e.pager == nil {
		return ""
	}
	model, err := e.pager.SelfModel(ctx)
	if err != nil {
		return ""
	}
	summary := strings.TrimSpace(model.Structural.Summary)
	if summary == "" && len(model.FailurePatterns) == 0 {
		return ""
	}
	var b strings.Builder
	if summary != "" {
		b.WriteString("Architecture (how you are built): ")
		b.WriteString(summary)
		b.WriteString("\n")
	}
	if len(model.FailurePatterns) > 0 {
		b.WriteString("Failure patterns to avoid:\n")
		for i, fp := range model.FailurePatterns {
			if i >= subagentMaxInheritedPatterns {
				break
			}
			stmt := strings.TrimSpace(fp.Statement)
			if stmt == "" {
				continue
			}
			b.WriteString("- ")
			b.WriteString(stmt)
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// capabilitySurface resolves the resident capability-surface material
// (epistemic-core req.2) from the durable self-model: the derived external API
// surface + is/is-not facts (written by the serving layer from its live route
// table), the self-authored failure patterns, and the structural summary. nil
// when no pager is wired — the agent then renders honest UNKNOWN gaps, never
// fabricated facts (req.2.3).
func (e *Engine) capabilitySurface(ctx context.Context) *agent.CapabilitySurface {
	if e.pager == nil {
		return nil
	}
	model, err := e.pager.SelfModel(ctx)
	if err != nil {
		return nil
	}
	cs := &agent.CapabilitySurface{StructuralSummary: strings.TrimSpace(model.Structural.Summary)}
	if sf := model.Structural.Surface; sf != nil {
		cs.API = append([]string(nil), sf.API...)
		cs.Is = append([]string(nil), sf.Is...)
		cs.IsNot = append([]string(nil), sf.IsNot...)
	}
	for _, fp := range model.FailurePatterns {
		if stmt := strings.TrimSpace(fp.Statement); stmt != "" {
			cs.FailurePatterns = append(cs.FailurePatterns, stmt)
		}
	}
	return cs
}

// aggregateResults distils the swarm's outcomes into one model-readable block
// the parent agent reads to compose its answer. Index-ordered (stable), each
// section labelled with the sub-agent's name + role so the parent can cite it.
func aggregateResults(results []subResult) string {
	var b strings.Builder
	done := 0
	for _, res := range results {
		if res.ok {
			done++
		}
	}
	fmt.Fprintf(&b, "Your %d sub-agents finished (%d succeeded). Their reports:\n", len(results), done)
	for _, res := range results {
		role := ""
		if res.persona != "" {
			role = " — " + res.persona
		}
		marker := ""
		if !res.ok {
			marker = " [did not fully complete]"
		}
		fmt.Fprintf(&b, "\n## %02d · %s%s%s\n%s\n", res.index, res.name, role, marker, strings.TrimSpace(res.text))
	}
	b.WriteString("\nSynthesize these into your answer for the user. Use their concrete findings (file paths, URLs, facts) verbatim; note honestly if any sub-agent didn't finish.")
	return b.String()
}

// captureReporter is a sub-agent's output sink: it captures the sub-agent's
// FINAL answer (Reporter.Say) for the aggregated digest, and forwards genuine
// narration / reasoning as lightweight notes onto that sub-agent's window so
// the user sees it working — without ever creating a top-level chat bubble.
type captureReporter struct {
	engine  *Engine
	run     *run
	swarmID string
	index   int
	name    string

	mu      sync.Mutex
	lastSay string
}

func (c *captureReporter) Say(text string, _ bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	c.mu.Lock()
	c.lastSay = text
	c.mu.Unlock()
}

func (c *captureReporter) final() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSay
}

func (c *captureReporter) Status(text string) {
	text = strings.TrimSpace(text)
	// Tool-start markers are surfaced richly by the observer; drop the bare one.
	if text == "" || strings.HasPrefix(text, "• ") {
		return
	}
	c.note(text, "status")
}

// Progress is a no-op for sub-agents: the synthetic narrate-before-act intent
// stub is ephemeral, not a durable distilled note (only genuine narration via
// Status becomes a captured note).
func (c *captureReporter) Progress(string) {}

func (c *captureReporter) Notice(text string) { c.note(text, "notice") }
func (c *captureReporter) Think(text string)  { c.note(text, "thinking") }

// Delta is a no-op for sub-agents: a swarm member reports through its distilled
// notes + final summary, not a live token stream (it has no own chat thread).
func (c *captureReporter) Delta(int, string, string) {}

func (c *captureReporter) note(text, kind string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	c.engine.publishSubagent(c.run, c.swarmID, c.index, "subagent.note", map[string]interface{}{
		"name": c.name,
		"kind": kind,
		"text": clip(text, 600),
	})
}

func synthSwarmID(seed string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("swarm|%d|%s", time.Now().UnixNano(), seed)))
	return "swarm_" + hex.EncodeToString(h[:8])
}
