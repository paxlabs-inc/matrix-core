// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"matrix/neo/internal/agent"
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
			results[idx] = e.runOneSubagent(ctx, r, swarmID, idx+1, spec)
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
func (e *Engine) runOneSubagent(ctx context.Context, r *run, swarmID string, index int, spec tools.SubagentSpec) subResult {
	// A fresh config with the sub-agent's name + smaller step budget.
	cfg := e.cfg
	cfg.AgentName = spec.Name
	if e.cfg.SubagentStepBudget > 0 {
		cfg.StepBudget = e.cfg.SubagentStepBudget
	}

	var (
		text    string
		ok      bool
		lastErr error
	)
	for attempt := 1; attempt <= subagentMaxAttempts; attempt++ {
		// Each attempt is a brand-new headless agent over a clean window, so a
		// retry never inherits the corrupted state that failed the last one.
		rep := &captureReporter{engine: e, run: r, swarmID: swarmID, index: index, name: spec.Name}
		sub := agent.New(agent.Options{
			Config:        cfg,
			Main:          e.main,
			Cheap:         e.cheap,
			Tools:         e.tools,
			Pager:         e.pager, // shared cortex READ lane; no consolidator (no write-back noise)
			Reporter:      rep,
			Observer:      func(ev agent.ToolEvent) { e.surfaceSubagentStep(r, swarmID, index, spec.Name, ev) },
			Persona:       spec.Persona,
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
		if !shouldRetrySubagent(attempt, subagentMaxAttempts, err, text != "", ctx.Err()) {
			break
		}
		// Transient hard failure with no usable output: announce the retry, back
		// off briefly (honoring parent cancellation), then rebuild a fresh window.
		e.publishSubagent(r, swarmID, index, "subagent.status", map[string]interface{}{
			"name":    spec.Name,
			"status":  "retrying",
			"attempt": attempt + 1,
			"reason":  clip(friendlyErr(err), 200),
		})
		if !swarmBackoff(ctx, attempt) {
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
// retry, returning false if the parent context is cancelled during the wait.
func swarmBackoff(ctx context.Context, attempt int) bool {
	d := time.Duration(attempt) * 500 * time.Millisecond
	if d > 3*time.Second {
		d = 3 * time.Second
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
