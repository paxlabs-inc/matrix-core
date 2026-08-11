// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"encoding/json"
	"fmt"
	"sort"
)

// VerifierKind describes the class of verification.
type VerifierKind string

const (
	VerifierArtifact  VerifierKind = "artifact"
	VerifierSemantic  VerifierKind = "semantic"
	VerifierFact      VerifierKind = "fact"
	VerifierFramework VerifierKind = "framework"
	VerifierPolicy    VerifierKind = "policy"
	VerifierVisual    VerifierKind = "visual"
	VerifierEffect    VerifierKind = "effect"
	VerifierDelivery  VerifierKind = "delivery"
)

// VerifierResult is the typed outcome of one verifier execution.
type VerifierResult struct {
	CriterionID  string       `json:"criterion_id"`
	VerifierKind VerifierKind `json:"verifier_kind"`
	Passed       bool         `json:"passed"`
	Evidence     string       `json:"evidence"`
	RepairState  string       `json:"repair_state,omitempty"`
}

// VerifierGraph maps acceptance criteria to executable verifiers.
// Per O1 req.9 ac.1: every measurable criterion compiles to verifiers.
type VerifierGraph struct {
	Verifiers []CriterionVerifier `json:"verifiers"`
}

// CriterionVerifier binds one acceptance criterion to its verifiers.
type CriterionVerifier struct {
	CriterionID string           `json:"criterion_id"`
	Statement   string           `json:"statement"`
	Kinds       []VerifierKind   `json:"kinds"`
	Results     []VerifierResult `json:"results,omitempty"`
	Closed      bool             `json:"closed"`
}

// CompileVerifiers creates a verifier graph from the task contract's criteria.
func CompileVerifiers(criteria []Criterion) VerifierGraph {
	var verifiers []CriterionVerifier
	for _, c := range criteria {
		kinds := inferVerifierKinds(c)
		verifiers = append(verifiers, CriterionVerifier{
			CriterionID: c.ID,
			Statement:   c.Statement,
			Kinds:       kinds,
		})
	}
	return VerifierGraph{Verifiers: verifiers}
}

func inferVerifierKinds(c Criterion) []VerifierKind {
	stmt := c.Statement
	var kinds []VerifierKind
	// Artifact checks for file/artifact integrity
	if containsAny(stmt, "file", "artifact", "output", "created", "written") {
		kinds = append(kinds, VerifierArtifact)
	}
	// Semantic checks
	if containsAny(stmt, "semantic", "meaning", "correct", "match", "request") {
		kinds = append(kinds, VerifierSemantic)
	}
	// Framework validation
	if containsAny(stmt, "framework", "build", "lint", "validate", "test") {
		kinds = append(kinds, VerifierFramework)
	}
	if containsAny(stmt, "fact", "source", "claim", "evidence") {
		kinds = append(kinds, VerifierFact)
	}
	// Policy checks
	if containsAny(stmt, "policy", "permission", "security", "privacy") {
		kinds = append(kinds, VerifierPolicy)
	}
	// Effect verification
	if containsAny(stmt, "effect", "state", "delivered", "persisted") {
		kinds = append(kinds, VerifierEffect)
	}
	// Delivery reachability
	if containsAny(stmt, "delivery", "surface", "reach", "user") {
		kinds = append(kinds, VerifierDelivery)
	}
	// Default: at minimum an artifact and semantic check
	if len(kinds) == 0 {
		kinds = []VerifierKind{VerifierArtifact, VerifierSemantic}
	}
	return kinds
}

// RecordResult records a verifier result and closes the criterion if all
// required verifier kinds have passed.
func (vg *VerifierGraph) RecordResult(r VerifierResult) {
	if r.CriterionID == "" || r.VerifierKind == "" || r.Evidence == "" {
		return
	}
	for i := range vg.Verifiers {
		if vg.Verifiers[i].CriterionID == r.CriterionID {
			vg.Verifiers[i].Results = append(vg.Verifiers[i].Results, r)
			// Check that every required kind has at least one passing result
			allKindsVerified := true
			for _, requiredKind := range vg.Verifiers[i].Kinds {
				kindPassed := false
				for _, res := range vg.Verifiers[i].Results {
					if res.VerifierKind == requiredKind && res.Passed {
						kindPassed = true
						break
					}
				}
				if !kindPassed {
					allKindsVerified = false
					break
				}
			}
			vg.Verifiers[i].Closed = allKindsVerified
			return
		}
	}
}

