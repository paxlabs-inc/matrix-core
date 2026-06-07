// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package runtime hosts the executor-side plan walker and skill loader.
//
// The walker promotes the working executor/cmd/mcl-e2e/walk.go contract
// (sess#22b — 75/75 assertions green against real Fireworks + Together +
// real npx/uvx MCP subprocess servers) into a reusable package.
//
// Citations for every design choice live in matrix.kvx Session 23
// locked design table (search S23Q*). Most load-bearing:
//
//	S23Q1  package path                — matrix.kvx mcl_next_entry_point
//	S23Q2  pluggable handler interfaces — Q6/Q10/Q13 v1 carve-outs
//	S23Q3  node-kind semantics          — MCL/ir/plan.go ValidNodeKinds
//	S23Q4  arg coercion verbatim from harness
//	S23Q9  material-correction halt path
//	S23Q11 transport vs in-band error split — Q14 lock
//	S23Q12 e2e harness keeps its own walker until later cleanup
//
// The walker is the only executor-side component that touches both
// the cortex Event memory surface AND the envelope chain in the same
// transaction. It is intentionally synchronous; streaming progress and
// SSE land in v1.1 per Q14.
package runtime

import (
	"context"
	"errors"
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

// EventSink is the structured-event surface the walker emits to.
//
// Signature mirrors executor/cmd/mcl-e2e/transcript.go:46 so the harness
// Transcript satisfies it directly. Production callers (cmd/mcl-execute)
// can wire their own JSONL writer. Calls are best-effort; failure to log
// MUST NOT fail the walk.
type EventSink interface {
	Event(eventType, phase string, fields map[string]interface{})
}

// EnvelopeSink signs and persists an envelope. The walker calls this
// once per ToolCall node to journal a plan.step envelope. Production
// implementations wrap the executor's per-intent EnvelopeStream
// (cmd/mcl-e2e/envelopes.go:53 is the reference). nil disables signing.
//
// `correlation` is the envelope being correlated to (usually plan.proposed);
// `causation` is the prior envelope in chronological chain. Both may be empty.
type EnvelopeSink interface {
	SignAndPersist(kind string, body interface{}, correlation, causation string) (*envelope.Envelope, error)
	LastID() string
}

// StepHandler is invoked for NodeStep nodes. Production callers wire
// this to the executor LLM (DefaultExecutorModel per Q13). The v1
// default is NoopStepHandler which logs the kind-coverage and returns
// success without invoking any model. Spec: research/06-agents.md §5.2
// (plan tree generation lives inside skills which run under the
// executor).
type StepHandler interface {
	HandleStep(ctx context.Context, plan *ir.PlanTree, node *ir.PlanNode) (*StepResult, error)
}

// StepResult carries the LLM response that a StepHandler produced.
type StepResult struct {
	// Outputs maps expected-output slot name to produced value.
	Outputs map[string]string
	// Text is the raw LLM output text for journaling.
	Text string
	// LatencyMs is the wall-clock of the underlying model call.
	LatencyMs int64
}

// SubDispatchHandler is invoked for NodeSubDispatch nodes. Default
// behaviour returns ErrSubDispatchNotImplemented because Q6 explicitly
// defers cross-agent sub-dispatch to v1.1 (matrix.kvx executor_locked_design Q6).
// In-process recursion under the same agent is a hook point — callers
// can wire a sub-walker by implementing this interface.
type SubDispatchHandler interface {
	HandleSubDispatch(ctx context.Context, parent *ir.PlanTree, node *ir.PlanNode) (*SubDispatchResult, error)
}

// SubDispatchResult carries the sub-intent's terminal outcome.
type SubDispatchResult struct {
	// SubIntentID is the ULID of the dispatched child intent.
	SubIntentID string
	// Outcome mirrors envelope.IntentAttestBody.Outcome (success|failure|partial).
	Outcome string
	// CitedURIs are matrix://cortex/... URIs the sub-intent depended on.
	CitedURIs []string
}

// GateHandler is invoked for NodeGate nodes. v1 ships AllowAllGate as
// the default (matches cmd/mcl-e2e/walk.go:94-100 behaviour: log + skip).
// Production callers wire a synchronous-wait handler that emits
// envelope.PolicyGateBody and blocks on PolicyGateResolveBody per Q10.
type GateHandler interface {
	HandleGate(ctx context.Context, node *ir.PlanNode) (*GateDecision, error)
}

// GateDecision is the resolved gate outcome.
type GateDecision struct {
	// Approved=true allows the walk to continue past the gate.
	Approved bool
	// Answer is the user-supplied (or auto-) answer text.
	Answer string
}

// Walker walks a PlanTree depth-first, dispatching ToolCall nodes through
// a tool.Registry and threading the lifecycle plus cortex journaling
// through the supplied sinks.
//
// Thread-safety: a single Walker may be Run() concurrently for multiple
// plans only if every dependency it holds is itself thread-safe. The
// stdlib path (cortex.Cortex, tool.Registry, EnvelopeStream) IS thread-safe.
type Walker struct {
	registry *tool.Registry
	cortex   *cortex.Cortex
	envelope EnvelopeSink
	events   EventSink

	step StepHandler
	sub  SubDispatchHandler
	gate GateHandler

	// correctionInbox + onCorrection drive Q11 live materiality.
	correctionInbox <-chan Correction
	onCorrection    CorrectionHandler

	// actorURI is stamped onto every Event memory written by the walker.
	// Cortex requires CreatedBy on every Write meta (cortex/cortex.go:138).
	actorURI string

	// clock is overridable for tests.
	clock func() time.Time
}

// WalkerParams configures a Walker. Registry is required; everything else
// has a working default that matches the cmd/mcl-e2e/walk.go behaviour.
type WalkerParams struct {
	Registry *tool.Registry

	// Cortex is the actor's typed memory store. nil disables Event-memory
	// journaling (the walker still emits transcript Events and plan.step
	// envelopes, but no cortex Write).
	Cortex *cortex.Cortex

	// Envelope sinks plan.step envelopes. nil disables envelope signing.
	Envelope EnvelopeSink

	// Events sinks structured walk events. nil → noOpSink (no-op writer).
	Events EventSink

	// Step / Sub / Gate handlers override defaults.
	Step StepHandler
	Sub  SubDispatchHandler
	Gate GateHandler

	// CorrectionInbox is polled between every node dispatch. Receiving a
	// correction triggers the OnCorrection callback. Caller is responsible
	// for closing the channel on shutdown. nil disables mid-walk
	// correction handling.
	//
	// Spec citation: matrix.kvx executor_locked_design Q11 ("Materiality
	// classification enforced live during plan walk — material
	// modifications halt execution until re-accept"). Without an inbox,
	// the walker is synchronous-only and corrections must be applied
	// between full walk invocations.
	CorrectionInbox <-chan Correction

	// OnCorrection is invoked when CorrectionInbox delivers a correction.
	// Returning Material=true causes Walker.Run to halt with
	// ErrMaterialCorrection. Returning Material=false logs the correction
	// and continues. Required when CorrectionInbox is non-nil.
	OnCorrection CorrectionHandler

	// ActorURI is stamped onto every Event memory's CreatedBy. Required
	// when Cortex != nil.
	ActorURI string

	// Clock for tests; defaults to time.Now.
	Clock func() time.Time
}

// Correction is a mid-walk intent.correct envelope plus its decoded body.
// Delivered through Walker.CorrectionInbox to be classified live per Q11.
type Correction struct {
	// EnvelopeID is the matrix-ULID of the intent.correct envelope.
	EnvelopeID string

	// Target is "intent" or "plan" — mirrors envelope.IntentCorrectBody.Target.
	Target string

	// Patches is the RFC 6902 JSON Patch bytes from the body.
	Patches []byte

	// Reason is the structured reason code from the body.
	Reason string

	// RetryFrom names the PlanNode.ID to resume from after a non-material
	// correction. Empty = no resume hint.
	RetryFrom string
}

// CorrectionHandler classifies an in-flight correction. Returning
// (material=true, ...) halts the walk with ErrMaterialCorrection.
//
// The handler typically wraps materiality.Classify against the
// pre/post plan-trees the caller maintains alongside the walker.
type CorrectionHandler func(ctx context.Context, c Correction) (material bool, reasons []string, err error)

// NewWalker constructs a Walker. Returns an error only if a required
// dependency is missing (Registry, or ActorURI when Cortex != nil).
func NewWalker(p WalkerParams) (*Walker, error) {
	if p.Registry == nil {
		return nil, errors.New("runtime: walker requires Registry")
	}
	if p.Cortex != nil && p.ActorURI == "" {
		return nil, errors.New("runtime: walker requires ActorURI when Cortex is set")
	}
	if p.CorrectionInbox != nil && p.OnCorrection == nil {
		return nil, errors.New("runtime: walker requires OnCorrection when CorrectionInbox is set")
	}
	w := &Walker{
		registry:        p.Registry,
		cortex:          p.Cortex,
		envelope:        p.Envelope,
		events:          p.Events,
		step:            p.Step,
		sub:             p.Sub,
		gate:            p.Gate,
		correctionInbox: p.CorrectionInbox,
		onCorrection:    p.OnCorrection,
		actorURI:        p.ActorURI,
		clock:           p.Clock,
	}
	if w.events == nil {
		w.events = noopSink{}
	}
	if w.step == nil {
		w.step = NoopStepHandler{}
	}
	if w.sub == nil {
		w.sub = NotImplementedSubDispatch{}
	}
	if w.gate == nil {
		w.gate = AllowAllGate{}
	}
	if w.clock == nil {
		w.clock = time.Now
	}
	return w, nil
}

// WalkResult collects per-node outcomes of a plan walk so the caller's
// attest stage can cite the resulting Event memories.
//
// Field semantics mirror cmd/mcl-e2e/walk.go:20 so harness code can
// migrate to the production walker without touching its assertion layer.
type WalkResult struct {
	// NodeIDs is the ordered list of ToolCall node IDs dispatched
	// (includes both successes and failures).
	NodeIDs []string

	// EventURIs is the parallel list of cortex Event memory URIs written
	// per ToolCall. Empty when Cortex is nil.
	EventURIs []memory.URI

	// ToolDurations maps node ID → tool call latency in milliseconds.
	ToolDurations map[string]int64

	// Errors maps node ID → transport/registry error message (when the
	// Go-level err was non-nil). Distinct from IsErrors.
	Errors map[string]string

	// IsErrors maps node ID → whether the tool reported IsError=true
	// in-band. Q14 lock: in-band failures are NOT Go errors.
	IsErrors map[string]bool

	// StepResults maps node ID → StepResult for NodeStep dispatches.
	StepResults map[string]*StepResult

	// GateDecisions maps node ID → GateDecision for NodeGate dispatches.
	GateDecisions map[string]*GateDecision

	// SubResults maps node ID → SubDispatchResult for NodeSubDispatch.
	SubResults map[string]*SubDispatchResult

	// Corrections records every intent.correct envelope handled during
	// the walk (Q11 live materiality). Append-only in arrival order.
	Corrections []CorrectionOutcome
}

// CorrectionOutcome records one Q11 correction classification.
type CorrectionOutcome struct {
	EnvelopeID string
	Material   bool
	Reasons    []string
	// Err is the stringified error from OnCorrection if any; empty on
	// success. Material corrections also halt Run() with
	// ErrMaterialCorrection, in addition to being recorded here.
	Err string
}

// Run walks plan depth-first and returns the aggregated result.
//
// Returns an error only when a NodeSequential child fails or the walker
// detects an unrecoverable structural problem (unknown node kind). All
// tool-call transport / in-band failures are captured in WalkResult and
// do NOT halt the walk (matches cmd/mcl-e2e/walk.go:153 contract).
func (w *Walker) Run(ctx context.Context, plan *ir.PlanTree) (*WalkResult, error) {
	if plan == nil {
		return nil, errors.New("runtime: nil PlanTree")
	}
	wr := &WalkResult{
		ToolDurations: map[string]int64{},
		Errors:        map[string]string{},
		IsErrors:      map[string]bool{},
		StepResults:   map[string]*StepResult{},
		GateDecisions: map[string]*GateDecision{},
		SubResults:    map[string]*SubDispatchResult{},
	}
	if err := w.walkNode(ctx, plan, &plan.Root, wr); err != nil {
		return wr, err
	}
	return wr, nil
}

// pollCorrections drains the CorrectionInbox non-blockingly between
// nodes and routes each delivered correction through OnCorrection.
// Returns ErrMaterialCorrection if any handler classified material;
// returns the handler's error if it returned one; nil otherwise.
//
// Spec citation: matrix.kvx executor_locked_design Q11 (live during
// plan walk) — implementation as a between-node hook rather than
// mid-node preemption, which would risk leaving partial-journal state.
func (w *Walker) pollCorrections(ctx context.Context, wr *WalkResult) error {
	if w.correctionInbox == nil {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case c, ok := <-w.correctionInbox:
			if !ok {
				w.correctionInbox = nil
				return nil
			}
			material, reasons, herr := w.onCorrection(ctx, c)
			w.events.Event("plan.correction.classified", "walk", map[string]interface{}{
				"envelope_id": c.EnvelopeID,
				"target":      c.Target,
				"reason":      c.Reason,
				"material":    material,
				"reasons":     reasons,
				"error":       errString(herr),
			})
			wr.Corrections = append(wr.Corrections, CorrectionOutcome{
				EnvelopeID: c.EnvelopeID,
				Material:   material,
				Reasons:    reasons,
				Err:        errString(herr),
			})
			if herr != nil {
				return fmt.Errorf("runtime: correction handler: %w", herr)
			}
			if material {
				return fmt.Errorf("%w: envelope=%s reasons=%v",
					ErrMaterialCorrection, c.EnvelopeID, reasons)
			}
		default:
			return nil
		}
	}
}

