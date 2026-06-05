// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"matrix/bridge"
	"matrix/cortex"
	"matrix/executor/compilecache"
	"matrix/executor/runtime"
	"matrix/mcl/ir"
	"matrix/mcl/llm"
	"matrix/mcl/mtx/interpreter"
)

// defaultCompileConfidenceThreshold is the frame-confidence floor below
// which compile escalates to opts.EscalationModel (when set). 0.75 keeps
// the cheap compiler on the happy path while still catching genuinely
// uncertain extractions. Overridable per call via
// compileOpts.ConfidenceThreshold.
const defaultCompileConfidenceThreshold = 0.75

// compileOpts configures a single compile run.
type compileOpts struct {
	Skill      *runtime.LoadedSkill
	Prose      string
	Verb       string
	Grammar    string
	Actor      string // matrix://user/<did>
	Agent      string // matrix://agent/<did>
	IntentID   string
	Model      string
	BaseURL    string // optional override for the LLM endpoint (gateway / BYO swap)
	Seed       int64
	SlotValues map[string]string

	// Interactive controls the clarify loop. When true (default for `walk`
	// from a TTY), unmet blocking unknowns produce stdin prompts. When
	// false, blocking unknowns return ErrClarifyRequired with the
	// questions attached.
	Interactive bool
	Reader      io.Reader // default os.Stdin
	Writer      io.Writer // default os.Stderr
	MaxClarify  int       // default 3 — caller cap on clarify rounds

	// DisableCache, when true, skips the meta/compile_cache lookup +
	// write entirely (Session 31d · P4). Defaults to false (cache
	// enabled). Tests + the mcl-e2e legacy-router A/B harness flip
	// this to force every compile to hit the LLM.
	DisableCache bool

	// --- sess#32 ambient-architect MatrixGateway routing (plan §5.16) ---
	//
	// When GatewayURL is non-empty, the compiler LLM call is proxied
	// through the gateway and metered against the actor's daily PAX
	// budget. ActorDID + GoalID are stamped on the request as
	// X-Matrix-Actor-DID / X-Matrix-Goal-ID. CostHook captures the
	// gateway's response headers (X-Matrix-Cost-Pax + spend trailers)
	// for the daemon's cost telemetry surface (transcript +
	// /metrics). All four are optional; empty disables routing and
	// preserves the legacy direct-provider posture verbatim.
	GatewayURL string
	ActorDID   string
	GoalID     string
	CostHook   func(http.Header)

	// --- v1 launch (2026-06-01): compiler low-confidence escalation ---
	//
	// When EscalationModel is non-empty, compile re-invokes the compiler
	// slot ONCE with this stronger (whitelisted) model if the frame call
	// self-reports a confidence below ConfidenceThreshold OR emits an
	// invalid/empty verb. Empty EscalationModel disables escalation
	// (dev/CLI default). ForgeMode compiles never escalate — the opencode
	// brain is already frontier. The escalation call keeps SlotCompiler so
	// the gateway meters it under the compiler free-tier whitelist.
	EscalationModel     string
	ConfidenceThreshold float64

	// ForgeMode (sess#36 / Forge Phase 3) — when true, the compiler
	// resolves its slot from llm.ForgeRegistry (opencode.ai/zen + Matrix
	// identity preamble injection) instead of the legacy DefaultRegistry
	// (Fireworks). Set by the daemon when -forge-mode is on. Empty
	// preserves legacy posture.
	ForgeMode bool
}

// compileResult bundles everything produced by compile.
type compileResult struct {
	Intent           *ir.Intent
	IntentJSON       []byte
	IntentHash       string
	FrameJSON        string
	Slots            map[string]*interpreter.Slot
	Unknowns         []*interpreter.Unknown
	ClarifyQuestions []*interpreter.ClarifyQuestion
	MatchedCondition string
	LatencyMs        int64
	Rounds           int // how many compile rounds (1 + clarify-resolves)
}

// errClarifyRequired is returned by compile when there are unanswered
// blocking unknowns AND opts.Interactive is false. The caller can read
// .Questions off the *interpreter.RunResult attached to compileResult
// to render a CLI-side intent.clarify envelope.
type errClarifyRequired struct {
	Questions []*interpreter.ClarifyQuestion
}

func (e *errClarifyRequired) Error() string {
	return fmt.Sprintf("compile: %d blocking clarify question(s) unresolved", len(e.Questions))
}

