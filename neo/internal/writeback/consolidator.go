// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package writeback is Neo's automatic background consolidation pass (the
// frozen spec's write-back, option B): after each turn a cheap model sweeps
// the transcript and promotes durable learnings into cortex — objective facts
// (semantic), task outcomes (episodic), and reusable how-to patterns
// (procedural). The main agent never has to consciously call remember(); this
// keeps the durable store current so compaction only has to capture the
// ephemeral story-so-far.
package writeback

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
)

// Consolidator runs consolidation jobs on a background goroutine.
type Consolidator struct {
	cfg   config.Config
	extract  *llm.Client // durable-learning extraction (stronger; quality sets memory quality)
	classify *llm.Client // cheap relation-classify on the rare similar-neighbor path
	pager *memory.Pager

	jobs chan string
	done chan struct{}

	// P2-2: proposed skills awaiting activation. Populated by proposeIfReady
	// when a pattern's coverage crosses cfg.MinPatternSuccesses. Deduped by
	// the pattern's name (the dedup key). Guarded by proposeMu.
	proposeMu     sync.Mutex
	proposed      []string
	proposedSet   map[string]struct{}
	reinforceCount map[string]int // tracks how many times each pattern name was reinforced (for auto-propose)
}

// New builds a consolidator. extract drives the per-turn learning extraction
// (its quality directly sets memory quality); classify is the cheap model used
// only on the rare similar-neighbor relation check. A nil classify falls back
// to extract.
func New(extract, classify *llm.Client, pager *memory.Pager, cfg config.Config) *Consolidator {
	return &Consolidator{
		cfg:            cfg,
		extract:        extract,
		classify:       classify,
		pager:          pager,
		jobs:           make(chan string, 8),
		done:           make(chan struct{}),
		proposedSet:    make(map[string]struct{}),
		reinforceCount: make(map[string]int),
	}
}

// Start launches the worker. Safe to call once.
func (c *Consolidator) Start() {
	if c == nil {
		return
	}
	go c.loop()
}

// Consolidate enqueues a turn transcript for background consolidation. Never
// blocks the agent: if the queue is full the job is handed to a detached
// goroutine instead of being dropped, so a burst of turns never silently
// rots the durable store (cortex stays best-effort + eventually-current; the
// live transcript is ground truth for the turn anyway).
func (c *Consolidator) Consolidate(transcript string) {
	if c == nil || strings.TrimSpace(transcript) == "" {
		return
	}
	select {
	case c.jobs <- transcript:
	default:
		// Queue full (a burst of turns). Rather than silently DROP the learning
		// — which is how a durable memory quietly rots — process this one on a
		// detached goroutine. Bursts are rare (turns are serialized per
		// conversation and the worker drains continuously), so this overflow
		// path cannot fan out unboundedly in practice.
		go c.process(context.Background(), transcript)
	}
}

// ConsolidateSync runs the SAME extraction logic as Consolidate, but
// SYNCHRONOUSLY on the caller's goroutine — not through the async jobs
// channel. It is called from compact() before evicting older turns so that
// durable facts/events/patterns in the about-to-be-evicted transcript reach
// cortex BEFORE the turns are lost from the working window. This closes the
// gap where an async sweep could lag behind compaction and lose the last
// turns. The call respects the context deadline and never panics on a nil
// receiver. It does NOT touch the async queue (steady-state is unchanged).
func (c *Consolidator) ConsolidateSync(ctx context.Context, transcript string) {
	if c == nil || strings.TrimSpace(transcript) == "" {
		return
	}
	// Run the same extraction logic synchronously. We use a bounded sub-context
	// so a slow model does not stall compaction beyond the existing compaction
	// stall budget (the caller's context already bounds the total time).
	subCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c.process(subCtx, transcript)
}

