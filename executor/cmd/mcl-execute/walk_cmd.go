// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// walk_cmd.go — the `walk` subcommand orchestrator.
//
// Pipeline:
//
//   1. Parse flags + open transcript.
//   2. Load skill via runtime.SkillLoader.
//   3. Build infra (manifest → MCP manager → registry → cortex + embedder).
//   4. Load/create actor identity + envelopeStream.
//   5. Compile prose → Intent (interactive clarify loop).
//   6. Drive lifecycle drafting → proposed (intent.compiled).
//   7. Drive lifecycle proposed → accepted (intent.accept).
//   8. Synthesize plan via executor LLM (plan_tree@1).
//   9. Drive lifecycle accepted → executing (plan.proposed).
//  10. Build runtime.Walker with llmStepHandler + stdinGateHandler +
//      optional inProcessSubDispatch.
//  11. walker.Run → WalkResult.
//  12. Drive lifecycle executing → completed (intent.attest) on success
//      or executing → failed (intent.fail) on walk-fatal error, both
//      with cortex.Attest pumping the salience EMA.
//
// All transitions are signed envelopes persisted under
// <journal-dir>/<intent-id>/. cortex.OverallRoot is captured pre/post
// for the audit trail. The transcript is JSONL.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"matrix/cortex"
	"matrix/executor/runtime"
	"matrix/mcl/ir"
)

