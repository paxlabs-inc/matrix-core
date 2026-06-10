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
{"facts": ["..."], "user_facts": ["..."], "patterns": [{"name": "...", "trigger": "...", "preconditions": ["..."], "steps": ["..."], "gotchas": ["..."], "success_criteria": ["..."]}], "outcome": {"summary": "...", "status": "success|failure|partial"}}

Rules:
- facts: objective, durable truths about the user's repo, environment, or domain (NOT transient chit-chat, NOT the question itself). Usually [].
- user_facts: durable truths about the USER THEMSELVES — their name, role, stated identity, or stable working preferences (e.g. "The user's name is Andrew"). These are pinned to every future conversation, so include ONLY what the user actually asserted about themselves. Usually [].
- patterns: reusable how-to recipes worth reapplying to similar future tasks. Each is an object — name (short label), trigger (when to apply it), preconditions (what must be true first), steps (the proven tool sequence), gotchas (learned failure modes), success_criteria (how to know it worked). Omit a field if unknown. Usually [].
- outcome: include ONLY if a concrete task was actually completed or failed in this transcript; otherwise set it to null.
- Copy identifiers (addresses, tx hashes, IDs, file paths, numbers) VERBATIM.
- If nothing is durable, return {"facts": [], "patterns": [], "outcome": null}.`

type patternJSON struct {
	Name            string   `json:"name"`
	Trigger         string   `json:"trigger"`
	Preconditions   []string `json:"preconditions"`
	Steps           []string `json:"steps"`
	Gotchas         []string `json:"gotchas"`
	SuccessCriteria []string `json:"success_criteria"`
}

type extract struct {
	Facts     []string      `json:"facts"`
	UserFacts []string      `json:"user_facts"`
	Patterns  []patternJSON `json:"patterns"`
	Outcome   *struct {
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
			_, _ = c.pager.RememberFact(ctx, s)
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
