// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"matrix/cortex"
	"matrix/cortex/memory"
	"matrix/cortex/store"
	"matrix/executor/mcp"
	"matrix/executor/tool"
	"matrix/mcl/envelope"
	"matrix/mcl/ir"
	"matrix/mcl/llm"
)

// RunConfig holds the per-run parameters for one end-to-end execution.
type RunConfig struct {
	Tag          string // "A" | "B" | "C"
	RootDir      string // runs/<ts>
	SkillPath    string
	Prose        string
	Verb         string
	IntentID     string // pre-allocated for cross-run hash equality
	Model        string
	Provider     llm.Provider
	ProviderSet  bool
	Seed         int64
	ActorSeedHex string // 64-hex ed25519 seed
}

// RunReport carries everything the cross-run determinism check needs.
type RunReport struct {
	Tag              string
	IntentHash       string
	IntentJSON       []byte
	MtxDigest        string
	CompileLatencyMs int64
	PreReplayRoot    string
	PostReplayRoot   string
	Lifecycle        string
	WalkErrors       int
	WalkSucceeded    int
	AttestSeq        uint64
	AttestLearnSeq   uint64
	WeightsUpdated   bool
	WorkspaceRoot    string
	Skipped          bool
	SkipReason       string
}

// RunOnce executes a complete e2e cycle for one run config. Returns a
// RunReport even on partial failure so cross-run analysis can continue.
func RunOnce(ctx context.Context, cfg RunConfig, assert *AssertCtx) (*RunReport, error) {
	report := &RunReport{Tag: cfg.Tag}

	Section(fmt.Sprintf("RUN %s — model=%s seed=%d", cfg.Tag, cfg.Model, cfg.Seed))

	// ── Phase 1 — Setup ──
	Subsection("Phase 1/8 — workspace + cortex + embedder + MCP servers")
	ws, err := NewWorkspace(cfg.RootDir, cfg.Tag)
	if err != nil {
		return report, err
	}
	report.WorkspaceRoot = ws.Root

	t, err := NewTranscript(ws.Transcript, cfg.Tag, true)
	if err != nil {
		return report, err
	}
	defer t.Close()
	t.Event("run.start", "setup", map[string]interface{}{
		"model": cfg.Model, "seed": cfg.Seed, "prose": cfg.Prose, "verb": cfg.Verb,
	})

	actor, err := NewActorIdentity(cfg.ActorSeedHex, "e2e"+cfg.Tag)
	if err != nil {
		return report, err
	}
	t.Event("actor.identity", "setup", map[string]interface{}{
		"did":       actor.DID,
		"user_uri":  actor.UserURI,
		"agent_uri": actor.AgentURI,
		"pub":       hex.EncodeToString(actor.Public)[:16] + "…",
	})

	if err := SeedGitRepo(ws.GitRepoDir); err != nil {
		return report, fmt.Errorf("seed git repo: %w", err)
	}

	stderrSink, err := os.Create(ws.StderrLog)
	if err != nil {
		return report, err
	}
	defer stderrSink.Close()

	manifest := BuildAgentManifest(actor.DID, ws.WorkspaceFS, ws.GitRepoDir)
	manifestPath := filepath.Join(ws.Root, "agent-manifest.json")
	if err := PersistManifest(manifest, manifestPath); err != nil {
		return report, err
	}
	t.Event("manifest.persisted", "setup", map[string]interface{}{
		"path": manifestPath, "servers": len(manifest.Servers),
	})

	mgr := mcp.NewManager(mcp.ManagerParams{
		HealthInterval: 0,
		StderrSink:     stderrSink,
	})
	defer mgr.Close()

	for _, s := range manifest.Servers {
		// Inherit parent process env when manifest declares no overrides.
		// Empty slice in stdio.go is treated as "blank env" (no PATH, no
		// HOME) which kills npx/uvx; nil tells exec.Cmd to inherit.
		var subEnv []string
		if len(s.Env) > 0 {
			subEnv = append(append([]string{}, os.Environ()...), s.Env...)
		}
		spec := mcp.ServerSpec{
			Alias:         s.Alias,
			Transport:     s.Transport,
			Command:       s.Command,
			Args:          s.Args,
			Env:           subEnv,
			PackageDigest: s.PackageDigest,
			ExpectedTools: toolNames(s.Tools),
		}
		spawnCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		_, spawnErr := mgr.Spawn(spawnCtx, spec)
		cancel()
		assert.NoError(fmt.Sprintf("mcp.Manager.Spawn(%s)", s.Alias), spawnErr)
		if spawnErr != nil {
			t.Event("mcp.spawn.error", "setup", map[string]interface{}{
				"alias": s.Alias, "error": spawnErr.Error(),
			})
			report.SkipReason = fmt.Sprintf("MCP server %q failed to spawn: %v", s.Alias, spawnErr)
			report.Skipped = true
			return report, nil
		}
		t.Event("mcp.spawn.ok", "setup", map[string]interface{}{
			"alias": s.Alias, "tools": len(s.Tools), "version": s.Version,
		})
	}

	reg, err := tool.NewRegistry(tool.RegistryParams{
		Manifest: manifest,
		MCP:      mgr,
	})
	if err != nil {
		return report, err
	}
	t.Event("registry.built", "setup", map[string]interface{}{
		"tools": len(reg.List()),
	})

	// Cortex with deterministic clock + IDs.
	st, err := store.Open(ws.CortexRoot, "andrew", nil)
	if err != nil {
		return report, fmt.Errorf("store.Open: %w", err)
	}
	fixed := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	c := cortex.New(st,
		cortex.WithClock(FixedClock(fixed)),
		cortex.WithIDGen(SeededIDGen(0)),
	)
	// Ensure cleanup order is: embedder → store → MCP. Defers run LIFO so
	// stack them in reverse: store first (last to defer), embedder stop
	// last (first to defer). The embedder may already be stopped by Phase 8
	// replay; StopEmbedder is idempotent.
	defer st.Close()
	defer func() {
		if err := c.StopEmbedder(); err != nil {
			t.Event("embedder.stop.error", "cleanup", map[string]interface{}{"error": err.Error()})
		}
	}()
	t.Event("cortex.opened", "setup", map[string]interface{}{
		"root": ws.CortexRoot, "actor": "andrew",
	})

	// ── Phase 2 — Seed cortex ──
	Subsection("Phase 2/8 — seed cortex (Identity, Facts, Goal, Constraint, Pattern)")
	seedURIs, err := SeedCortex(c, actor.UserURI, t)
	if err != nil {
		return report, err
	}
	if _, err := MakeAndDrainEmbedder(c); err != nil {
		t.Event("embedder.error", "seed", map[string]interface{}{"error": err.Error()})
		report.SkipReason = fmt.Sprintf("embedder unavailable: %v", err)
		report.Skipped = true
		return report, nil
	}
	t.Event("cortex.embedder.drained", "seed", nil)
	root, _ := c.OverallRoot()
	t.Event("cortex.post-seed.root", "seed", map[string]interface{}{
		"overall_root": hex.EncodeToString(root[:]),
	})

	// Snapshot baseline.
	snap, err := c.Snapshot("e2e-post-seed")
	assert.NoError("cortex.Snapshot post-seed", err)
	if snap != nil {
		t.Event("cortex.snapshot.baseline", "seed", map[string]interface{}{
			"seq":          snap.JournalSeq,
			"overall_root": hex.EncodeToString(snap.OverallRoot[:]),
		})
	}

	// ── Phase 3 — Compile Intent ──
	Subsection("Phase 3/8 — compile NL → Intent IR (real Fireworks/Together LLM via bridge)")
	compileRes, err := RunCompile(ctx, c, CompileOpts{
		SkillPath:   cfg.SkillPath,
		Prose:       cfg.Prose,
		Verb:        cfg.Verb,
		Grammar:     "intent_frame@1",
		IntentID:    cfg.IntentID,
		Actor:       actor.UserURI,
		Agent:       actor.AgentURI,
		Model:       cfg.Model,
		Provider:    cfg.Provider,
		ProviderSet: cfg.ProviderSet,
		Seed:        cfg.Seed,
	}, t)
	if err != nil {
		t.Event("compile.error", "compile", map[string]interface{}{"error": err.Error()})
		assert.NoError("compile (real LLM)", err)
		report.SkipReason = fmt.Sprintf("compile failed: %v", err)
		report.Skipped = true
		return report, nil
	}
	report.IntentHash = compileRes.IntentHash
	report.IntentJSON = compileRes.IntentJSON
	report.MtxDigest = compileRes.MtxDigest
	report.CompileLatencyMs = compileRes.CompileLatencyMs

	assert.True("compile produced intent_hash", compileRes.IntentHash != "", "")
	assert.True("compile selected valid verb",
		ir.ValidVerb(compileRes.Intent.Frame.Verb),
		"verb="+compileRes.Intent.Frame.Verb)
	assert.True("compile produced ≥1 frame object OR ≥1 unknown",
		len(compileRes.Intent.Frame.Objects) > 0 || len(compileRes.Intent.Unknowns) > 0,
		fmt.Sprintf("objects=%d unknowns=%d", len(compileRes.Intent.Frame.Objects), len(compileRes.Intent.Unknowns)))
	assert.True("compile mtx_digest non-empty", compileRes.MtxDigest != "", "digest="+compileRes.MtxDigest[:16]+"…")

	// ── Phase 4 — Envelopes + Lifecycle (full surface) ──
	Subsection("Phase 4/8 — envelope sign + drive lifecycle (full surface)")
	stream, err := NewEnvelopeStream(ws.JournalDir, compileRes.IntentID, actor, t)
	if err != nil {
		return report, err
	}
	drv, err := NewLifecycleDriver(compileRes.IntentID, stream, assert, t)
	if err != nil {
		return report, err
	}

	// drafting → proposed
	if _, err := drv.DriveCompiled(compileRes.IntentJSON, compileRes.CompileLatencyMs); err != nil {
		return report, err
	}
	// proposed → clarifying
	clarifyEnv, err := drv.DriveClarify([]envelope.ClarifyQuestion{
		{
			UnknownID: "u_target_specificity",
			Field:     "frame.objects.target",
			Prompt:    "Should the checklist focus on internal cutover only, or also customer-facing comms?",
			Type:      "enum<internal|both>",
			Required:  false,
			Options:   []string{"internal", "both"},
			Default:   "both",
		},
	})
	if err != nil {
		return report, err
	}
	// clarifying → proposed
	answerPatch, _ := json.Marshal([]map[string]interface{}{
		{"op": "replace", "path": "/frame/objects/0/value", "value": "both"},
	})
	if _, err := drv.DriveAnswer(answerPatch, clarifyEnv.ID); err != nil {
		return report, err
	}
	// proposed → accepted
	if _, err := drv.DriveAccept(compileRes.IntentHash, false); err != nil {
		return report, err
	}

	// ── Phase 5 — PlanTree ──
	Subsection("Phase 5/8 — emit PlanTree (typed; ValidatePlan all 11 invariants)")
	plan, err := BuildPlan(compileRes.IntentID, actor.AgentURI, "matrix://skill/writing-plans@1.0.0", ws.WorkspaceFS, ws.GitRepoDir)
	if err != nil {
		return report, err
	}
	planJSON, err := ir.CanonicalJSONPlan(plan)
	if err != nil {
		return report, err
	}
	t.Event("plan.built", "plan", map[string]interface{}{
		"plan_id": plan.ID, "plan_hash": plan.Hash, "json_bytes": len(planJSON),
	})
	assert.True("ir.ValidatePlan accepts hand-built plan", plan.Hash != "", "hash="+plan.Hash[:16]+"…")

	// accepted → executing
	if _, err := drv.DrivePlanProposed(planJSON); err != nil {
		return report, err
	}

	// ── Phase 6 — Walk plan ──
	Subsection("Phase 6/8 — walk plan against real MCP servers (fs + fetch + git)")
	walkRes, err := WalkPlan(ctx, plan, reg, c, drv, actor.UserURI, t)
	if err != nil {
		t.Event("walk.error", "walk", map[string]interface{}{"error": err.Error()})
		assert.NoError("walk plan", err)
	}
	for nodeID, errMsg := range walkRes.Errors {
		assert.True("plan node "+nodeID+" succeeded", errMsg == "" && !walkRes.IsErrors[nodeID], errMsg)
	}
	report.WalkErrors = len(walkRes.Errors)
	report.WalkSucceeded = len(walkRes.NodeIDs) - len(walkRes.Errors)

	// Q2 full surface: non-material correction (executing → executing).
	t.Event("correction.non-material.start", "lifecycle", nil)
	if _, err := drv.DriveCorrectNonMaterial("typo_in_step_description",
		[]byte(`[{"op":"replace","path":"/root/children/0/description","value":"Filesystem roundtrip (fixed typo)"}]`)); err != nil {
		return report, err
	}
	// Q2 full surface: material correction (executing → accepted).
	// Per lifecycle.go MaterialTo lock, intent.correct (Material=true)
	// rewinds state to accepted; from there a fresh plan.proposed
	// re-enters executing. There is NO additional intent.accept envelope
	// emitted by the system — the original signed accept binding is still
	// the user's authorization, the correction patch is what's signed
	// fresh. So we go straight from accepted → executing via plan.proposed.
	t.Event("correction.material.start", "lifecycle", nil)
	if _, err := drv.DriveCorrectMaterial("budget_increase>10%",
		[]byte(`[{"op":"replace","path":"/budget/max_calls","value":50}]`)); err != nil {
		return report, err
	}
	if _, err := drv.DrivePlanProposed(planJSON); err != nil {
		return report, err
	}

	// ── Phase 7 — Attest + EMA ──
	Subsection("Phase 7/8 — cortex.Attest with cited memories + EMA weight learning")
	cited := make([]memory.URI, 0, len(walkRes.EventURIs)+len(seedURIs))
	cited = append(cited, walkRes.EventURIs...)
	// Cite at most 3 seed memories so EMA training signal is balanced.
	for i := 0; i < len(seedURIs) && i < 3; i++ {
		cited = append(cited, seedURIs[i])
	}
	t.Event("attest.start", "attest", map[string]interface{}{
		"cited_count": len(cited),
		"event_uris":  len(walkRes.EventURIs),
		"seed_uris":   3,
	})

	attestRes, attErr := c.Attest(cortex.AttestOpts{
		IntentID:  compileRes.IntentID,
		Outcome:   cortex.AttestOutcomeSuccess,
		Cited:     cited,
		CreatedBy: actor.AgentURI,
	})
	assert.NoError("cortex.Attest succeeded", attErr)
	if attErr == nil {
		report.AttestSeq = attestRes.Seq
		report.AttestLearnSeq = attestRes.LearnSeq
		report.WeightsUpdated = attestRes.WeightsUpdated
		t.Event("attest.complete", "attest", map[string]interface{}{
			"seq":             attestRes.Seq,
			"learn_seq":       attestRes.LearnSeq,
			"affected":        len(attestRes.AffectedIDs),
			"skipped":         len(attestRes.SkippedURIs),
			"weights_updated": attestRes.WeightsUpdated,
			"prev_w": fmt.Sprintf("R=%.3f A=%.3f C=%.3f D=%.3f V=%.3f",
				attestRes.PrevWeights.WR, attestRes.PrevWeights.WA, attestRes.PrevWeights.WC, attestRes.PrevWeights.WD, attestRes.PrevWeights.WV),
			"new_w": fmt.Sprintf("R=%.3f A=%.3f C=%.3f D=%.3f V=%.3f",
				attestRes.NewWeights.WR, attestRes.NewWeights.WA, attestRes.NewWeights.WC, attestRes.NewWeights.WD, attestRes.NewWeights.WV),
		})
		assert.True("KindAttest + KindLearnWeights at consecutive seqs",
			attestRes.LearnSeq == attestRes.Seq+1,
			fmt.Sprintf("attest=%d learn=%d", attestRes.Seq, attestRes.LearnSeq))
		assert.True("attest cited >= 1 memory",
			len(attestRes.AffectedIDs) >= 1,
			fmt.Sprintf("affected=%d", len(attestRes.AffectedIDs)))
	}

	// executing → completed via intent.attest envelope.
	citedStrings := make([]string, len(cited))
	for i, u := range cited {
		citedStrings[i] = string(u)
	}
	if _, err := drv.DriveAttest(citedStrings, []byte(`{"e2e":"complete"}`)); err != nil {
		return report, err
	}
	report.Lifecycle = drv.Summary()

	// ── Phase 8 — Replay byte-identical OverallRoot ──
	Subsection("Phase 8/8 — replay byte-identical OverallRoot (§13.4)")
	pre, post, err := VerifyReplayInvariant(c, t, assert)
	if err != nil {
		return report, err
	}
	report.PreReplayRoot = pre
	report.PostReplayRoot = post

	t.Event("run.complete", "summary", map[string]interface{}{
		"intent_hash": report.IntentHash,
		"lifecycle":   report.Lifecycle,
		"pre_root":    pre,
		"post_root":   post,
		"walk_errors": report.WalkErrors,
		"weights_upd": report.WeightsUpdated,
	})
	return report, nil
}

func toolNames(in []tool.ToolEntry) []string {
	out := make([]string, 0, len(in))
	for _, t := range in {
		out = append(out, t.Name)
	}
	return out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
