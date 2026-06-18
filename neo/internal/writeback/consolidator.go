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
	"time"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
)

// Consolidator runs consolidation jobs on a background goroutine.
type Consolidator struct {
	cfg   config.Config
	model *llm.Client
	pager *memory.Pager

	jobs chan string
	done chan struct{}
}

// New builds a consolidator over a (cheap) model and a pager.
func New(model *llm.Client, pager *memory.Pager, cfg config.Config) *Consolidator {
	return &Consolidator{
		cfg:   cfg,
		model: model,
		pager: pager,
		jobs:  make(chan string, 8),
		done:  make(chan struct{}),
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
// blocks the agent: if the queue is full the job is dropped (cortex stays a
// best-effort, eventually-current store; the live transcript is ground truth
// for the turn anyway).
func (c *Consolidator) Consolidate(transcript string) {
	if c == nil || strings.TrimSpace(transcript) == "" {
		return
	}
	select {
	case c.jobs <- transcript:
	default:
	}
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
		c.process(t)
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

func (c *Consolidator) process(transcript string) {
	if c.model == nil || c.pager == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := c.model.Chat(ctx, llm.ChatRequest{
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
		_, _ = c.pager.ReinforcePattern(ctx, spec, nil)
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
	if c == nil || c.model == nil || len(candidates) == 0 {
		return memory.RelationNew, "", ""
	}
	var b strings.Builder
	b.WriteString("NEW memory:\n")
	b.WriteString(strings.TrimSpace(newText))
	b.WriteString("\n\nEXISTING nearby memories:\n")
	for _, cand := range candidates {
		fmt.Fprintf(&b, "- uri=%s\n  %s\n", cand.URI, strings.TrimSpace(cand.Text))
	}
	res, err := c.model.Chat(ctx, llm.ChatRequest{
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