// proposeIfReady checks if a pattern has accumulated enough successes
// (coverage >= cfg.MinPatternSuccesses) to be PROPOSED as a candidate skill.
// This is the auto-propose extension (P2-2): when a successful tool-sequence
// repeats past the existing confidence/N-success guard, the consolidator
// records the pattern's name as a proposed skill. Deduped by name (the dedup
// key). Below the guard, nothing happens (anti-overfit). Nil-safe.
func (c *Consolidator) proposeIfReady(spec memory.PatternSpec, coverage int) {
	if c == nil {
		return
	}
	if spec.IsEmpty() {
		return
	}
	if coverage < c.cfg.MinPatternSuccesses {
		return
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return
	}
	c.proposeMu.Lock()
	defer c.proposeMu.Unlock()
	if c.proposedSet == nil {
		c.proposedSet = make(map[string]struct{})
	}
	if c.reinforceCount == nil {
		c.reinforceCount = make(map[string]int)
	}
	if _, exists := c.proposedSet[name]; exists {
		return // dedup: same name already proposed
	}
	c.proposedSet[name] = struct{}{}
	c.proposed = append(c.proposed, name)
}

// ProposedSkills returns the list of skill names that have been auto-proposed
// by the consolidator (patterns that crossed the MinPatternSuccesses guard).
// Nil-safe: returns nil for a nil receiver.
func (c *Consolidator) ProposedSkills() []string {
	if c == nil {
		return nil
	}
	c.proposeMu.Lock()
	defer c.proposeMu.Unlock()
	out := make([]string, len(c.proposed))
	copy(out, c.proposed)
	return out
}

// Stop drains and shuts down the worker.
func (c *Consolidator) Stop() {
	if c == nil {
		return
	}
	close(c.jobs)
	<-c.done
}

func (c *Consolidator) loop() {
	defer close(c.done)
	for t := range c.jobs {
		c.process(context.Background(), t)
	}
}

const consolidatePrompt = `You are a memory consolidator for an AI agent. Read the interaction transcript and extract ONLY durable learnings worth keeping beyond this session. Be very selective — most interactions yield nothing, and that is the correct, common answer.

Return STRICT JSON, nothing else, in exactly this shape:
{"facts": ["..."], "user_facts": ["..."], "preferences": [{"topic": "...", "polarity": "prefer|avoid|do|dont", "strength": 0.8, "rationale": "..."}], "corrections": ["..."], "patterns": [{"name": "...", "trigger": "...", "preconditions": ["..."], "steps": ["..."], "gotchas": ["..."], "success_criteria": ["..."]}], "outcome": {"summary": "...", "status": "success|failure|partial"}}

Rules:
- facts: objective, durable truths about the user's repo, environment, or domain (NOT transient chit-chat, NOT the question itself). Usually [].
- user_facts: durable truths about the USER THEMSELVES — their name, role, stated identity, or stable working preferences (e.g. "The user's name is Andrew"). These are pinned to every future conversation, so include ONLY what the user actually asserted about themselves. Usually [].
- preferences: durable WORKING preferences about HOW the user wants you to behave — tone, format, which tools/surfaces to use, how to present results (e.g. topic "render a Construct surface while working on a task", polarity "do", strength 0.85). polarity is one of prefer|avoid|do|dont; strength is 0..1. Include ONLY a genuine standing preference, not a one-off request. Usually [].
- corrections: standing behavioral rules you learned because the USER CORRECTED YOU this turn (you did X, they told you to do Y instead). Each is ONE short imperative rule worded for your future self (e.g. "Always render a Construct surface when performing a task, not just describe it"). These get pinned to every future turn, so include ONLY genuine corrections the user actually made. Usually [].
- patterns: reusable how-to recipes worth reapplying to similar future tasks. Each is an object — name (short label), trigger (when to apply it), preconditions (what must be true first), steps (the proven tool sequence), gotchas (learned failure modes), success_criteria (how to know it worked). Omit a field if unknown. Usually [].
- outcome: include ONLY if a concrete task was actually completed or failed in this transcript; otherwise set it to null.
- Copy identifiers (addresses, tx hashes, IDs, file paths, numbers) VERBATIM.
- If nothing is durable, return {"facts": [], "user_facts": [], "preferences": [], "corrections": [], "patterns": [], "outcome": null}.`