func (w *Walker) walkNode(ctx context.Context, plan *ir.PlanTree, n *ir.PlanNode, wr *WalkResult) error {
	if err := w.pollCorrections(ctx, wr); err != nil {
		return err
	}
	switch n.Kind {
	case ir.NodeSequential:
		for i := range n.Children {
			if err := w.walkNode(ctx, plan, &n.Children[i], wr); err != nil {
				return err
			}
		}
		return nil

	case ir.NodeParallel:
		// Goroutine-per-child + WaitGroup. First error wins.
		// Deferral noted in S23Q3: sibling cancellation NOT propagated
		// — running MCP calls reach their natural completion to avoid
		// partial-journal states.
		var wg sync.WaitGroup
		var mu sync.Mutex
		errCh := make(chan error, len(n.Children))
		for i := range n.Children {
			wg.Add(1)
			child := &n.Children[i]
			go func() {
				defer wg.Done()
				// Use a local result map and merge under mu so the
				// shared WalkResult maps stay race-free.
				if err := w.walkNodeWithLock(ctx, plan, child, wr, &mu); err != nil {
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
		return w.execToolCall(ctx, plan, n, wr, nil)

	case ir.NodeStep:
		return w.execStep(ctx, plan, n, wr, nil)

	case ir.NodeSubDispatch:
		return w.execSubDispatch(ctx, plan, n, wr, nil)

	case ir.NodeGate:
		return w.execGate(ctx, plan, n, wr, nil)

	default:
		return fmt.Errorf("runtime: unknown plan node kind %q (node %s)", n.Kind, n.ID)
	}
}

// walkNodeWithLock is the parallel-branch entry point. It threads a
// shared mutex through every recursive call so concurrent ToolCall /
// Step / Gate / SubDispatch goroutines never race on the result maps.
func (w *Walker) walkNodeWithLock(ctx context.Context, plan *ir.PlanTree, n *ir.PlanNode, wr *WalkResult, mu *sync.Mutex) error {
	switch n.Kind {
	case ir.NodeSequential:
		for i := range n.Children {
			if err := w.walkNodeWithLock(ctx, plan, &n.Children[i], wr, mu); err != nil {
				return err
			}
		}
		return nil
	case ir.NodeParallel:
		// Nested parallel: each inner branch gets the same mutex.
		var wg sync.WaitGroup
		errCh := make(chan error, len(n.Children))
		for i := range n.Children {
			wg.Add(1)
			child := &n.Children[i]
			go func() {
				defer wg.Done()
				if err := w.walkNodeWithLock(ctx, plan, child, wr, mu); err != nil {
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
		return w.execToolCall(ctx, plan, n, wr, mu)
	case ir.NodeStep:
		return w.execStep(ctx, plan, n, wr, mu)
	case ir.NodeSubDispatch:
		return w.execSubDispatch(ctx, plan, n, wr, mu)
	case ir.NodeGate:
		return w.execGate(ctx, plan, n, wr, mu)
	default:
		return fmt.Errorf("runtime: unknown plan node kind %q (node %s)", n.Kind, n.ID)
	}
}

// execToolCall dispatches one NodeToolCall: Registry.Get → Tool.Call
// → write Event memory → emit plan.step envelope.
func (w *Walker) execToolCall(ctx context.Context, plan *ir.PlanTree, n *ir.PlanNode, wr *WalkResult, mu *sync.Mutex) error {
	tc := n.ToolCall
	if tc == nil {
		return fmt.Errorf("runtime: NodeToolCall %s missing ToolCall payload", n.ID)
	}

	withLock(mu, func() { wr.NodeIDs = append(wr.NodeIDs, n.ID) })

	w.events.Event("plan.tool.dispatch", "walk", map[string]interface{}{
		"node_id":     n.ID,
		"tool":        tc.ToolRef,
		"side_effect": tc.SideEffectClass,
	})

	tl, err := w.registry.Get(tc.ToolRef)
	if err != nil {
		withLock(mu, func() { wr.Errors[n.ID] = err.Error() })
		w.signStep(plan.ID, n.ID, "failed", nil, err.Error(), 0)
		return fmt.Errorf("runtime: registry.Get %s: %w", tc.ToolRef, err)
	}

	// Resolve ${<nodeID>.output} references against the real upstream node
	// outputs the walker recorded on the plan tree, then coerce. Tool-call
	// args were previously passed verbatim (only NodeStep inputs were
	// resolved), so a plan that generated a value in one node (e.g. a codegen
	// step producing a tachyon_compile `sources` map) and referenced it in a
	// later tool_call sent the literal ${...} placeholder to the tool. The
	// snapshot is taken under the shared lock because parallel branches mutate
	// PlanNode.ResultText concurrently.
	var outputs map[string]string
	withLock(mu, func() { outputs = collectNodeOutputs(plan) })
	args := make(map[string]interface{}, len(tc.Args))
	for k, v := range tc.Args {
		args[k] = normalizeToolArg(k, resolveOutputRefs(v, outputs))
	}

	timeoutMs := tc.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 30000
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	t0 := w.clock()
	res, callErr := tl.Call(callCtx, args)
	dur := w.clock().Sub(t0).Milliseconds()
	withLock(mu, func() { wr.ToolDurations[n.ID] = dur })

	if callErr != nil {
		withLock(mu, func() { wr.Errors[n.ID] = callErr.Error() })
		w.signStep(plan.ID, n.ID, "failed", nil, callErr.Error(), dur)
		w.events.Event("plan.tool.error", "walk", map[string]interface{}{
			"node_id": n.ID,
			"tool":    tc.ToolRef,
			"ms":      dur,
			"error":   callErr.Error(),
		})
		// Q14 lock: transport errors are NOT walk-fatal; record a
		// failure Event memory and continue. Sibling nodes in
		// NodeSequential still halt because we return nil here; the
		// caller treats the captured error map as truth.
		evtURI, werr := w.writeEventMemory(n, tc.ToolRef, "", callErr, true)
		if werr == nil {
			withLock(mu, func() { wr.EventURIs = append(wr.EventURIs, evtURI) })
		}
		return nil
	}

	withLock(mu, func() { wr.IsErrors[n.ID] = res.IsError })
	fullText := tool.ExtractText(res)

	// Record the tool output on the node so a downstream NodeStep can
	// resolve ${<nodeID>.output} references against the real result
	// (transient, json:"-" — never hashed or journaled). Walks share
	// the same *PlanNode pointers, so the later step sees this write.
	//
	// Use a much larger cap than the 4000-char journal preview below:
	// a consumer step needs the WHOLE upstream payload, not a preview.
	// fleet_summary in particular puts its down/degraded problems digest
	// AFTER the per-node array, so a 4000-char cut hid the named down
	// nodes and produced an incomplete report. The bridge already caps
	// its own body (MAX_BYTES), so this is a defensive ceiling only.
	resolveText := fullText
	if len(resolveText) > 64000 {
		resolveText = resolveText[:64000] + "…(truncated)"
	}
	withLock(mu, func() { n.ResultText = resolveText })

	resultText := fullText
	if len(resultText) > 4000 {
		resultText = resultText[:4000] + "…(truncated)"
	}

	status := "completed"
	errMsg := ""
	if res.IsError {
		status = "failed"
		errMsg = "tool returned IsError=true"
		withLock(mu, func() { wr.Errors[n.ID] = errMsg })
	}

	w.events.Event("plan.tool.result", "walk", map[string]interface{}{
		"node_id":        n.ID,
		"tool":           tc.ToolRef,
		"is_error":       res.IsError,
		"ms":             dur,
		"result_preview": truncate(resultText, 2000),
	})

	resBytes := marshalToolResult(res, resultText)
	w.signStep(plan.ID, n.ID, status, resBytes, errMsg, dur)

	evtURI, err := w.writeEventMemory(n, tc.ToolRef, resultText, nil, res.IsError)
	if err != nil {
		return fmt.Errorf("runtime: write Event memory: %w", err)
	}
	withLock(mu, func() { wr.EventURIs = append(wr.EventURIs, evtURI) })
	return nil
}

// execStep dispatches a NodeStep to the configured StepHandler.
func (w *Walker) execStep(ctx context.Context, plan *ir.PlanTree, n *ir.PlanNode, wr *WalkResult, mu *sync.Mutex) error {
	if n.Step == nil {
		return fmt.Errorf("runtime: NodeStep %s missing Step payload", n.ID)
	}
	w.events.Event("plan.step.dispatch", "walk", map[string]interface{}{
		"node_id":     n.ID,
		"prompt_name": n.Step.PromptName,
	})
	res, err := w.step.HandleStep(ctx, plan, n)
	if err != nil {
		withLock(mu, func() { wr.Errors[n.ID] = err.Error() })
		w.signStep(plan.ID, n.ID, "failed", nil, err.Error(), 0)
		return nil
	}
	if res != nil {
		withLock(mu, func() {
			wr.StepResults[n.ID] = res
			// Chain step outputs too, so ${<nodeID>.output} works for
			// step→step references, not just tool→step.
			n.ResultText = res.Text
		})
		w.signStep(plan.ID, n.ID, "completed", []byte(res.Text), "", res.LatencyMs)
	}
	return nil
}

// execSubDispatch dispatches a NodeSubDispatch.
func (w *Walker) execSubDispatch(ctx context.Context, plan *ir.PlanTree, n *ir.PlanNode, wr *WalkResult, mu *sync.Mutex) error {
	if n.SubDispatch == nil {
		return fmt.Errorf("runtime: NodeSubDispatch %s missing payload", n.ID)
	}
	w.events.Event("plan.subdispatch.dispatch", "walk", map[string]interface{}{
		"node_id":   n.ID,
		"skill_ref": n.SubDispatch.SkillRef,
	})
	res, err := w.sub.HandleSubDispatch(ctx, plan, n)
	if err != nil {
		withLock(mu, func() { wr.Errors[n.ID] = err.Error() })
		w.signStep(plan.ID, n.ID, "failed", nil, err.Error(), 0)
		// SubDispatch failure halts the parent sequence — matches
		// research/02-protocol.md §13 subagent_failed reason.
		return fmt.Errorf("runtime: sub-dispatch %s: %w", n.ID, err)
	}
	if res != nil {
		withLock(mu, func() { wr.SubResults[n.ID] = res })
		w.signStep(plan.ID, n.ID, "completed", nil, "", 0)
	}
	return nil
}

// execGate dispatches a NodeGate.
func (w *Walker) execGate(ctx context.Context, plan *ir.PlanTree, n *ir.PlanNode, wr *WalkResult, mu *sync.Mutex) error {
	if n.Gate == nil {
		return fmt.Errorf("runtime: NodeGate %s missing payload", n.ID)
	}
	w.events.Event("plan.gate.dispatch", "walk", map[string]interface{}{
		"node_id":  n.ID,
		"question": n.Gate.Question,
	})
	dec, err := w.gate.HandleGate(ctx, n)
	if err != nil {
		withLock(mu, func() { wr.Errors[n.ID] = err.Error() })
		w.signStep(plan.ID, n.ID, "failed", nil, err.Error(), 0)
		return fmt.Errorf("runtime: gate %s: %w", n.ID, err)
	}
	if dec != nil {
		withLock(mu, func() { wr.GateDecisions[n.ID] = dec })
		status := "completed"
		errMsg := ""
		if !dec.Approved {
			status = "failed"
			errMsg = "gate denied"
			withLock(mu, func() { wr.Errors[n.ID] = errMsg })
		}
		w.signStep(plan.ID, n.ID, status, []byte(dec.Answer), errMsg, 0)
		if !dec.Approved {
			// Gate denial halts the walk — matches research/02 §14:
			// "When a gate fires, the executor MUST stop before the
			// gated step."
			return fmt.Errorf("runtime: gate %s denied", n.ID)
		}
	}
	return nil
}

// signStep emits a plan.step envelope through the configured EnvelopeSink.
// Best-effort: failure is logged but does NOT fail the walk because the
// cortex Event memory IS the authoritative audit trail per Phase 7
// memories_root anchoring.
func (w *Walker) signStep(planID, nodeID, status string, result []byte, errMsg string, latencyMs int64) {
	if w.envelope == nil {
		return
	}
	body := envelope.PlanStepBody{
		PlanID:    planID,
		NodeID:    nodeID,
		Status:    status,
		Result:    result,
		Error:     errMsg,
		LatencyMs: latencyMs,
	}
	_, err := w.envelope.SignAndPersist(envelope.KindPlanStep, body, "", w.envelope.LastID())
	if err != nil {
		w.events.Event("plan.step.sign_failed", "walk", map[string]interface{}{
			"node_id": nodeID,
			"error":   err.Error(),
		})
	}
}

// writeEventMemory persists a cortex Event capturing the tool outcome
// so subsequent attest can cite it (research/04 §4.2 EventKind=interaction).
// Returns ("", nil) when Cortex is not configured.
func (w *Walker) writeEventMemory(n *ir.PlanNode, toolRef, summary string, callErr error, isError bool) (memory.URI, error) {
	if w.cortex == nil {
		return "", nil
	}
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
	uri, err := w.cortex.Write(memory.Head{
		ActorScope: "private",
		Tags:       []memory.Tag{"walk", "tool-event", memory.Tag(toolRef)},
	}, memory.EventData{
		SchemaVersion: 1,
		Kind:          memory.EventInteraction,
		IntentRef:     "matrix://intent/" + n.ID, // placeholder if caller doesn't override
		Counterparty:  toolRef,
		OutcomeVal:    outcome,
		Summary:       short,
		Artifacts:     []string{toolRef},
	}, cortex.WriteMeta{
		CreatedBy:  w.actorURI,
		Confidence: 1.0,
		Provenance: memory.Provenance{Source: memory.SourceObserved},
	})
	return uri, err
}

// ---- default handler implementations ----

// NoopStepHandler emits a transcript event and returns success. v1
// default per S23Q2 — production callers wire an LLM-backed handler.
type NoopStepHandler struct{}

// HandleStep implements StepHandler.
func (NoopStepHandler) HandleStep(ctx context.Context, plan *ir.PlanTree, node *ir.PlanNode) (*StepResult, error) {
	// Caller's EventSink already logged the dispatch in execStep; we
	// just return an empty step result so the walk proceeds.
	return &StepResult{Outputs: map[string]string{}, Text: ""}, nil
}

// NotImplementedSubDispatch returns ErrSubDispatchNotImplemented. v1
// default per Q6 lock — cross-agent sub-dispatch lands in v1.1.
type NotImplementedSubDispatch struct{}

// HandleSubDispatch implements SubDispatchHandler.
func (NotImplementedSubDispatch) HandleSubDispatch(ctx context.Context, parent *ir.PlanTree, node *ir.PlanNode) (*SubDispatchResult, error) {
	return nil, ErrSubDispatchNotImplemented
}

// AllowAllGate auto-approves every gate. v1 default for headless CLI
// runs that don't have a human-in-loop responder wired (matches
// cmd/mcl-e2e/walk.go:94-100 behaviour).
type AllowAllGate struct{}

// HandleGate implements GateHandler.
func (AllowAllGate) HandleGate(ctx context.Context, node *ir.PlanNode) (*GateDecision, error) {
	return &GateDecision{Approved: true, Answer: "auto-approved (AllowAllGate)"}, nil
}

// ---- sentinel errors ----

// ErrSubDispatchNotImplemented signals the v1 placeholder behaviour for
// cross-agent sub-dispatch. Callers may errors.Is to route accordingly.
var ErrSubDispatchNotImplemented = errors.New("runtime: sub-dispatch not implemented in v1")

// ErrMaterialCorrection signals the walker received a material
// intent.correct mid-walk (S23Q9). Caller should transition lifecycle
// machine executing→accepted and re-derive a plan.
var ErrMaterialCorrection = errors.New("runtime: material correction; halt + rewind to accept")

// ---- internal helpers ----

// noopSink discards all events. Used when EventSink is nil.
type noopSink struct{}

func (noopSink) Event(eventType, phase string, fields map[string]interface{}) {}

// errString stringifies a nil-able error for transcript fields.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// withLock invokes fn under mu if mu is non-nil; otherwise inline.
// Used by execToolCall/execStep/etc to share one map-mutation helper
// between the sequential and parallel code paths.
func withLock(mu *sync.Mutex, fn func()) {
	if mu == nil {
		fn()
		return
	}
	mu.Lock()
	defer mu.Unlock()
	fn()
}

// truncate clips s to at most n runes (rough; treats bytes as runes for
// the ASCII-heavy paths the walker produces). Mirrors transcript.go:93.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
