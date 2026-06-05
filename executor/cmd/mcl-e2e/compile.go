// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"matrix/bridge"
	"matrix/cortex"
	"matrix/mcl/ir"
	"matrix/mcl/llm"
	"matrix/mcl/mtx/canonical"
	"matrix/mcl/mtx/interpreter"
	"matrix/mcl/mtx/parser"
	"matrix/mcl/mtx/validator"
)

// CompileResult bundles everything produced by the MCL compile stage so
// downstream phases (envelope sign, plan emission, attest) can pin them
// without re-running the LLM.
type CompileResult struct {
	IntentID           string     // ULID of this intent
	Intent             *ir.Intent // canonical IR
	IntentJSON         []byte     // canonical JSON encoding
	IntentHash         string     // sha256 of canonical JSON
	MtxDigest          string     // SKILL.mtx canonical AST hash
	ModelDigest        string     // sha256(model_id)
	CortexSnapshotHash string     // cortex_snapshot_hash at compile entry
	FrameJSON          string     // raw LLM output
	PromptMessages     []interpreter.Message
	Slots              map[string]*interpreter.Slot
	Unknowns           []*interpreter.Unknown
	ClarifyQuestions   []*interpreter.ClarifyQuestion
	MatchedCondition   string
	CompileLatencyMs   int64
}

// CompileOpts configure a single compile run.
type CompileOpts struct {
	SkillPath   string
	Prose       string
	Verb        string // pre-classified to skip stage 2
	Grammar     string // grammar id; default intent_frame@1
	IntentID    string // pre-allocated for cross-run determinism
	Actor       string // matrix://user/<did>
	Agent       string // matrix://agent/<did>
	Model       string // overrides DefaultCompilerModel
	Provider    llm.Provider
	ProviderSet bool
	Seed        int64
}

// RunCompile invokes the MCL compiler against a live cortex via bridge,
// using a real LLM client. Mirrors bridge/cmd/mclc-cortex/main.go but
// returns structured results instead of writing JSON to stdout.
func RunCompile(ctx context.Context, c *cortex.Cortex, opts CompileOpts, t *Transcript) (*CompileResult, error) {
	t.Event("compile.start", "compile", map[string]interface{}{
		"skill": opts.SkillPath,
		"prose": opts.Prose,
		"verb":  opts.Verb,
		"model": opts.Model,
		"seed":  opts.Seed,
	})

	src, err := os.ReadFile(opts.SkillPath)
	if err != nil {
		return nil, fmt.Errorf("compile: read skill: %w", err)
	}
	file, perrs := parser.New(src).Parse()
	if len(perrs) > 0 {
		return nil, fmt.Errorf("compile: parse errors: %v", perrs)
	}
	if verrs := validator.ValidateSkill(file); len(verrs) > 0 {
		return nil, fmt.Errorf("compile: validate: %v", verrs)
	}
	mtxDigest := canonical.Hash(file)
	t.Event("compile.skill.parsed", "compile", map[string]interface{}{
		"mtx_digest": mtxDigest,
	})

	cfg := llm.DefaultCompilerModel()
	if opts.Model != "" {
		cfg.Model = opts.Model
	}
	if opts.ProviderSet {
		cfg.Provider = opts.Provider
		cfg.ProviderSet = true
	}
	cfg.Seed = opts.Seed
	llmClient, err := llm.New(&cfg)
	if err != nil {
		return nil, fmt.Errorf("compile: llm.New: %w", err)
	}

	adapter := bridge.New(c)

	rootBytes, err := c.OverallRoot()
	if err != nil {
		return nil, fmt.Errorf("compile: pre-OverallRoot: %w", err)
	}
	cortexSnapHash := hex.EncodeToString(rootBytes[:])

	interp := interpreter.New(file, llmClient, adapter)
	t0 := time.Now()
	runRes, err := interp.Run(ctx, &interpreter.RunInput{
		Prose:      opts.Prose,
		Verb:       opts.Verb,
		Grammar:    opts.Grammar,
		Confidence: 1.0,
		SlotValues: map[string]string{},
	})
	dur := time.Since(t0)
	if err != nil {
		return nil, fmt.Errorf("compile: interpret: %w", err)
	}

	t.Event("compile.llm.complete", "compile", map[string]interface{}{
		"matched":   runRes.MatchedCondition,
		"slots":     len(runRes.Slots),
		"unknowns":  len(runRes.Unknowns),
		"frame_len": len(runRes.FrameJSON),
		"ms":        dur.Milliseconds(),
	})

	intentID := opts.IntentID
	if intentID == "" {
		// Deterministic ULID-like 26-char id from prose+verb for run tagging.
		intentID = synthIntentID(opts.Prose, opts.Verb)
	}

	intent, err := buildIntentFromRun(intentID, opts, runRes, mtxDigest, cortexSnapHash, cfg.Model, dur)
	if err != nil {
		return nil, fmt.Errorf("compile: build intent: %w", err)
	}

	hash, err := ir.Hash(intent)
	if err != nil {
		return nil, fmt.Errorf("compile: intent hash: %w", err)
	}
	intent.Hash = hash

	// Re-canonicalise after Hash assignment (CanonicalJSON omits the field
	// only when self-clearing inside Hash; final canonical form has it set).
	canon, err := ir.CanonicalJSON(intent)
	if err != nil {
		return nil, fmt.Errorf("compile: canonical json (final): %w", err)
	}

	res := &CompileResult{
		IntentID:           intentID,
		Intent:             intent,
		IntentJSON:         canon,
		IntentHash:         hash,
		MtxDigest:          mtxDigest,
		ModelDigest:        sha256Hex(cfg.Model),
		CortexSnapshotHash: cortexSnapHash,
		FrameJSON:          runRes.FrameJSON,
		PromptMessages:     runRes.PromptMessages,
		Slots:              runRes.Slots,
		Unknowns:           runRes.Unknowns,
		ClarifyQuestions:   runRes.ClarifyQuestions,
		MatchedCondition:   runRes.MatchedCondition,
		CompileLatencyMs:   dur.Milliseconds(),
	}

	t.Event("compile.intent.hashed", "compile", map[string]interface{}{
		"intent_hash": hash,
		"intent_id":   intentID,
		"verb":        intent.Frame.Verb,
		"objects":     len(intent.Frame.Objects),
	})
	return res, nil
}

