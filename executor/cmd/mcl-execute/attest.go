// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// attest.go produces the terminal envelope for a completed (or failed)
// walk. Two responsibilities:
//
//  1. Collect the cited cortex URIs from the WalkResult so the
//     intent.attest body's CitedURIs[] feeds cortex.Attest's salience
//     EMA loop (research/04 §8.3 + matrix.kvx phase 12).
//
//  2. Build a structured EvidenceJSON blob the executor can ship to
//     consumers (debugger UIs, replay harness, anchor tools).
//
// Both intent.attest (success) and intent.fail (typed reason) routes
// pass through here so the lifecycle terminus is consistent.

import (
	"context"
	"encoding/json"
	"fmt"

	"matrix/cortex"
	"matrix/cortex/memory"
	"matrix/executor/runtime"
	"matrix/mcl/envelope"
	"matrix/mcl/ir"
)

// attestEvidence is the structured payload baked into IntentAttestBody.EvidenceJSON.
// Extending this struct is a non-breaking change because EvidenceJSON is
// opaque bytes per body.go; only the executor + replay harness decode it.
type attestEvidence struct {
	IntentID      string                      `json:"intent_id"`
	IntentHash    string                      `json:"intent_hash"`
	PlanID        string                      `json:"plan_id"`
	PlanHash      string                      `json:"plan_hash"`
	NodeIDs       []string                    `json:"node_ids,omitempty"`
	EventURIs     []string                    `json:"event_uris,omitempty"`
	ToolDurations map[string]int64            `json:"tool_durations,omitempty"`
	IsErrors      map[string]bool             `json:"is_errors,omitempty"`
	StepCount     int                         `json:"step_count"`
	GateCount     int                         `json:"gate_count"`
	SubCount      int                         `json:"sub_count"`
	CorrectionLog []runtime.CorrectionOutcome `json:"correction_log,omitempty"`
	OverallRoot   string                      `json:"cortex_overall_root,omitempty"`
	LifecyclePath string                      `json:"lifecycle_path,omitempty"`
}