// runWalk parses flags + dispatches the walk pipeline. Exits non-zero
// on any error after surfacing the message to stderr + transcript.
func runWalk(args []string) {
	fs := flag.NewFlagSet("walk", flag.ExitOnError)
	var (
		skillURI       = fs.String("skill", "", "matrix://skill/<slug>@<version> URI to compile against (REQUIRED)")
		prose          = fs.String("prose", "", "natural-language goal (REQUIRED)")
		verb           = fs.String("verb", "", "force compile-time verb (overrides classifier)")
		intentID       = fs.String("intent-id", "", "explicit intent ID (default: hash of (verb, prose))")
		manifestPath   = fs.String("manifest", "/root/matrix/agents/default.json", "agent manifest path")
		skillsRoot     = fs.String("skills-root", "/root/matrix/skills", "skill repository root")
		cortexRoot     = fs.String("cortex-root", "", "cortex storage root (empty disables cortex)")
		cortexActor    = fs.String("cortex-actor", "executor", "cortex actor namespace")
		journalDir     = fs.String("journal-dir", "/root/matrix/journal/logs", "envelope journal directory")
		transcriptPath = fs.String("transcript", "", "JSONL transcript path (empty=stderr only)")
		keyfile        = fs.String("keyfile", "/root/matrix/.matrix/executor.key", "ed25519 seed path (created if absent)")
		didLabel       = fs.String("did", "executor", "DID label suffix")
		compilerModel  = fs.String("compiler-model", "", "override compiler LLM model")
		executorModel  = fs.String("executor-model", "", "override executor LLM model")
		llmBaseURL     = fs.String("llm-base-url", "", "override LLM endpoint base URL (gateway / BYO swap)")
		seed           = fs.Int64("seed", 42, "compiler seed (D11)")
		gatePolicy     = fs.String("gate-policy", "prompt", "policy gate handling: approve|deny|prompt")
		withEmbedder   = fs.Bool("with-embedder", false, "start cortex hash embedder (in-process)")
		withFireworks  = fs.Bool("with-fireworks-embedder", false, "start cortex Fireworks embedder (REAL)")
		anchor         = fs.Bool("anchor", false, "request chain anchoring on intent.accept (D10)")
		interactive    = fs.Bool("interactive", true, "stdin clarify + gate prompts")
		maxClarify     = fs.Int("max-clarify", 3, "max clarify rounds before giving up")
		maxRetry       = fs.Int("max-retry", 2, "max plan-synthesis retry rounds")
		allowSubDisp   = fs.Bool("allow-sub-dispatch", false, "enable in-process sub-dispatch (Q6 v1 carve-out)")
		timeout        = fs.Duration("timeout", 10*time.Minute, "overall walk timeout")
	)
	fs.Parse(args)

	if *skillURI == "" {
		fatalf("walk: -skill is required")
	}
	if *prose == "" {
		fatalf("walk: -prose is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	ctx = withSignalCancel(ctx)

	t, err := openTranscript(*transcriptPath)
	if err != nil {
		fatalf("walk: open transcript: %v", err)
	}
	defer t.Close()

	t.Event("walk.start", "boot", map[string]interface{}{
		"skill":          *skillURI,
		"prose":          *prose,
		"manifest":       *manifestPath,
		"cortex_root":    *cortexRoot,
		"interactive":    *interactive,
		"with_fireworks": *withFireworks,
	})

	// 1. Load skill.
	loader := runtime.NewSkillLoader(*skillsRoot)
	skill, err := loader.Load(*skillURI)
	if err != nil {
		fatalf("walk: skill load: %v", err)
	}
	t.Event("skill.loaded", "boot", map[string]interface{}{
		"uri":     skill.URI,
		"hash":    skill.CanonicalHash,
		"verbs":   skill.MclVerbs,
		"display": skill.Display,
	})

	// 2. Infra (manifest + MCP manager + registry + cortex).
	in, err := newInfra(ctx, infraOpts{
		ManifestPath:       *manifestPath,
		CortexRoot:         *cortexRoot,
		CortexActor:        *cortexActor,
		WithEmbedder:       *withEmbedder,
		WithFireworksEmbed: *withFireworks,
		StderrSink:         os.Stderr,
	}, t)
	if err != nil {
		fatalf("walk: infra: %v", err)
	}
	defer in.Close()

	// 3. Identity + envelope stream.
	actor, err := loadOrCreateIdentity(*keyfile, *didLabel)
	if err != nil {
		fatalf("walk: identity: %v", err)
	}
	t.Event("identity.loaded", "boot", map[string]interface{}{
		"did":  actor.DID,
		"user": actor.UserURI,
	})

	resolvedIntentID := *intentID
	if resolvedIntentID == "" {
		resolvedIntentID = synthIntentID(*prose, *verb)
	}

	stream, err := newEnvelopeStream(*journalDir, resolvedIntentID, actor, t)
	if err != nil {
		fatalf("walk: envelope stream: %v", err)
	}

	drv, err := newLifecycleDriver(resolvedIntentID, stream, t)
	if err != nil {
		fatalf("walk: lifecycle: %v", err)
	}

	// 4. Compile (interactive clarify loop wired through stdin).
	compRes, err := compile(ctx, in.cortex, compileOpts{
		Skill:       skill,
		Prose:       *prose,
		Verb:        *verb,
		Actor:       actor.UserURI,
		Agent:       actor.AgentURI,
		IntentID:    resolvedIntentID,
		Model:       *compilerModel,
		BaseURL:     *llmBaseURL,
		Seed:        *seed,
		Interactive: *interactive,
		MaxClarify:  *maxClarify,
	}, t)
	if err != nil {
		fatalf("walk: compile: %v", err)
	}
	t.Event("compile.done", "compile", map[string]interface{}{
		"intent_id":   compRes.Intent.ID,
		"intent_hash": compRes.IntentHash,
		"verb":        compRes.Intent.Frame.Verb,
		"refs":        len(compRes.Intent.References),
		"unknowns":    len(compRes.Unknowns),
		"rounds":      compRes.Rounds,
		"ms":          compRes.LatencyMs,
	})

	// 5. Drive drafting → proposed.
	if _, err := drv.DriveCompiled(compRes.IntentJSON, compRes.LatencyMs); err != nil {
		fatalf("walk: drive compiled: %v", err)
	}

	// 6. Drive proposed → accepted.
	if _, err := drv.DriveAccept(compRes.IntentHash, *anchor); err != nil {
		fatalf("walk: drive accept: %v", err)
	}

	// 7. Synthesize plan.
	synthRes, err := synthesize(ctx, synthesizeOpts{
		Skill:    skill,
		Intent:   compRes.Intent,
		Manifest: in.manifest,
		Registry: in.registry,
		Manager:  in.manager,
		Agent:    actor.AgentURI,
		Model:    *executorModel,
		BaseURL:  *llmBaseURL,
		Seed:     *seed,
		MaxRetry: *maxRetry,
	}, t)
	if err != nil {
		// Synthesis failure is fatal; drive lifecycle to failed for audit.
		_, _ = signTerminalFail(ctx, drv, in.cortex, compRes.Intent, nil, nil,
			"correction_invalid", fmt.Sprintf("plan synthesis failed: %v", err), t)
		fatalf("walk: synthesize: %v", err)
	}
	t.Event("synth.done", "synth", map[string]interface{}{
		"plan_id":   synthRes.Plan.ID,
		"plan_hash": synthRes.PlanHash,
		"nodes":     countPlanNodes(&synthRes.Plan.Root),
		"rounds":    synthRes.Rounds,
		"ms":        synthRes.LatencyMs,
	})

	// 8. Drive accepted → executing.
	if _, err := drv.DrivePlanProposed(synthRes.PlanJSON); err != nil {
		fatalf("walk: drive plan: %v", err)
	}

	// 9. Capture cortex root pre-walk (audit).
	preRoot := captureRoot(in.cortex)
	t.Event("walk.cortex.pre", "walk", map[string]interface{}{
		"overall_root": preRoot,
	})

	// 10. Build walker.
	walker, err := buildWalker(in, drv, stream, actor, skill, *executorModel, *llmBaseURL, *seed,
		*gatePolicy, *allowSubDisp, t)
	if err != nil {
		fatalf("walk: build walker: %v", err)
	}

	// 11. Run walk.
	walkRes, werr := walker.Run(ctx, synthRes.Plan)

	postRoot := captureRoot(in.cortex)
	t.Event("walk.cortex.post", "walk", map[string]interface{}{
		"overall_root": postRoot,
		"changed":      preRoot != postRoot,
	})

	// 12. Terminal envelope.
	if werr != nil {
		reason := classifyWalkError(werr)
		_, ferr := signTerminalFail(ctx, drv, in.cortex, compRes.Intent, synthRes.Plan,
			walkRes, reason, werr.Error(), t)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "walk: drive fail: %v\n", ferr)
		}
		printSummary(t, drv, walkRes, "failed", werr.Error())
		os.Exit(1)
	}

	if hasIsErrors(walkRes) {
		// Tools reported in-band failure for at least one node.
		// Treat as "partial" success: still attest to capture cited
		// URIs but mark the lifecycle as failed for audit clarity.
		// Q14 lock: in-band failures are NOT Go errors but ARE
		// observable in WalkResult.IsErrors.
		_, ferr := signTerminalFail(ctx, drv, in.cortex, compRes.Intent, synthRes.Plan,
			walkRes, "tool_error", "tool reported in-band failure", t)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "walk: drive fail: %v\n", ferr)
		}
		printSummary(t, drv, walkRes, "failed", "tool reported in-band failure")
		os.Exit(1)
	}

	if _, err := signTerminalAttest(ctx, drv, in.cortex, compRes.Intent, synthRes.Plan, walkRes, t); err != nil {
		fmt.Fprintf(os.Stderr, "walk: drive attest: %v\n", err)
		os.Exit(1)
	}
	printSummary(t, drv, walkRes, "completed", "")
}

