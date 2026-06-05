// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"matrix/cortex"
	"matrix/cortex/memory"
	"matrix/executor/tool"
	"matrix/mcl/envelope"
	"matrix/mcl/ir"
)

// WalkResult collects the per-node outcomes of a manual plan walk so the
// attest stage can cite the resulting Event memories.
type WalkResult struct {
	NodeIDs       []string
	EventURIs     []memory.URI
	ToolDurations map[string]int64
	Errors        map[string]string
	IsErrors      map[string]bool
}

// WalkPlan walks the tree depth-first (with concurrent dispatch on parallel
// branches), invokes every ToolCall via the registry, journals each result
// into a cortex Event memory, and signs a plan.step envelope per node.
//
// Steps + Gates + SubDispatch nodes are skipped (the harness runs without a
// real executor model + without sub-agents); they're recorded in the
// transcript so the kind-coverage shows up in the post-run audit.
func WalkPlan(ctx context.Context, plan *ir.PlanTree, reg *tool.Registry, c *cortex.Cortex, drv *LifecycleDriver, actorURI string, t *Transcript) (*WalkResult, error) {
	wr := &WalkResult{
		ToolDurations: map[string]int64{},
		Errors:        map[string]string{},
		IsErrors:      map[string]bool{},
	}
	if err := walkNode(ctx, &plan.Root, plan.ID, reg, c, drv, actorURI, t, wr); err != nil {
		return wr, err
	}
	return wr, nil
}

func walkNode(ctx context.Context, n *ir.PlanNode, planID string, reg *tool.Registry, c *cortex.Cortex, drv *LifecycleDriver, actorURI string, t *Transcript, wr *WalkResult) error {
	switch n.Kind {
	case ir.NodeSequential:
		for i := range n.Children {
			if err := walkNode(ctx, &n.Children[i], planID, reg, c, drv, actorURI, t, wr); err != nil {
				return err
			}
		}
		return nil
	case ir.NodeParallel:
		// Run children concurrently. First failure propagates up.
		var wg sync.WaitGroup
		errCh := make(chan error, len(n.Children))
		for i := range n.Children {
			wg.Add(1)
			child := &n.Children[i]
			go func() {
				defer wg.Done()
				if err := walkNode(ctx, child, planID, reg, c, drv, actorURI, t, wr); err != nil {
					errCh <- err
				}
			}()
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			if err != nil {
				return err
			}
		}
		return nil
	case ir.NodeToolCall:
		return execToolCall(ctx, n, planID, reg, c, drv, actorURI, t, wr)
	case ir.NodeStep:
		t.Event("plan.step.skipped", "walk", map[string]interface{}{
			"node_id": n.ID,
			"reason":  "no executor model wired in e2e harness; kind-coverage only",
			"prompt":  n.Step.PromptName,
		})
		return nil
	case ir.NodeSubDispatch:
		t.Event("plan.subdispatch.skipped", "walk", map[string]interface{}{
			"node_id":   n.ID,
			"skill_ref": n.SubDispatch.SkillRef,
			"reason":    "in-process sub-dispatch deferred to v1.1 (Q6 lock)",
		})
		return nil
	case ir.NodeGate:
		t.Event("plan.gate.skipped", "walk", map[string]interface{}{
			"node_id":  n.ID,
			"question": n.Gate.Question,
			"reason":   "policy gate auto-approved in e2e harness (would block in production)",
		})
		return nil
	default:
		return fmt.Errorf("walk: unknown node kind %q (node %s)", n.Kind, n.ID)
	}
}

