// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// two-model-smoke — drive two real LLMs through a shared cortex via
// OpenAI-compatible tool calling, then assert Cortex.Rebuild preserves
// OverallRoot byte-identically.
//
// See ./README.md for usage and the contract this harness validates.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"matrix/cortex"
	"matrix/cortex/replay"
	"matrix/cortex/store"
)

func main() {
	root := flag.String("root", "", "cortex data root directory (required)")
	actor := flag.String("actor", "", "cortex actor name; both LLMs share this single Pebble DB (required)")
	turns := flag.Int("turns", 6, "total exchanges, alternating between agents A and B")
	toolsPerTurn := flag.Int("tools-per-turn", 8, "max tool calls per agent per turn")
	transcriptPath := flag.String("transcript", "transcript.jsonl", "JSONL transcript output path")
	scenarioName := flag.String("scenario", "default", "named scenario (currently only 'default')")
	temperature := flag.Float64("temperature", 0.4, "sampling temperature for both models")
	modelA := flag.String("model-a", "openai/gpt-oss-120b", "Together AI model id for agent A (researcher)")
	modelB := flag.String("model-b", "accounts/fireworks/models/deepseek-v4-flash", "Fireworks AI model id for agent B (reviewer)")
	noRebuildAssert := flag.Bool("no-rebuild-assert", false, "skip the final replay-determinism assertion (only for diagnostics)")
	flag.Parse()

	if *root == "" || *actor == "" {
		fmt.Fprintln(os.Stderr, "usage: two-model-smoke -root DIR -actor NAME [flags]")
		flag.PrintDefaults()
		os.Exit(2)
	}
	if *turns < 2 {
		die("turns must be >= 2 (need at least one exchange per agent)")
	}
	if *toolsPerTurn < 1 {
		die("tools-per-turn must be >= 1")
	}

	scenario, err := scenarioByName(*scenarioName)
	if err != nil {
		die("%v", err)
	}

	if err := os.MkdirAll(*root, 0o755); err != nil {
		die("mkdir root: %v", err)
	}
	s, err := store.Open(*root, *actor, nil)
	if err != nil {
		die("open store: %v", err)
	}
	defer s.Close()
	c := cortex.New(s)

	transcriptFile, err := os.OpenFile(*transcriptPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		die("open transcript: %v", err)
	}
	defer transcriptFile.Close()
	tr := newTranscript(transcriptFile)
	tr.event("config", map[string]interface{}{
		"root":         *root,
		"actor":        *actor,
		"turns":        *turns,
		"toolsPerTurn": *toolsPerTurn,
		"scenario":     scenario.Name,
		"temperature":  *temperature,
		"model_a":      *modelA,
		"model_b":      *modelB,
		"transcript":   *transcriptPath,
	})

	dispatcher := &Dispatcher{c: c, actor: *actor}
	tools := toolDefs()
	client := NewClient()

	agentA := &agent{
		Name:        "alice",
		Model:       *modelA,
		System:      scenario.SystemA,
		Temperature: *temperature,
		History:     []ChatMessage{{Role: "system", Content: scenario.SystemA}, {Role: "user", Content: scenario.Opening}},
	}
	agentB := &agent{
		Name:        "bob",
		Model:       *modelB,
		System:      scenario.SystemB,
		Temperature: *temperature,
		History:     []ChatMessage{{Role: "system", Content: scenario.SystemB}, {Role: "user", Content: scenario.Opening}},
	}

	tr.event("scenario", map[string]interface{}{
		"name":        scenario.Name,
		"description": scenario.Description,
		"opening":     scenario.Opening,
	})

	// Pre-existing cortex state baseline. Useful when the harness is
	// run on a non-empty actor — the rebuild assertion still holds
	// regardless, but transcript readers can see the deltas.
	if root, err := c.OverallRoot(); err == nil {
		tr.event("baseline", map[string]interface{}{
			"overall_root_pre": fmt.Sprintf("%x", root[:]),
		})
	}

	// Drive the conversation. Each iteration is one agent's turn.
	current, other := agentA, agentB
	for i := 1; i <= *turns; i++ {
		tr.event("turn_start", map[string]interface{}{
			"index":            i,
			"agent":            current.Name,
			"model":            current.Model,
			"history_messages": len(current.History),
		})
		final, usage, err := runOneTurn(client, current, tools, dispatcher, *toolsPerTurn, tr)
		if err != nil {
			tr.event("turn_error", map[string]interface{}{
				"index": i,
				"agent": current.Name,
				"error": err.Error(),
			})
			fmt.Fprintf(os.Stderr, "turn %d (%s): %v\n", i, current.Name, err)
			break
		}
		tr.event("turn_end", map[string]interface{}{
			"index":             i,
			"agent":             current.Name,
			"final_text":        final,
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.TotalTokens,
		})
		// Cross-pollinate: the other agent sees this final text as a
		// user message in their next turn.
		if final != "" {
			other.History = append(other.History, ChatMessage{
				Role:    "user",
				Content: fmt.Sprintf("(from %s) %s", current.Name, final),
			})
		}
		current, other = other, current
	}

	// Final assertion: snapshot, rebuild, verify root preservation.
	finalRun(c, tr, *noRebuildAssert)
}