// buildWalker assembles a runtime.Walker with the production handlers.
// Sub-dispatch is enabled iff allowSubDispatch is true (Q6 v1 carve-out).
func buildWalker(in *infra, drv *lifecycleDriver, stream *envelopeStream, actor *actorIdentity, skill *runtime.LoadedSkill, execModel, llmBaseURL string, seed int64, gatePolicy string, allowSubDispatch bool, t *transcript) (*runtime.Walker, error) {
	stepH, err := newLLMStepHandler(execModel, llmBaseURL, seed, skill.URI, skill.MdBytes, t)
	if err != nil {
		return nil, fmt.Errorf("walker: step handler: %w", err)
	}
	gateH := newStdinGateHandler(gatePolicy, actor.UserURI, t)

	params := runtime.WalkerParams{
		Registry: in.registry,
		Cortex:   in.cortex,
		Envelope: stream,
		Events:   t,
		Step:     stepH,
		Gate:     gateH,
		ActorURI: actor.UserURI,
	}

	if allowSubDispatch {
		// Build a closure that lets the sub-dispatch recursively
		// synthesize + walk a sub-skill in the SAME process. Q6 v1
		// carve-out — same agent, in-process only.
		loader := runtime.NewSkillLoader("/root/matrix/skills")
		sub := &inProcessSubDispatch{
			loader: loader,
			t:      t,
			synthesizer: func(ctx context.Context, sk *runtime.LoadedSkill, parent *ir.PlanTree, node *ir.PlanNode) (*ir.PlanTree, error) {
				// sess#37c — Synthetic sub-Intent (matches daemon
				// path; see step_handler.go buildSubIntent for the
				// verb-resolution + parent-linkage rules).
				subIntent := buildSubIntent(sk, parent, node, actor)
				subRes, serr := synthesize(ctx, synthesizeOpts{
					Skill:    sk,
					Intent:   subIntent,
					Manifest: in.manifest,
					Registry: in.registry,
					Manager:  in.manager,
					Agent:    actor.AgentURI,
					Model:    execModel,
					BaseURL:  llmBaseURL,
					Seed:     seed,
					MaxRetry: 1,
					IntentID: subIntent.ID,
				}, t)
				if serr != nil {
					return nil, serr
				}
				return subRes.Plan, nil
			},
			makeWalker: func(sk *runtime.LoadedSkill) (*runtime.Walker, error) {
				return buildWalker(in, drv, stream, actor, sk, execModel, llmBaseURL, seed, gatePolicy, false, t)
			},
		}
		params.Sub = sub
	}

	return runtime.NewWalker(params)
}