type patternJSON struct {
	Name            string   `json:"name"`
	Trigger         string   `json:"trigger"`
	Preconditions   []string `json:"preconditions"`
	Steps           []string `json:"steps"`
	Gotchas         []string `json:"gotchas"`
	SuccessCriteria []string `json:"success_criteria"`
}

type prefJSON struct {
	Topic     string  `json:"topic"`
	Polarity  string  `json:"polarity"`
	Strength  float32 `json:"strength"`
	Rationale string  `json:"rationale"`
}

type extract struct {
	Facts       []string      `json:"facts"`
	UserFacts   []string      `json:"user_facts"`
	Preferences []prefJSON    `json:"preferences"`
	Corrections []string      `json:"corrections"`
	Patterns    []patternJSON `json:"patterns"`
	Outcome     *struct {
		Summary string `json:"summary"`
		Status  string `json:"status"`
	} `json:"outcome"`
}

func (c *Consolidator) process(ctx context.Context, transcript string) {
	if c.extract == nil || c.pager == nil {
		return
	}
	// The caller may have already bounded the context (ConsolidateSync).
	// For the async path (loop), ctx is context.Background() with a 60s timeout.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	res, err := c.extract.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			llm.SystemMessage(consolidatePrompt),
			llm.UserMessage("Transcript:\n\n" + transcript),
		},
	})
	if err != nil || res == nil {
		return
	}
	var out extract
	if err := parseLooseJSON(res.Message.Content, &out); err != nil {
		return
	}

	for i, f := range out.Facts {
		if i >= 5 {
			break
		}
		if s := strings.TrimSpace(f); s != "" {
			_, _ = c.pager.RememberFactRelated(ctx, s, c.classifyRelation)
		}
	}
	for i, f := range out.UserFacts {
		if i >= 5 {
			break
		}
		if s := strings.TrimSpace(f); s != "" {
			_, _ = c.pager.RememberUserFact(ctx, s)
		}
	}
	for i, pj := range out.Preferences {
		if i >= 5 {
			break
		}
		if strings.TrimSpace(pj.Topic) == "" {
			continue
		}
		strength := pj.Strength
		if strength <= 0 {
			strength = 0.7 // a stated preference is a strong default by construction
		}
		_, _ = c.pager.RememberPreferenceRelated(ctx, pj.Topic, pj.Polarity, strength, pj.Rationale, c.classifyRelation)
	}
	for i, corr := range out.Corrections {
		if i >= 5 {
			break
		}
		if s := strings.TrimSpace(corr); s != "" {
			// A correction is a learned do-rule, firm by default (a strong
			// standing default, not an inviolable hard rule).
			_, _ = c.pager.RememberConstraintRelated(ctx, s, "do", "firm", c.classifyRelation)
		}
	}
	for i, pj := range out.Patterns {
		if i >= 3 {
			break
		}
		spec := memory.PatternSpec{
			Name:            strings.TrimSpace(pj.Name),
			Trigger:         strings.TrimSpace(pj.Trigger),
			Preconditions:   pj.Preconditions,
			Steps:           pj.Steps,
			Gotchas:         pj.Gotchas,
			SuccessCriteria: pj.SuccessCriteria,
		}
		if spec.IsEmpty() {
			continue
		}
		c.proposeMu.Lock()
		c.reinforceCount[spec.Name]++
		coverage := c.reinforceCount[spec.Name]
		c.proposeMu.Unlock()
		_, _ = c.pager.ReinforcePattern(ctx, spec, nil)
		// P2-2: auto-propose a candidate skill when the pattern crosses
		// the MinPatternSuccesses guard. The coverage is tracked locally
		// (ReinforcePattern returns a URI, not a count). proposeIfReady
		// dedupes + guards the threshold.
		c.proposeIfReady(spec, coverage)
	}
	if out.Outcome != nil && strings.TrimSpace(out.Outcome.Summary) != "" {
		_, _ = c.pager.RecordOutcome(ctx, out.Outcome.Summary, mapOutcome(out.Outcome.Status), "")
	}
}