// buildIntentFromRun converts the interpreter RunResult into a typed
// ir.Intent. The LLM emits FrameJSON matching intent_frame@1 schema; we
// best-effort decode it and merge with slot/unknown state from the
// interpreter side.
func buildIntentFromRun(intentID string, opts CompileOpts, run *interpreter.RunResult, mtxDigest, cortexSnap, model string, dur time.Duration) (*ir.Intent, error) {
	intent := &ir.Intent{
		ID:         intentID,
		Version:    ir.StateDraft, // placeholder; State below
		Actor:      opts.Actor,
		Agent:      opts.Agent,
		Prose:      opts.Prose,
		State:      ir.StateProposed, // post-compile
		Confidence: 1.0,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		SignedBy:   opts.Actor,
		CompileMetadata: &ir.CompileMetadata{
			Seed:               compilerSeed(intentID, opts.Actor, cortexSnap, mtxDigest, model),
			MtxDigest:          mtxDigest,
			ModelDigest:        sha256Hex(model),
			ModelVersion:       model,
			Temperature:        0,
			Grammar:            opts.Grammar,
			SkillID:            "writing-plans",
			SkillVersion:       "1.0.0",
			CortexSnapshotHash: cortexSnap,
		},
	}
	intent.Version = "mcl/0.1"

	verb := opts.Verb
	if run.FrameJSON != "" {
		var frame map[string]interface{}
		if err := json.Unmarshal([]byte(run.FrameJSON), &frame); err == nil {
			if v, ok := frame["verb"].(string); ok && v != "" {
				verb = v
			}
			if objs, ok := frame["objects"].([]interface{}); ok {
				for _, o := range objs {
					m, ok := o.(map[string]interface{})
					if !ok {
						continue
					}
					se := ir.SlotEntry{
						Name: stringField(m, "name"),
						Type: stringField(m, "type"),
					}
					if u := stringField(m, "uri"); u != "" {
						se.URI = u
						se.Value = u
					} else if v := stringField(m, "value"); v != "" {
						se.Value = v
					} else if k := stringField(m, "kind"); k != "" {
						// Some grammars emit {kind, ref} pairs.
						se.Type = k
						se.Value = stringField(m, "ref")
					}
					intent.Frame.Objects = append(intent.Frame.Objects, se)
				}
			}
		}
	}
	if !ir.ValidVerb(verb) {
		// Fall back to the matched on-block's verb extracted from condition.
		if strings.HasPrefix(run.MatchedCondition, "verb=") {
			verb = strings.TrimPrefix(run.MatchedCondition, "verb=")
		}
	}
	intent.Frame.Verb = verb

	for _, u := range run.Unknowns {
		intent.Unknowns = append(intent.Unknowns, ir.Unknown{
			ID:        "u" + u.SlotName,
			Field:     "frame.objects." + u.SlotName,
			Type:      "ArtifactRef",
			Severity:  u.Severity,
			Rationale: u.Reason,
			Default:   u.Default,
		})
	}

	for _, slot := range run.Slots {
		if slot.Status == interpreter.SlotResolved && strings.HasPrefix(slot.Value, "matrix://") {
			intent.References = append(intent.References, ir.Reference{
				URI:     slot.Value,
				Type:    slot.TypeName,
				Role:    slot.Name,
				Summary: slot.RawProse,
			})
		}
	}

	return intent, nil
}

// compilerSeed mirrors D11: hash(intent_id || actor || snapshot_hash || mtx_digest || model_digest).
func compilerSeed(intentID, actor, snap, mtxDigest, model string) string {
	h := sha256.New()
	h.Write([]byte(intentID))
	h.Write([]byte{0x1f})
	h.Write([]byte(actor))
	h.Write([]byte{0x1f})
	h.Write([]byte(snap))
	h.Write([]byte{0x1f})
	h.Write([]byte(mtxDigest))
	h.Write([]byte{0x1f})
	h.Write([]byte(sha256Hex(model)))
	return hex.EncodeToString(h.Sum(nil))
}

// sha256Hex returns hex(sha256(s)).
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// synthIntentID derives a deterministic 26-char ULID-shaped ID from a
// canonical (prose, verb) tuple. NOT a real ULID — just stable across
// runs for cross-run hash equality.
func synthIntentID(prose, verb string) string {
	h := sha256.Sum256([]byte("intent|" + verb + "|" + prose))
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	out := make([]byte, 26)
	for i := 0; i < 26; i++ {
		out[i] = crockford[h[i]&0x1f]
	}
	return string(out)
}

func stringField(m map[string]interface{}, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