// captureRoot returns the cortex OverallRoot in lowercase hex, or "" when
// cortex is nil or unreachable. Best-effort; never returns an error.
func captureRoot(c *cortex.Cortex) string {
	if c == nil {
		return ""
	}
	root, err := c.OverallRoot()
	if err != nil {
		return ""
	}
	return hexFromRoot(root[:])
}

// classifyWalkError maps a walker error to a research/02 §13 failure reason.
// Falls through to "tool_error" as the catch-all when the cause is unclear.
func classifyWalkError(err error) string {
	if err == nil {
		return "tool_error"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "material correction"):
		return "ambiguous_after_clarify"
	case strings.Contains(msg, "deadline"):
		return "deadline_exceeded"
	case strings.Contains(msg, "budget"):
		return "budget_exceeded"
	case strings.Contains(msg, "policy") || strings.Contains(msg, "denied"):
		return "policy_denied"
	case strings.Contains(msg, "sub-dispatch") || strings.Contains(msg, "subagent"):
		return "subagent_failed"
	case strings.Contains(msg, "constraint"):
		return "blocked_by_constraint"
	default:
		return "tool_error"
	}
}

// hasIsErrors returns true iff at least one ToolCall reported IsError=true.
func hasIsErrors(wr *runtime.WalkResult) bool {
	if wr == nil {
		return false
	}
	for _, b := range wr.IsErrors {
		if b {
			return true
		}
	}
	return false
}

// firstWalkError returns the first recorded node-level error (a Go-level
// decode/transport failure the walker records in WalkResult.Errors, e.g.
// a budget_exhausted 429 on a step's LLM decode). The walker records
// these but does NOT itself treat them as fatal, so runMessage must
// promote them to a terminal failure — otherwise a plan whose only
// content step errored still attests as "completed" and the user is told
// the task succeeded when nothing was produced (the silent-success bug).
//
// The node with the lexicographically-smallest id is chosen so the
// selected error is deterministic across runs/replays.
func firstWalkError(wr *runtime.WalkResult) (nodeID, msg string, ok bool) {
	if wr == nil || len(wr.Errors) == 0 {
		return "", "", false
	}
	for id, m := range wr.Errors {
		if nodeID == "" || id < nodeID {
			nodeID, msg = id, m
		}
	}
	return nodeID, msg, true
}

// classifyStepError maps a raw node error message to a stable terminal
// reason code for the audit trail and user-facing message selection.
func classifyStepError(msg string) string {
	switch {
	case strings.Contains(msg, "budget_exhausted"):
		return "budget_exhausted"
	case strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "timeout"):
		return "timeout"
	default:
		return "step_error"
	}
}

// printSummary writes a human-readable summary to stderr at end-of-walk.
// Exits without affecting the JSONL transcript layout.
func printSummary(t *transcript, drv *lifecycleDriver, wr *runtime.WalkResult, status, errorMsg string) {
	t.Event("walk.summary", "summary", map[string]interface{}{
		"status":      status,
		"lifecycle":   drv.Summary(),
		"node_count":  nodesIn(wr),
		"event_count": eventsIn(wr),
		"error":       errorMsg,
	})

	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "── walk summary ──")
	fmt.Fprintf(os.Stderr, "Status:     %s\n", status)
	fmt.Fprintf(os.Stderr, "Lifecycle:  %s\n", drv.Summary())
	if wr != nil {
		fmt.Fprintf(os.Stderr, "Nodes:      %d (%d events written)\n",
			nodesIn(wr), eventsIn(wr))
		if len(wr.Errors) > 0 {
			fmt.Fprintln(os.Stderr, "Per-node errors:")
			for k, v := range wr.Errors {
				fmt.Fprintf(os.Stderr, "  - %s: %s\n", k, v)
			}
		}
	}
	if errorMsg != "" {
		fmt.Fprintf(os.Stderr, "Error:      %s\n", errorMsg)
	}
}

func nodesIn(wr *runtime.WalkResult) int {
	if wr == nil {
		return 0
	}
	return len(wr.NodeIDs)
}
func eventsIn(wr *runtime.WalkResult) int {
	if wr == nil {
		return 0
	}
	return len(wr.EventURIs)
}

// withSignalCancel cancels ctx on SIGINT/SIGTERM so a Ctrl-C cleanly
// aborts the walk + tears down infra. Mirrors the cortex-shell + e2e
// harness behaviour.
func withSignalCancel(ctx context.Context) context.Context {
	derived, cancel := context.WithCancel(ctx)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-derived.Done():
		case <-sigCh:
			cancel()
		}
	}()
	return derived
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