// runOneTurn drives a single agent through up to N tool calls, then
// returns the final assistant text. Appends every message (assistant
// + tool results) to the agent's History so cross-turn context is
// preserved.
func runOneTurn(client *Client, ag *agent, tools []ToolDef, disp *Dispatcher, maxTools int, tr *transcript) (string, ChatUsage, error) {
	var totalUsage ChatUsage
	for hop := 0; hop < maxTools+1; hop++ {
		// Force a final answer on the last hop by stripping tools.
		callTools := tools
		if hop == maxTools {
			callTools = nil
		}
		msg, usage, err := client.Call(ag.Model, ag.History, callTools, ag.Temperature)
		if err != nil {
			return "", totalUsage, err
		}
		if usage != nil {
			totalUsage.PromptTokens += usage.PromptTokens
			totalUsage.CompletionTokens += usage.CompletionTokens
			totalUsage.TotalTokens += usage.TotalTokens
		}
		ag.History = append(ag.History, *msg)
		tr.event("assistant", map[string]interface{}{
			"agent":      ag.Name,
			"hop":        hop,
			"content":    truncate(msg.Content, 400),
			"tool_calls": len(msg.ToolCalls),
		})

		if len(msg.ToolCalls) == 0 {
			// Final assistant message; we're done with this turn.
			return msg.Content, totalUsage, nil
		}

		// Execute every tool call sequentially. Append a role="tool"
		// message per call carrying the JSON-encoded result.
		for _, call := range msg.ToolCalls {
			before := time.Now()
			result := disp.Dispatch(call, ag.Name)
			elapsed := time.Since(before)
			tr.event("tool_call", map[string]interface{}{
				"agent":     ag.Name,
				"hop":       hop,
				"tool":      call.Function.Name,
				"arguments": truncate(call.Function.Arguments, 400),
				"result":    truncate(result, 400),
				"elapsed":   elapsed.String(),
			})
			ag.History = append(ag.History, ChatMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: call.ID,
			})
		}
	}
	return "", totalUsage, fmt.Errorf("turn exceeded %d tool hops without producing a final assistant message", maxTools+1)
}

