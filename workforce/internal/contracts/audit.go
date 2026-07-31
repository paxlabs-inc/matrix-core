package contracts

import (
	"fmt"
	"strings"
	"time"
)

type VerificationProcedureRef struct {
	ID      string      `json:"procedure_id"`
	Version uint64      `json:"version"`
	Digest  ContentHash `json:"digest"`
}

func (value VerificationProcedureRef) Validate() error {
	if err := validateID("procedure_id", value.ID); err != nil {
		return err
	}
	if value.Version == 0 {
		return fmt.Errorf("verification procedure version must be positive")
	}
	return value.Digest.Validate()
}

type PredicateKind string

const (
	PredicateArtifactHash PredicateKind = "artifact_hash"
	PredicateEvidenceHash PredicateKind = "evidence_hash"
	PredicateApproval     PredicateKind = "approval_present"
	PredicateSemantic     PredicateKind = "semantic_review"
)

func (kind PredicateKind) Valid() bool {
	return kind == PredicateArtifactHash || kind == PredicateEvidenceHash ||
		kind == PredicateApproval || kind == PredicateSemantic
}

type VerificationPredicate struct {
	ID           string        `json:"predicate_id"`
	Kind         PredicateKind `json:"kind"`
	SubjectID    string        `json:"subject_id"`
	ExpectedHash *ContentHash  `json:"expected_hash"`
	Description  string        `json:"description"`
}

func (value VerificationPredicate) Validate() error {
	if err := validateID("predicate_id", value.ID); err != nil {
		return err
	}
	if !value.Kind.Valid() {
		return fmt.Errorf("invalid predicate kind %q", value.Kind)
	}
	if value.Kind != PredicateSemantic {
		if err := validateID("predicate subject_id", value.SubjectID); err != nil {
			return err
		}
	} else if value.SubjectID != "" {
		return fmt.Errorf("semantic predicate cannot bind a subject id")
	}
	switch value.Kind {
	case PredicateArtifactHash, PredicateEvidenceHash:
		if value.ExpectedHash == nil {
			return fmt.Errorf("hash predicate requires expected_hash")
		}
		if err := value.ExpectedHash.Validate(); err != nil {
			return err
		}
	default:
		if value.ExpectedHash != nil {
			return fmt.Errorf("non-hash predicate cannot bind expected_hash")
		}
	}
	if strings.TrimSpace(value.Description) == "" || len(value.Description) > 1024 {
		return fmt.Errorf("predicate description must contain 1 to 1024 bytes")
	}
	return nil
}

type AppealRecord struct {
	PriorVerdictID VerdictID      `json:"prior_verdict_id"`
	PriorOutcome   VerdictOutcome `json:"prior_outcome"`
	Grounds        ArtifactRef    `json:"grounds"`
	FiledAt        time.Time      `json:"filed_at"`
}

func (value AppealRecord) Validate() error {
	if err := validateID("prior_verdict_id", string(value.PriorVerdictID)); err != nil {
		return err
	}
	if value.PriorOutcome != VerdictFail && value.PriorOutcome != VerdictRequiresHuman {
		return fmt.Errorf("only fail or requires_human verdicts may be appealed")
	}
	if err := value.Grounds.Validate(); err != nil {
		return err
	}
	if !isUTC(value.FiledAt) {
		return fmt.Errorf("appeal filed_at must be UTC")
	}
	return nil
}

type VerdictPacket struct {
	SchemaVersion   string                   `json:"schema_version"`
	OrganizationID  OrganizationID           `json:"organization_id"`
	Intent          Intent                   `json:"intent"`
	ExecutingSeatID SeatID                   `json:"executing_seat_id"`
	AuditorSeatID   SeatID                   `json:"auditor_seat_id"`
	Procedure       VerificationProcedureRef `json:"procedure"`
	Predicates      []VerificationPredicate  `json:"predicates"`
	Skill           SkillRef                 `json:"skill"`
	VerifierDigest  ContentHash              `json:"verifier_digest"`
	Artifacts       []ArtifactRef            `json:"artifacts"`
	Observations    []EvidenceRef            `json:"observations"`
	Approvals       []ApprovalID             `json:"approvals"`
	Reconciliation  []ReconciliationLineage  `json:"reconciliation"`
	Model           ModelBinding             `json:"model"`
	MGS             MGSGenomeRef             `json:"mgs"`
	Runtime         RuntimeBinding           `json:"runtime"`
	Source          SourceState              `json:"source"`
	Appeal          *AppealRecord            `json:"appeal"`
	Developer       *DeveloperAuditEvidence  `json:"developer"`
}

func (packet VerdictPacket) Validate() error {
	if err := validateSchema(packet.SchemaVersion); err != nil {
		return err
	}
	if err := validateID("organization_id", string(packet.OrganizationID)); err != nil {
		return err
	}
	if err := packet.Intent.Validate(); err != nil {
		return err
	}
	if packet.Intent.OrganizationID != packet.OrganizationID {
		return fmt.Errorf("verdict intent organization does not match packet")
	}
	if err := validateID("executing_seat_id", string(packet.ExecutingSeatID)); err != nil {
		return err
	}
	if err := validateID("auditor_seat_id", string(packet.AuditorSeatID)); err != nil {
		return err
	}
	if packet.ExecutingSeatID == packet.AuditorSeatID ||
		packet.Intent.OwnerSeatID != packet.ExecutingSeatID {
		return fmt.Errorf("executing seat cannot audit or attest its own work")
	}
	if err := packet.Procedure.Validate(); err != nil {
		return err
	}
	if len(packet.Predicates) == 0 || len(packet.Predicates) > 128 {
		return fmt.Errorf("verdict packet requires 1 to 128 predicates")
	}
	for i := range packet.Predicates {
		if err := packet.Predicates[i].Validate(); err != nil {
			return fmt.Errorf("predicate %d: %w", i, err)
		}
	}
	if err := packet.Skill.Validate(); err != nil {
		return err
	}
	if err := packet.VerifierDigest.Validate(); err != nil {
		return err
	}
	for i := range packet.Artifacts {
		if err := packet.Artifacts[i].Validate(); err != nil {
			return err
		}
	}
	for i := range packet.Observations {
		if err := packet.Observations[i].Validate(); err != nil {
			return err
		}
	}
	for _, approval := range packet.Approvals {
		if err := validateID("approval_id", string(approval)); err != nil {
			return err
		}
	}
	for i := range packet.Reconciliation {
		if err := packet.Reconciliation[i].Validate(); err != nil {
			return err
		}
	}
	if err := packet.Model.Validate(); err != nil {
		return err
	}
	if err := packet.MGS.Validate(); err != nil {
		return err
	}
	if err := packet.Runtime.Validate(); err != nil {
		return err
	}
	if err := packet.Source.Validate(); err != nil {
		return err
	}
	if packet.Appeal != nil {
		if err := packet.Appeal.Validate(); err != nil {
			return err
		}
	}
	if packet.Developer != nil {
		if err := packet.Developer.Validate(); err != nil {
			return err
		}
		if packet.Developer.OrganizationID != packet.OrganizationID ||
			packet.Developer.SourceRoot != packet.Source.RootDigest ||
			packet.Developer.GraphGeneration != packet.Source.GraphGeneration {
			return fmt.Errorf("developer audit evidence does not match verdict source")
		}
	}
	return nil
}