// AllClosed returns true when every criterion has passed all its verifiers.
func (vg VerifierGraph) AllClosed() bool {
	for _, v := range vg.Verifiers {
		if !v.Closed {
			return false
		}
	}
	return true
}

// OpenCriteria returns the IDs of criteria that are not yet closed.
func (vg VerifierGraph) OpenCriteria() []string {
	var out []string
	for _, v := range vg.Verifiers {
		if !v.Closed {
			out = append(out, v.CriterionID)
		}
	}
	return out
}

// ProofManifest is the durable record linking every criterion to its proof.
// Per O1 req.9 ac.5: completion requires every criterion closed, effects
// reconciled, artifact delivered, and a durable proof manifest.
type ProofManifest struct {
	RunID             string              `json:"run_id"`
	ContractID        string              `json:"contract_id"`
	AllCriteria       bool                `json:"all_criteria_closed"`
	EffectsReconciled bool                `json:"effects_reconciled"`
	Delivered         bool                `json:"delivered"`
	ProofHash         string              `json:"proof_hash"`
	Verifiers         []CriterionVerifier `json:"verifiers"`
}

// GenerateProofManifest produces the proof manifest from the verifier graph.
func (vg VerifierGraph) GenerateProofManifest(runID, contractID string, effectsReconciled, delivered bool) ProofManifest {
	pm := ProofManifest{
		RunID:             runID,
		ContractID:        contractID,
		AllCriteria:       vg.AllClosed(),
		EffectsReconciled: effectsReconciled,
		Delivered:         delivered,
		Verifiers:         vg.Verifiers,
	}
	b, _ := json.Marshal(pm)
	pm.ProofHash = ContentHash(b)
	return pm
}

// IsComplete returns true when all completion requirements are met.
func (pm ProofManifest) IsComplete() bool {
	return pm.Verify() == nil
}

// Verify proves the manifest is internally complete and hash-consistent.
// Closed booleans without verifier evidence cannot manufacture completion.
func (pm ProofManifest) Verify() error {
	if pm.RunID == "" || pm.ContractID == "" || !pm.AllCriteria || !pm.EffectsReconciled || !pm.Delivered {
		return fmt.Errorf("proof manifest is incomplete")
	}
	if len(pm.Verifiers) == 0 {
		return fmt.Errorf("proof manifest has no verifiers")
	}
	for _, verifier := range pm.Verifiers {
		if verifier.CriterionID == "" || !verifier.Closed || len(verifier.Kinds) == 0 {
			return fmt.Errorf("criterion %q is not verifiably closed", verifier.CriterionID)
		}
		for _, kind := range verifier.Kinds {
			passed := false
			for _, result := range verifier.Results {
				if result.VerifierKind == kind && result.Passed && result.Evidence != "" {
					passed = true
					break
				}
			}
			if !passed {
				return fmt.Errorf("criterion %s lacks evidence for verifier %s", verifier.CriterionID, kind)
			}
		}
	}
	want := pm.ProofHash
	pm.ProofHash = ""
	encoded, _ := json.Marshal(pm)
	if want == "" || ContentHash(encoded) != want {
		return fmt.Errorf("proof manifest hash mismatch")
	}
	return nil
}

// UnmetRequirements returns descriptions of what is not yet complete.
func (pm ProofManifest) UnmetRequirements() []string {
	var out []string
	if !pm.AllCriteria {
		out = append(out, "not all acceptance criteria are closed")
	}
	if !pm.EffectsReconciled {
		out = append(out, "effects are not reconciled")
	}
	if !pm.Delivered {
		out = append(out, "artifact is not delivered to the authorized surface")
	}
	sort.Strings(out)
	return out
}

// FmtUnmet returns a formatted string of unmet requirements.
func (pm ProofManifest) FmtUnmet() string {
	reqs := pm.UnmetRequirements()
	if len(reqs) == 0 {
		return ""
	}
	return fmt.Sprintf("proof manifest incomplete: %v", reqs)
}