// relationClassifyPrompt drives the cheap-model relation classifier. It is
// asked ONLY when a new write has a topically-similar (but not identical)
// neighbor, so the model call is rare and cheap.
const relationClassifyPrompt = `You compare a NEW memory an AI agent just learned against EXISTING nearby memories of the same kind and decide their relationship. Return STRICT JSON, nothing else:
{"relation": "duplicate|supersedes|contradicts|relates|new", "target_uri": "<the single existing memory the relation is about, copied verbatim, or empty>", "reason": "<short>"}

Definitions:
- duplicate: the new memory says the same thing as an existing one (adds nothing). target_uri = that memory.
- supersedes: the new memory is an updated or corrected version of an existing one — same subject, newer/more correct assertion. target_uri = the OLD memory it replaces.
- contradicts: the new memory directly conflicts with an existing one (they cannot both be true). target_uri = the conflicting memory; reason = which part conflicts.
- relates: same topic as an existing one but a distinct, compatible assertion. target_uri = the related memory.
- new: unrelated to all listed memories. target_uri = empty.

Rules:
- Choose exactly ONE relation and at most ONE target_uri.
- target_uri MUST be one of the uri= values shown verbatim; never invent one.
- When unsure between contradicts and relates, choose relates (the benign link).`

type relationJSON struct {
	Relation  string `json:"relation"`
	TargetURI string `json:"target_uri"`
	Reason    string `json:"reason"`
}

// classifyRelation implements memory.RelationClassifier using the cheap
// consolidation model. It is passed to the Remember*Related write paths and
// invoked only when a similar neighbor exists. On any model/parse error it
// returns RelationNew (no edge), so a flaky classifier degrades to the plain
// v1 write rather than mislinking memories.
func (c *Consolidator) classifyRelation(ctx context.Context, newText string, candidates []memory.Neighbor) (memory.Relation, string, string) {
	if c == nil || len(candidates) == 0 {
		return memory.RelationNew, "", ""
	}
	m := c.classify
	if m == nil {
		m = c.extract
	}
	if m == nil {
		return memory.RelationNew, "", ""
	}
	var b strings.Builder
	b.WriteString("NEW memory:\n")
	b.WriteString(strings.TrimSpace(newText))
	b.WriteString("\n\nEXISTING nearby memories:\n")
	for _, cand := range candidates {
		fmt.Fprintf(&b, "- uri=%s\n  %s\n", cand.URI, strings.TrimSpace(cand.Text))
	}
	res, err := m.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			llm.SystemMessage(relationClassifyPrompt),
			llm.UserMessage(b.String()),
		},
	})
	if err != nil || res == nil {
		return memory.RelationNew, "", ""
	}
	var out relationJSON
	if err := parseLooseJSON(res.Message.Content, &out); err != nil {
		return memory.RelationNew, "", ""
	}
	return memory.ParseRelation(out.Relation), strings.TrimSpace(out.TargetURI), strings.TrimSpace(out.Reason)
}

func mapOutcome(s string) memory.Outcome {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "success":
		return memory.OutcomeSuccess
	case "failure":
		return memory.OutcomeFailure
	default:
		return memory.OutcomePartial
	}
}

// parseLooseJSON tolerates a model that wraps JSON in prose or code fences by
// extracting the outermost {...} object before unmarshaling.
func parseLooseJSON(s string, out interface{}) error {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if j := strings.LastIndexByte(s, '}'); j > i {
			s = s[i : j+1]
		}
	}
	return json.Unmarshal([]byte(s), out)
}