// compile compiles prose → Intent via the compiler LLM, grounded in cortex
// through matrix/bridge. Mirrors bridge/cmd/mclc-cortex/main.go but
// returns a typed result + supports an interactive clarify loop.
//
// Production semantics:
//   - LLM is REQUIRED. compile returns an error if llm.New fails.
//   - The interpreter's resolve statements run against the live cortex,
//     so cortex.find / cortex.context / cortex.resolve produce real
//     References on the resulting Intent.
//   - Clarify loop: when interp.Run returns blocking unknowns,
//     compile prompts each on stdin (or returns errClarifyRequired in
//     non-interactive mode), accumulates answers into SlotValues,
//     re-runs interp.Run, repeats up to MaxClarify times.
//   - Intent metadata is fully populated: Hash, CompileMetadata.Seed
//     (D11), CompileMetadata.MtxDigest, ModelDigest, CortexSnapshotHash.
func compile(ctx context.Context, c *cortex.Cortex, opts compileOpts, t *transcript) (*compileResult, error) {
	if opts.Skill == nil {
		return nil, fmt.Errorf("compile: nil skill")
	}
	if opts.Prose == "" {
		return nil, fmt.Errorf("compile: empty prose")
	}
	if opts.Grammar == "" {
		opts.Grammar = "intent_frame@1"
	}
	if opts.Seed == 0 {
		opts.Seed = 42
	}
	if opts.SlotValues == nil {
		opts.SlotValues = map[string]string{}
	}
	if opts.Reader == nil {
		opts.Reader = os.Stdin
	}
	if opts.Writer == nil {
		opts.Writer = os.Stderr
	}
	if opts.MaxClarify == 0 {
		opts.MaxClarify = 3
	}
	if opts.ConfidenceThreshold <= 0 {
		opts.ConfidenceThreshold = defaultCompileConfidenceThreshold
	}
	if opts.IntentID == "" {
		opts.IntentID = synthIntentID(opts.Prose, opts.Verb)
	}

	t.Event("compile.start", "compile", map[string]interface{}{
		"skill_uri":   opts.Skill.URI,
		"skill_hash":  opts.Skill.CanonicalHash,
		"prose":       opts.Prose,
		"verb":        opts.Verb,
		"intent_id":   opts.IntentID,
		"interactive": opts.Interactive,
	})

	// Build compiler LLM (REQUIRED — no silent dry-run).
	// sess#36: ForgeMode swaps to llm.ForgeRegistry (opencode.ai/zen)
	// so self-maintenance compiles route through Claude Opus 4.7 + GPT
	// 5.5 with the Matrix identity preamble injected. Legacy posture
	// preserved when ForgeMode is false.
	var cfg llm.Config
	if opts.ForgeMode {
		cfg = llm.ForgeCompilerModel()
	} else {
		cfg = llm.DefaultCompilerModel()
	}
	if opts.Model != "" {
		cfg.Model = opts.Model
	}
	if opts.BaseURL != "" {
		// Endpoint suffix is the OpenAI-compat chat-completions path.
		// MatrixGateway / BYO Fireworks / BYO Together / vLLM-localhost
		// all expose /v1/chat/completions, so we append it
		// unconditionally and let the operator point -llm-base-url
		// (or the Tauri-shell wizard) at the host portion only.
		cfg.Endpoint = strings.TrimRight(opts.BaseURL, "/") + "/v1/chat/completions"
	}
	cfg.Seed = opts.Seed
	if opts.GatewayURL != "" {
		// Sess#32 ambient-architect MatrixGateway routing (plan §5.16).
		// Populated only when the daemon was booted with -gateway-url
		// (or env MATRIX_GATEWAY_URL). The compiler slot's free-tier
		// whitelist is enforced on the gateway side; this end stamps
		// the slot/actor metadata so the gateway can route + bill.
		cfg.GatewayURL = opts.GatewayURL
		cfg.ActorDID = opts.ActorDID
		cfg.IntentID = opts.IntentID
		cfg.GoalID = opts.GoalID
		cfg.SlotLabel = llm.SlotCompiler.String()
		cfg.OnResponseHeaders = opts.CostHook
	}
	modelDigest := sha256Hex(cfg.Model)

	// Router decision audit (sess#31d P4): emit BEFORE attempting the
	// cache lookup so the audit stream records intent even when the
	// cache short-circuits the LLM call. cache_hit is patched below
	// when the lookup actually fires.
	recordRouterDecision(t, routerDecision{
		Slot:     llm.SlotCompiler.String(),
		Model:    cfg.Model,
		IntentID: opts.IntentID,
		Reason:   "compiler.slot.resolve",
	})

	client, err := llm.New(&cfg)
	if err != nil {
		return nil, fmt.Errorf("compile: llm.New (compiler): %w", err)
	}

	// Bridge cortex into the interpreter so resolve statements ground.
	var adapter interpreter.Cortex
	if c != nil {
		adapter = bridge.New(c, bridge.WithDefaultLimit(10))
	} else {
		t.Event("compile.cortex.absent", "compile", map[string]interface{}{
			"note": "no cortex provided; resolve statements will return empty",
		})
	}

	snapHash, err := computeCortexSnapHash(c)
	if err != nil {
		return nil, err
	}

	// Compile-cache lookup (sess#31d P4). Sidecar at
	// meta/compile_cache/<sha256_hex> keyed on
	//   sha256(skill_digest || prose || cortex_snap_hash || verb || model_digest)
	// Cache MISS conditions (any one bypasses the lookup):
	//   - opts.DisableCache (legacy-router A/B path, tests)
	//   - cortex c is nil (no store to read from)
	//   - opts.SlotValues populated (interactive clarify re-entry can't
	//     hit a stable key; pre-supplied slot answers also imply the
	//     caller is iterating, not re-running, so don't return stale)
	//   - opts.Verb is empty (the cache key needs a stable verb;
	//     verb=="" would collapse all verbs to one cache bucket)
	cacheable := !opts.DisableCache && c != nil && len(opts.SlotValues) == 0 && opts.Verb != ""
	var cacheKey string
	if cacheable {
		cacheKey = compilecache.Key(
			opts.Skill.CanonicalHash, opts.Prose, snapHash, opts.Verb, modelDigest,
		)
		entry, ok, lerr := compilecache.Lookup(c.Store(), cacheKey)
		if lerr != nil {
			t.Event("compile.cache.error", "compile", map[string]interface{}{
				"key":   cacheKey,
				"error": lerr.Error(),
			})
		}
		if ok && entry != nil {
			t.Event("compile.cache.hit", "compile", map[string]interface{}{
				"intent_id":    opts.IntentID,
				"intent_hash":  entry.IntentHash,
				"model_digest": entry.ModelDigest,
				"key":          cacheKey,
				"cached_at":    entry.CachedAt,
			})
			if m := t.Metrics(); m != nil {
				m.IncCacheHit()
			}
			recordRouterDecision(t, routerDecision{
				Slot:     llm.SlotCompiler.String(),
				Model:    cfg.Model,
				IntentID: opts.IntentID,
				CacheHit: true,
				Reason:   "compile.cache.hit",
			})
			// Decode the cached canonical JSON into a fresh *ir.Intent
			// so callers receive the typed shape (lifecycle driver +
			// envelope sink both consume *ir.Intent, not just bytes).
			var intent ir.Intent
			if uerr := json.Unmarshal(entry.IntentJSON, &intent); uerr == nil {
				return &compileResult{
					Intent:     &intent,
					IntentJSON: entry.IntentJSON,
					IntentHash: entry.IntentHash,
					LatencyMs:  0, // cache hit; LLM was not invoked
					Rounds:     0, // no clarify rounds; pre-resolved
				}, nil
			}
			// Decode failure on a current-schema cached blob is a
			// pre-existing corruption we surface but do NOT propagate
			// — fall through to fresh compile so the caller still
			// gets a usable Intent.
			t.Event("compile.cache.decode_error", "compile", map[string]interface{}{
				"key": cacheKey,
			})
		} else {
			t.Event("compile.cache.miss", "compile", map[string]interface{}{
				"intent_id": opts.IntentID,
				"key":       cacheKey,
			})
			if m := t.Metrics(); m != nil {
				m.IncCacheMiss()
			}
		}
	}

	interp := interpreter.New(opts.Skill.File, client, adapter)

	// --- clarify loop ---
	var (
		res        *compileResult
		round      = 0
		totalMS    int64
		lastRunRes *interpreter.RunResult
		escalated  bool // compiler low-confidence escalation fires at most once
	)
	for round < opts.MaxClarify {
		round++
		t0 := time.Now()
		runRes, runErr := interp.Run(ctx, &interpreter.RunInput{
			Prose:      opts.Prose,
			Verb:       opts.Verb,
			Grammar:    opts.Grammar,
			Confidence: 1.0,
			SlotValues: opts.SlotValues,
		})
		dur := time.Since(t0)
		totalMS += dur.Milliseconds()
		// Histogram observation (sess#31d P4). Recorded regardless
		// of runErr so failures are visible in the per-route error
		// counter. Slot=compiler; kind is empty for non-executor
		// routes so all compiler rounds share one series.
		if m := t.Metrics(); m != nil {
			m.Observe(routeMetricKey{
				Slot:  llm.SlotCompiler.String(),
				Model: cfg.Model,
			}, dur.Milliseconds(), runErr)
		}
		if runErr != nil {
			return nil, fmt.Errorf("compile: interpret round %d: %w", round, runErr)
		}
		lastRunRes = runRes
		t.Event("compile.llm.complete", "compile", map[string]interface{}{
			"round":     round,
			"matched":   runRes.MatchedCondition,
			"slots":     len(runRes.Slots),
			"unknowns":  len(runRes.Unknowns),
			"questions": len(runRes.ClarifyQuestions),
			"frame_len": len(runRes.FrameJSON),
			"ms":        dur.Milliseconds(),
			"model":     cfg.Model,
		})

		// --- v1 launch: compiler low-confidence escalation -------------
		// intent_frame@1 now emits a self-assessed confidence. When the
		// cheap compiler is uncertain — confidence below threshold OR an
		// invalid/empty verb — re-invoke the compiler slot ONCE with the
		// stronger EscalationModel before assembling the intent or
		// surfacing clarify. Never in ForgeMode (already frontier), never
		// more than once per compile. The escalated call reuses the
		// SlotCompiler gateway metadata so the gateway meters it under the
		// compiler free-tier whitelist.
		if !escalated && opts.EscalationModel != "" && !opts.ForgeMode {
			conf := frameConfidence(runRes.FrameJSON)
			verb := frameVerb(runRes.FrameJSON, opts.Verb)
			if conf < opts.ConfidenceThreshold || !ir.ValidVerb(verb) {
				escalated = true
				ecfg := cfg
				ecfg.Model = opts.EscalationModel
				eclient, eerr := llm.New(&ecfg)
				if eerr != nil {
					t.Event("compile.escalate.error", "compile", map[string]interface{}{
						"intent_id": opts.IntentID,
						"to_model":  opts.EscalationModel,
						"error":     eerr.Error(),
					})
				} else {
					t.Event("compile.escalate", "compile", map[string]interface{}{
						"intent_id":  opts.IntentID,
						"from_model": cfg.Model,
						"to_model":   opts.EscalationModel,
						"confidence": conf,
						"verb":       verb,
						"reason":     escalateReason(verb),
					})
					recordRouterDecision(t, routerDecision{
						Slot:     llm.SlotCompiler.String(),
						Model:    ecfg.Model,
						IntentID: opts.IntentID,
						Reason:   "compile.escalate.low_confidence",
					})
					cfg.Model = ecfg.Model
					modelDigest = sha256Hex(cfg.Model)
					interp = interpreter.New(opts.Skill.File, eclient, adapter)
					t0 = time.Now()
					reRun, reErr := interp.Run(ctx, &interpreter.RunInput{
						Prose:      opts.Prose,
						Verb:       opts.Verb,
						Grammar:    opts.Grammar,
						Confidence: 1.0,
						SlotValues: opts.SlotValues,
					})
					dur = time.Since(t0)
					totalMS += dur.Milliseconds()
					if m := t.Metrics(); m != nil {
						m.Observe(routeMetricKey{
							Slot:  llm.SlotCompiler.String(),
							Model: cfg.Model,
						}, dur.Milliseconds(), reErr)
					}
					if reErr != nil {
						return nil, fmt.Errorf("compile: escalated interpret round %d: %w", round, reErr)
					}
					runRes = reRun
					lastRunRes = runRes
					t.Event("compile.llm.complete", "compile", map[string]interface{}{
						"round":     round,
						"escalated": true,
						"matched":   runRes.MatchedCondition,
						"unknowns":  len(runRes.Unknowns),
						"questions": len(runRes.ClarifyQuestions),
						"frame_len": len(runRes.FrameJSON),
						"ms":        dur.Milliseconds(),
						"model":     cfg.Model,
					})
				}
			}
		}

		blocking := blockingUnknowns(runRes.Unknowns)
		if len(blocking) == 0 || len(runRes.ClarifyQuestions) == 0 {
			// Done — assemble final intent.
			intent, ierr := buildIntent(opts, runRes, snapHash, cfg.Model)
			if ierr != nil {
				return nil, ierr
			}
			canon, herr := ir.CanonicalJSON(intent)
			if herr != nil {
				return nil, fmt.Errorf("compile: canonical json: %w", herr)
			}
			hash, herr := ir.Hash(intent)
			if herr != nil {
				return nil, fmt.Errorf("compile: hash intent: %w", herr)
			}
			intent.Hash = hash
			canon, herr = ir.CanonicalJSON(intent)
			if herr != nil {
				return nil, fmt.Errorf("compile: canonical json (final): %w", herr)
			}
			res = &compileResult{
				Intent:           intent,
				IntentJSON:       canon,
				IntentHash:       hash,
				FrameJSON:        runRes.FrameJSON,
				Slots:            runRes.Slots,
				Unknowns:         runRes.Unknowns,
				ClarifyQuestions: runRes.ClarifyQuestions,
				MatchedCondition: runRes.MatchedCondition,
				LatencyMs:        totalMS,
				Rounds:           round,
			}
			t.Event("compile.intent.hashed", "compile", map[string]interface{}{
				"intent_id":   intent.ID,
				"intent_hash": hash,
				"verb":        intent.Frame.Verb,
				"references":  len(intent.References),
				"rounds":      round,
			})

			// Compile-cache write (sess#31d P4). Only when cacheable
			// AND the LLM run was clean (no clarify questions left,
			// no error path) — partial intents are NOT cached so a
			// retry doesn't pick up stale half-done frames.
			if cacheable && cacheKey != "" {
				entry := &compilecache.Entry{
					SchemaVersion: compilecache.SchemaVersion,
					IntentJSON:    canon,
					IntentHash:    hash,
					ModelDigest:   modelDigest,
					Verb:          opts.Verb,
					SkillDigest:   opts.Skill.CanonicalHash,
					SnapHash:      snapHash,
				}
				if werr := compilecache.Store(c.Store(), cacheKey, entry); werr != nil {
					t.Event("compile.cache.write_error", "compile", map[string]interface{}{
						"key":   cacheKey,
						"error": werr.Error(),
					})
				} else {
					t.Event("compile.cache.write", "compile", map[string]interface{}{
						"intent_id":   intent.ID,
						"intent_hash": hash,
						"key":         cacheKey,
					})
				}
			}
			return res, nil
		}

		// Blocking unknowns present.
		if !opts.Interactive {
			return &compileResult{
				ClarifyQuestions: runRes.ClarifyQuestions,
				Unknowns:         runRes.Unknowns,
				Rounds:           round,
				LatencyMs:        totalMS,
				FrameJSON:        runRes.FrameJSON,
				Slots:            runRes.Slots,
				MatchedCondition: runRes.MatchedCondition,
			}, &errClarifyRequired{Questions: runRes.ClarifyQuestions}
		}

		// Interactive clarify: prompt + collect answers.
		t.Event("compile.clarify.prompt", "compile", map[string]interface{}{
			"round":     round,
			"questions": len(runRes.ClarifyQuestions),
		})
		if err := promptClarifyAnswers(opts.Reader, opts.Writer, runRes.ClarifyQuestions, opts.SlotValues); err != nil {
			return nil, fmt.Errorf("compile: clarify stdin: %w", err)
		}
	}

	// Out of rounds — surface the last attempt as a clarify-required error.
	return &compileResult{
		ClarifyQuestions: lastRunRes.ClarifyQuestions,
		Unknowns:         lastRunRes.Unknowns,
		Rounds:           round,
		LatencyMs:        totalMS,
	}, fmt.Errorf("compile: exhausted %d clarify rounds with unresolved blockers", opts.MaxClarify)
}