func execToolCall(ctx context.Context, n *ir.PlanNode, planID string, reg *tool.Registry, c *cortex.Cortex, drv *LifecycleDriver, actorURI string, t *Transcript, wr *WalkResult) error {
	tc := n.ToolCall
	wr.NodeIDs = append(wr.NodeIDs, n.ID)

	t.Event("plan.tool.dispatch", "walk", map[string]interface{}{
		"node_id":     n.ID,
		"tool":        tc.ToolRef,
		"side_effect": tc.SideEffectClass,
	})

	tl, err := reg.Get(tc.ToolRef)
	if err != nil {
		wr.Errors[n.ID] = err.Error()
		_, _ = signStep(drv, planID, n.ID, "failed", nil, err.Error(), 0)
		return fmt.Errorf("walk: registry.Get %s: %w", tc.ToolRef, err)
	}

	args := make(map[string]interface{}, len(tc.Args))
	for k, v := range tc.Args {
		args[k] = coerceArg(v)
	}

	timeoutMs := tc.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 30000
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	t0 := time.Now()
	res, err := tl.Call(callCtx, args)
	dur := time.Since(t0).Milliseconds()
	wr.ToolDurations[n.ID] = dur

	if err != nil {
		wr.Errors[n.ID] = err.Error()
		_, _ = signStep(drv, planID, n.ID, "failed", nil, err.Error(), dur)
		t.Event("plan.tool.error", "walk", map[string]interface{}{
			"node_id": n.ID, "tool": tc.ToolRef, "ms": dur, "error": err.Error(),
		})
		// Continue walking — Q14 IsError vs Go-err distinction; we treat
		// transport errors as recoverable in the e2e harness.
		// Still write a synthetic Event memory for cortex audit.
		evtURI, werr := writeToolEventMemory(c, actorURI, n, tc.ToolRef, "", err, true)
		if werr == nil {
			wr.EventURIs = append(wr.EventURIs, evtURI)
		}
		return nil
	}

	wr.IsErrors[n.ID] = res.IsError
	resultText := tool.ExtractText(res)
	if len(resultText) > 4000 {
		resultText = resultText[:4000] + "…(truncated)"
	}

	status := "completed"
	if res.IsError {
		status = "failed"
		wr.Errors[n.ID] = "tool returned IsError=true"
	}
	t.Event("plan.tool.result", "walk", map[string]interface{}{
		"node_id":        n.ID,
		"tool":           tc.ToolRef,
		"is_error":       res.IsError,
		"ms":             dur,
		"result_preview": truncate(resultText, 2000),
	})

	resBytes, _ := json.Marshal(map[string]interface{}{
		"call_id":     res.CallID,
		"is_error":    res.IsError,
		"duration_ms": res.DurationMs,
		"text":        resultText,
	})

	if _, err := signStep(drv, planID, n.ID, status, resBytes, "", dur); err != nil {
		return fmt.Errorf("walk: sign plan.step: %w", err)
	}

	evtURI, err := writeToolEventMemory(c, actorURI, n, tc.ToolRef, resultText, nil, res.IsError)
	if err != nil {
		return fmt.Errorf("walk: write Event memory: %w", err)
	}
	wr.EventURIs = append(wr.EventURIs, evtURI)
	return nil
}

// signStep emits a plan.step envelope. Keeps the signature short.
func signStep(drv *LifecycleDriver, planID, nodeID, status string, result []byte, errMsg string, latencyMs int64) (*envelope.Envelope, error) {
	body := envelope.PlanStepBody{
		PlanID:    planID,
		NodeID:    nodeID,
		Status:    status,
		Result:    result,
		Error:     errMsg,
		LatencyMs: latencyMs,
	}
	return drv.stream.SignAndPersist(envelope.KindPlanStep, body, "", drv.stream.LastID())
}

// writeToolEventMemory persists a cortex Event capturing the tool outcome
// so subsequent attest can cite it. Spec: research/04 §4.2 EventKind=interaction.
func writeToolEventMemory(c *cortex.Cortex, actorURI string, n *ir.PlanNode, toolRef, summary string, callErr error, isError bool) (memory.URI, error) {
	outcome := memory.OutcomeSuccess
	short := "tool " + n.ID + " ok"
	if callErr != nil {
		outcome = memory.OutcomeFailure
		short = "tool " + n.ID + " failed: " + callErr.Error()
	} else if isError {
		outcome = memory.OutcomeFailure
		short = "tool " + n.ID + " in-band failure"
	}
	if summary != "" {
		short += " — " + truncate(strings.ReplaceAll(summary, "\n", " "), 120)
	}
	uri, err := c.Write(memory.Head{
		ActorScope: "private",
		Tags:       []memory.Tag{"e2e", "tool-event", memory.Tag(toolRef)},
	}, memory.EventData{
		SchemaVersion: 1,
		Kind:          memory.EventInteraction,
		IntentRef:     "matrix://intent/" + n.ID, // placeholder; real exec would use real intent id
		Counterparty:  toolRef,
		OutcomeVal:    outcome,
		Summary:       short,
		Artifacts:     []string{toolRef},
	}, cortex.WriteMeta{
		CreatedBy:  actorURI,
		Confidence: 1.0,
		Provenance: memory.Provenance{Source: memory.SourceObserved},
	})
	return uri, err
}

// coerceArg converts a string-typed PlanTree arg into its likely
// JSON-friendly type. The IR shape is map[string]string for canonical
// hashing simplicity; MCP servers expect ints/bools per their input
// schemas. The walker does best-effort coercion: pure-digit strings
// become int, "true"/"false" become bool, everything else stays string.
//
// A future plan-IR revision may add typed Args (oneof string/int/bool/
// number/array) at which point this becomes a no-op shim.
func coerceArg(v string) interface{} {
	switch v {
	case "true":
		return true
	case "false":
		return false
	}
	if v == "" {
		return v
	}
	// Try integer first (no fractional dot).
	if isAllDigitsOptSign(v) {
		var n int64
		_, err := fmt.Sscanf(v, "%d", &n)
		if err == nil {
			return n
		}
	}
	// Try float.
	if hasFloatShape(v) {
		var f float64
		_, err := fmt.Sscanf(v, "%g", &f)
		if err == nil {
			return f
		}
	}
	return v
}

func isAllDigitsOptSign(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' || s[0] == '+' {
		i = 1
		if len(s) == 1 {
			return false
		}
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func hasFloatShape(s string) bool {
	hasDot := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.':
			if hasDot {
				return false
			}
			hasDot = true
		case r == '-' || r == '+' || r == 'e' || r == 'E':
		default:
			return false
		}
	}
	return hasDot
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