// finalRun captures pre/post OverallRoot via Cortex.Rebuild and asserts
// they match. This is the strongest single-call cortex correctness
// check (research/04-cortex.md §13.4 "indexes are pure projection of
// canonical state").
func finalRun(c *cortex.Cortex, tr *transcript, skip bool) {
	preSnap, err := c.Snapshot("two-model-smoke-end")
	if err != nil {
		tr.event("snapshot_error", map[string]interface{}{"error": err.Error()})
		fmt.Fprintf(os.Stderr, "snapshot: %v\n", err)
		return
	}
	tr.event("snapshot_taken", map[string]interface{}{
		"seq_at_snapshot": preSnap.SeqAtSnapshot,
		"journal_seq":     preSnap.JournalSeq,
		"overall_root":    fmt.Sprintf("%x", preSnap.OverallRoot[:]),
		"counters": map[string]uint64{
			"memories":   preSnap.Counters.Memories,
			"edges":      preSnap.Counters.Edges,
			"tombstoned": preSnap.Counters.Tombstoned,
		},
	})

	if skip {
		tr.event("rebuild_skipped", map[string]interface{}{"reason": "-no-rebuild-assert"})
		return
	}

	res, err := c.Rebuild(cortex.RebuildOptions{})
	if err != nil {
		tr.event("rebuild_error", map[string]interface{}{"error": err.Error()})
		fmt.Fprintf(os.Stderr, "rebuild: %v\n", err)
		return
	}
	tr.event("rebuild_done", map[string]interface{}{
		"journal_seq":             res.JournalSeq,
		"memories_scanned":        res.MemoriesScanned,
		"edges_scanned":           res.EdgesScanned,
		"journal_leaves_appended": res.JournalLeavesAppended,
		"pre_overall_root":        fmt.Sprintf("%x", res.PreOverallRoot[:]),
		"post_overall_root":       fmt.Sprintf("%x", res.PostOverallRoot[:]),
	})

	if err := replay.VerifyPreservesRoot(res); err != nil {
		tr.event("rebuild_assertion_failed", map[string]interface{}{"error": err.Error()})
		fmt.Fprintf(os.Stderr, "REBUILD ASSERTION FAILED: %v\n", err)
		os.Exit(1)
	}
	tr.event("rebuild_assertion_passed", map[string]interface{}{
		"overall_root": fmt.Sprintf("%x", res.PostOverallRoot[:]),
	})
	fmt.Fprintf(os.Stderr, "rebuild assertion passed; overall_root=%x...\n", res.PostOverallRoot[:8])
}

// agent holds per-conversation state. History accumulates across all
// turns this agent participates in; the system prompt is the first
// message and stays at index 0.
type agent struct {
	Name        string
	Model       string
	System      string
	Temperature float64
	History     []ChatMessage
}

// transcript is a tiny JSONL writer plus a stderr mirror for live
// progress visibility. Every line is one event with at least
// {"ts": ..., "kind": ...}.
type transcript struct {
	w io.Writer
}

func newTranscript(w io.Writer) *transcript { return &transcript{w: w} }

func (t *transcript) event(kind string, payload map[string]interface{}) {
	row := map[string]interface{}{
		"ts":   time.Now().UTC().Format(time.RFC3339Nano),
		"kind": kind,
	}
	for k, v := range payload {
		row[k] = v
	}
	enc, err := json.Marshal(row)
	if err != nil {
		fmt.Fprintf(os.Stderr, "transcript marshal: %v\n", err)
		return
	}
	_, _ = t.w.Write(enc)
	_, _ = t.w.Write([]byte("\n"))
	// Mirror a one-line summary to stderr for live tailing.
	fmt.Fprintf(os.Stderr, "[%s] %s\n", kind, oneLineSummary(payload))
}

func oneLineSummary(p map[string]interface{}) string {
	// Prioritize the most useful fields by kind.
	keys := []string{"agent", "tool", "index", "error", "overall_root", "post_overall_root", "content", "final_text", "result"}
	var parts []string
	for _, k := range keys {
		if v, ok := p[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%s", k, truncate(fmt.Sprint(v), 200)))
		}
	}
	return strings.Join(parts, " ")
}

// die prints a fatal message and exits with status 1.
func die(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "two-model-smoke: "+format+"\n", args...)
	os.Exit(1)
}

// silence "imported and not used" for filepath when path operations
// aren't directly used in this file but the dependency is required by
// callers.
var _ = filepath.Join

// Copyright © 2026 Paxlabs Inc. All rights reserved.