// citedURIs collects the cortex Event memory URIs the walker wrote.
// These feed both cortex.Attest (post-attest) and the IntentAttestBody.
// Includes any References pinned at compile time so the salience EMA
// captures the full read+write surface used during execution.
func citedURIs(walk *runtime.WalkResult, intent *ir.Intent) []string {
	seen := map[string]bool{}
	out := make([]string, 0)

	if walk != nil {
		for _, u := range walk.EventURIs {
			s := string(u)
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	if intent != nil {
		for _, r := range intent.References {
			if r.URI == "" || seen[r.URI] {
				continue
			}
			seen[r.URI] = true
			out = append(out, r.URI)
		}
	}
	return out
}

// buildAttestEvidence packages the walk outcome for IntentAttestBody.EvidenceJSON.
// Returns nil bytes on marshal failure (rare; structures are JSON-safe).
func buildAttestEvidence(intent *ir.Intent, plan *ir.PlanTree, walk *runtime.WalkResult, c *cortex.Cortex, drv *lifecycleDriver) []byte {
	ev := attestEvidence{
		IntentID:   safeIntentID(intent),
		IntentHash: safeIntentHash(intent),
		PlanID:     safePlanID(plan),
		PlanHash:   safePlanHash(plan),
	}
	if walk != nil {
		ev.NodeIDs = walk.NodeIDs
		ev.EventURIs = make([]string, 0, len(walk.EventURIs))
		for _, u := range walk.EventURIs {
			ev.EventURIs = append(ev.EventURIs, string(u))
		}
		ev.ToolDurations = walk.ToolDurations
		ev.IsErrors = walk.IsErrors
		ev.StepCount = len(walk.StepResults)
		ev.GateCount = len(walk.GateDecisions)
		ev.SubCount = len(walk.SubResults)
		ev.CorrectionLog = walk.Corrections
	}
	if c != nil {
		if root, err := c.OverallRoot(); err == nil {
			ev.OverallRoot = hexFromRoot(root[:])
		}
	}
	if drv != nil {
		ev.LifecyclePath = drv.Summary()
	}
	out, err := json.Marshal(ev)
	if err != nil {
		return nil
	}
	return out
}

// signTerminalAttest signs intent.attest, drives the lifecycle to
// completed, and triggers cortex.Attest so the salience EMA learns from
// the citation surface (Phase 12 lock — atomic with the journal entry).
func signTerminalAttest(ctx context.Context, drv *lifecycleDriver, c *cortex.Cortex, intent *ir.Intent, plan *ir.PlanTree, walk *runtime.WalkResult, t *transcript) (*envelope.Envelope, error) {
	cited := citedURIs(walk, intent)
	evidence := buildAttestEvidence(intent, plan, walk, c, drv)

	env, err := drv.DriveAttest(cited, evidence)
	if err != nil {
		return env, fmt.Errorf("attest: drive: %w", err)
	}

	t.Event("attest.envelope.signed", "attest", map[string]interface{}{
		"intent_id": safeIntentID(intent),
		"plan_id":   safePlanID(plan),
		"cited":     len(cited),
		"env_id":    env.ID,
	})

	if c != nil && len(cited) > 0 {
		_, aerr := c.Attest(cortex.AttestOpts{
			IntentID:  safeIntentID(intent),
			Outcome:   cortex.AttestOutcomeSuccess,
			Cited:     toMemoryURIs(cited),
			CreatedBy: drv.stream.actor.UserURI,
		})
		if aerr != nil {
			// cortex.Attest failure is non-fatal here: the envelope
			// is already on disk, the lifecycle is terminal, and the
			// salience EMA can be re-run from journal replay if
			// needed. Just log + surface it in the transcript.
			t.Event("cortex.attest.error", "attest", map[string]interface{}{
				"error": aerr.Error(),
			})
		} else {
			t.Event("cortex.attest.ok", "attest", map[string]interface{}{
				"cited": len(cited),
			})
		}
	}

	return env, nil
}

// signTerminalFail signs intent.fail, drives the lifecycle to failed,
// and triggers cortex.Attest with outcome=failure so the salience EMA
// can decrement Citations on cited URIs whose load-bearing role was
// the proximate cause of failure (research/04 §8.3 + Phase 11.5 lock).
func signTerminalFail(ctx context.Context, drv *lifecycleDriver, c *cortex.Cortex, intent *ir.Intent, plan *ir.PlanTree, walk *runtime.WalkResult, reason, message string, t *transcript) (*envelope.Envelope, error) {
	cited := citedURIs(walk, intent)
	evidence := buildAttestEvidence(intent, plan, walk, c, drv)

	env, err := drv.DriveFail(reason, message, evidence)
	if err != nil {
		return env, fmt.Errorf("fail: drive: %w", err)
	}

	t.Event("fail.envelope.signed", "attest", map[string]interface{}{
		"intent_id": safeIntentID(intent),
		"plan_id":   safePlanID(plan),
		"reason":    reason,
		"cited":     len(cited),
		"env_id":    env.ID,
	})

	if c != nil && len(cited) > 0 {
		_, aerr := c.Attest(cortex.AttestOpts{
			IntentID:  safeIntentID(intent),
			Outcome:   cortex.AttestOutcomeFailure,
			Reason:    reason,
			Cited:     toMemoryURIs(cited),
			CreatedBy: drv.stream.actor.UserURI,
		})
		if aerr != nil {
			t.Event("cortex.attest.error", "attest", map[string]interface{}{
				"error": aerr.Error(),
			})
		}
	}

	return env, nil
}

// --- helpers ---

func safeIntentID(i *ir.Intent) string {
	if i == nil {
		return ""
	}
	return i.ID
}
func safeIntentHash(i *ir.Intent) string {
	if i == nil {
		return ""
	}
	return i.Hash
}
func safePlanID(p *ir.PlanTree) string {
	if p == nil {
		return ""
	}
	return p.ID
}
func safePlanHash(p *ir.PlanTree) string {
	if p == nil {
		return ""
	}
	return p.Hash
}

// toMemoryURIs converts a []string to []memory.URI for cortex.Attest.
// Empty entries are silently dropped so a sparse cited list never
// reaches the cortex API as zero-value URIs.
func toMemoryURIs(in []string) []memory.URI {
	out := make([]memory.URI, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		out = append(out, memory.URI(s))
	}
	return out
}

// hexFromRoot encodes a 32-byte root array to lowercase hex without
// pulling encoding/hex into a single-use site (identity.go already
// owns sha256Hex; Cortex roots are produced as fixed [32]byte here).
func hexFromRoot(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, x := range b {
		out[i*2] = digits[x>>4]
		out[i*2+1] = digits[x&0x0f]
	}
	return string(out)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