// blockingUnknowns filters to severity=="blocking".
func blockingUnknowns(in []*interpreter.Unknown) []*interpreter.Unknown {
	var out []*interpreter.Unknown
	for _, u := range in {
		if u.Severity == ir.SeverityBlocking {
			out = append(out, u)
		}
	}
	return out
}

// promptClarifyAnswers prints each ClarifyQuestion + reads one line per
// question from r, writing the answer into slotValues keyed by SlotName.
// Empty answers are skipped (caller may want to keep the unknown as
// soft/info; the loop terminates once no blocking unknowns remain).
func promptClarifyAnswers(r io.Reader, w io.Writer, qs []*interpreter.ClarifyQuestion, slotValues map[string]string) error {
	br := bufio.NewReader(r)
	fmt.Fprintln(w, "\n── Clarification needed ──")
	for _, q := range qs {
		fmt.Fprintf(w, "  %s [%s, %s]: %s\n", q.SlotName, q.TypeName,
			ternary(q.Required, "required", "optional"), q.Prompt)
		fmt.Fprintf(w, "  > ")
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		ans := strings.TrimSpace(line)
		if ans != "" {
			slotValues[q.SlotName] = ans
		}
	}
	return nil
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// buildIntent converts an interpreter RunResult into a typed *ir.Intent.
// The CompileMetadata fields are filled per D11.
func buildIntent(opts compileOpts, run *interpreter.RunResult, snapHash, modelID string) (*ir.Intent, error) {
	intent := &ir.Intent{
		ID:         opts.IntentID,
		Version:    "mcl/0.1",
		Actor:      opts.Actor,
		Agent:      opts.Agent,
		Prose:      opts.Prose,
		State:      ir.StateProposed,
		Confidence: 1.0,
		CreatedAt:  nowRFC3339(),
		SignedBy:   opts.Actor,
		CompileMetadata: &ir.CompileMetadata{
			Seed:               compilerSeed(opts.IntentID, opts.Actor, snapHash, opts.Skill.CanonicalHash, modelID),
			MtxDigest:          opts.Skill.CanonicalHash,
			ModelDigest:        sha256Hex(modelID),
			ModelVersion:       modelID,
			Temperature:        0,
			Grammar:            opts.Grammar,
			SkillID:            opts.Skill.ID,
			SkillVersion:       opts.Skill.Version,
			CortexSnapshotHash: snapHash,
		},
	}

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
						se.Type = k
						se.Value = stringField(m, "ref")
					}
					intent.Frame.Objects = append(intent.Frame.Objects, se)
				}
			}
		}
	}
	if !ir.ValidVerb(verb) {
		// Fall back to extracting verb= from the matched condition.
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

// compilerSeed mirrors D11: hash(intent_id || actor || snapshot_hash ||
// mtx_digest || model_digest), each field separated by 0x1f (US).
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

// computeCortexSnapHash returns the hex-encoded MMR root at compile entry,
// or 32 zero bytes if cortex is nil.
func computeCortexSnapHash(c *cortex.Cortex) (string, error) {
	if c == nil {
		return strings.Repeat("0", 64), nil
	}
	root, err := c.OverallRoot()
	if err != nil {
		return "", fmt.Errorf("compile: cortex OverallRoot: %w", err)
	}
	return hex.EncodeToString(root[:]), nil
}

// synthIntentID derives a deterministic 26-char ULID-shaped ID. Used when
// the caller doesn't supply IntentID, so repeated runs over the same
// (prose, verb) pair produce equal IntentIDs (cross-run audit equality).
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

// frameConfidence extracts the compiler's self-reported confidence from
// an intent_frame@1 JSON blob. Returns 1.0 when the field is absent or
// the blob is unparseable, so a provider that drops the (grammar-
// required) field never triggers a spurious escalation — the
// invalid-verb check still covers hard misses.
func frameConfidence(frameJSON string) float64 {
	if frameJSON == "" {
		return 1.0
	}
	var f struct {
		Confidence *float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(frameJSON), &f); err != nil || f.Confidence == nil {
		return 1.0
	}
	return *f.Confidence
}

// frameVerb extracts the verb from an intent_frame@1 blob, falling back
// to the caller-supplied verb when the frame omits or malforms it.
func frameVerb(frameJSON, fallback string) string {
	if frameJSON != "" {
		var f struct {
			Verb string `json:"verb"`
		}
		if err := json.Unmarshal([]byte(frameJSON), &f); err == nil && f.Verb != "" {
			return f.Verb
		}
	}
	return fallback
}

// escalateReason renders a short audit tag for why compile escalated:
// an out-of-vocab verb is a hard miss; otherwise it was low confidence.
func escalateReason(verb string) string {
	if !ir.ValidVerb(verb) {
		return "invalid_verb"
	}
	return "low_confidence"
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
