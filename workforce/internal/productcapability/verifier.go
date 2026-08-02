package productcapability

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"matrix/workforce/internal/contracts"
)

// VerifierProcedure is an immutable deterministic artifact predicate.
// Semantic acceptance still requires the separate signed Verification record.
type VerifierProcedure struct {
	SchemaVersion string                `json:"schema_version"`
	ID            string                `json:"procedure_id"`
	Version       uint64                `json:"version"`
	ArtifactKind  ArtifactKind          `json:"artifact_kind"`
	Checks        []string              `json:"checks"`
	Digest        contracts.ContentHash `json:"digest"`
}

// Validate enforces the exact versioned machine-checkable predicate identity.
func (value VerifierProcedure) Validate() error {
	if value.SchemaVersion != SchemaVersion || value.Version == 0 || !value.ArtifactKind.Valid() {
		return fmt.Errorf("product capability: verifier procedure identity is invalid")
	}
	if err := validateToken("verifier procedure id", value.ID); err != nil {
		return err
	}
	if err := validateTokenSet("verifier check", value.Checks, 1, 32); err != nil {
		return err
	}
	expected, err := value.computeDigest()
	if err != nil {
		return err
	}
	if value.Digest != expected {
		return fmt.Errorf("product capability: verifier procedure digest is invalid")
	}
	return nil
}

// NewVerifierProcedure creates the exact v1 deterministic predicate for a skill.
func NewVerifierProcedure(id string, kind ArtifactKind, checks []string) (VerifierProcedure, error) {
	value := VerifierProcedure{
		SchemaVersion: SchemaVersion, ID: id, Version: 1,
		ArtifactKind: kind, Checks: append([]string(nil), checks...),
	}
	digest, err := value.computeDigest()
	if err != nil {
		return VerifierProcedure{}, err
	}
	value.Digest = digest
	return value, value.Validate()
}

// RecordVerifierProcedure returns the exact aggregate verifier required for a
// durable record kind. Store.Commit rejects any substituted procedure digest.
func RecordVerifierProcedure(body RecordBody) (VerifierProcedure, error) {
	var kind ArtifactKind
	switch body.Kind {
	case RecordProductDesignHandoff:
		kind = ArtifactDesignHandoff
	case RecordEngineeringResult:
		kind = ArtifactIndependentReview
	case RecordMetricDefinition:
		kind = ArtifactProductAnalytics
	case RecordReliabilityIncident:
		kind = ArtifactIncidentEvidence
	default:
		return VerifierProcedure{}, fmt.Errorf("product capability: record kind has no verifier")
	}
	return NewVerifierProcedure(
		"verify.record."+string(body.Kind), kind,
		[]string{
			"content_addressed", "evidence_present", "fresh",
			"scope_bound", "independent_review_required",
		},
	)
}

// VerifyArtifact executes the deterministic checks owned by this release.
// It does not replace semantic, security, legal, or business Auditor review.
func (value VerifierProcedure) VerifyArtifact(artifact Artifact, now time.Time) error {
	if err := value.Validate(); err != nil {
		return err
	}
	if err := artifact.ValidateAt(now); err != nil {
		return err
	}
	if artifact.Kind != value.ArtifactKind {
		return fmt.Errorf("product capability: verifier targets another artifact kind")
	}
	for _, check := range value.Checks {
		switch check {
		case "content_addressed":
			if err := artifact.Artifact.Hash.Validate(); err != nil {
				return err
			}
		case "evidence_present":
			if len(artifact.Evidence) == 0 {
				return fmt.Errorf("product capability: verifier requires evidence")
			}
		case "fresh":
			if !artifact.FreshUntil.After(now) {
				return ErrExpired
			}
		case "scope_bound":
			if artifact.OrganizationID == "" || artifact.InitiativeID == "" ||
				artifact.ProjectID == "" || artifact.WorkspaceID == "" {
				return fmt.Errorf("product capability: verifier requires exact scope")
			}
		case "source_fresh":
			if artifact.Source == nil || !artifact.Source.Fresh {
				return fmt.Errorf("product capability: verifier requires fresh source")
			}
		case "independent_review_required":
			return nil
		default:
			return fmt.Errorf("product capability: verifier check %q is unsupported", check)
		}
	}
	return nil
}

func (value VerifierProcedure) computeDigest() (contracts.ContentHash, error) {
	payloadValue := verifierDigestPayload{
		SchemaVersion: value.SchemaVersion, ID: value.ID, Version: value.Version,
		ArtifactKind: value.ArtifactKind, Checks: append([]string(nil), value.Checks...),
	}
	if err := payloadValue.Validate(); err != nil {
		return contracts.ContentHash{}, fmt.Errorf("product capability: cannot digest invalid verifier procedure")
	}
	for _, check := range payloadValue.Checks {
		if !slices.Contains([]string{
			"content_addressed", "evidence_present", "fresh", "scope_bound",
			"source_fresh", "independent_review_required",
		}, check) {
			return contracts.ContentHash{}, fmt.Errorf("product capability: unsupported verifier check")
		}
	}
	payload, err := contracts.EncodeCanonical(payloadValue)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(payload)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

type verifierDigestPayload struct {
	SchemaVersion string       `json:"schema_version"`
	ID            string       `json:"procedure_id"`
	Version       uint64       `json:"version"`
	ArtifactKind  ArtifactKind `json:"artifact_kind"`
	Checks        []string     `json:"checks"`
}

func (value verifierDigestPayload) Validate() error {
	if value.SchemaVersion != SchemaVersion || value.Version == 0 || !value.ArtifactKind.Valid() {
		return fmt.Errorf("product capability: verifier digest payload is invalid")
	}
	if err := validateToken("verifier procedure id", value.ID); err != nil {
		return err
	}
	return validateTokenSet("verifier check", value.Checks, 1, 32)
}
