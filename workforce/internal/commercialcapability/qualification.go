package commercialcapability

import (
	"fmt"
	"time"

	"matrix/workforce/internal/contracts"
)

type WakeEvidence struct {
	WakeID          contracts.WakeID      `json:"wake_id"`
	SeatID          contracts.SeatID      `json:"seat_id"`
	ProcessID       string                `json:"process_id"`
	StartedAt       time.Time             `json:"started_at"`
	EndedAt         time.Time             `json:"ended_at"`
	RuntimeEvidence contracts.EvidenceRef `json:"runtime_evidence"`
}

func (value WakeEvidence) Validate() error {
	for _, token := range []struct{ name, value string }{
		{"wake_id", string(value.WakeID)}, {"seat_id", string(value.SeatID)},
		{"process_id", value.ProcessID},
	} {
		if err := validateToken(token.name, token.value); err != nil {
			return err
		}
	}
	if !validUTC(value.StartedAt) || !validUTC(value.EndedAt) || !value.EndedAt.After(value.StartedAt) {
		return fmt.Errorf("commercial capability: wake evidence interval is invalid")
	}
	if err := value.RuntimeEvidence.Validate(); err != nil {
		return fmt.Errorf("commercial capability: wake runtime evidence: %w", err)
	}
	if value.RuntimeEvidence.Kind != "fresh_process_runtime" ||
		value.RuntimeEvidence.ObservedAt.Before(value.StartedAt) ||
		value.RuntimeEvidence.ObservedAt.After(value.EndedAt) {
		return fmt.Errorf("commercial capability: wake does not carry fresh-process runtime evidence")
	}
	return nil
}

type QualificationEvidence struct {
	SchemaVersion     string                  `json:"schema_version"`
	RecordID          RecordID                `json:"record_id"`
	CheckpointID      CheckpointID            `json:"checkpoint_id"`
	AuthorWake        WakeEvidence            `json:"author_wake"`
	VerifierWake      WakeEvidence            `json:"verifier_wake"`
	SourceEvidence    []contracts.EvidenceRef `json:"source_evidence"`
	CommitReceipt     contracts.EvidenceRef   `json:"commit_receipt"`
	SimulationMode    bool                    `json:"simulation_mode"`
	ExternalEffectIDs []string                `json:"external_effect_ids"`
}

func (value QualificationEvidence) Validate() error {
	if value.SchemaVersion != SchemaVersion {
		return fmt.Errorf("commercial capability: qualification schema is invalid")
	}
	if err := validateToken("qualification record_id", string(value.RecordID)); err != nil {
		return err
	}
	if err := validateToken("qualification checkpoint_id", string(value.CheckpointID)); err != nil {
		return err
	}
	if err := value.AuthorWake.Validate(); err != nil {
		return err
	}
	if err := value.VerifierWake.Validate(); err != nil {
		return err
	}
	if value.AuthorWake.WakeID == value.VerifierWake.WakeID ||
		value.AuthorWake.ProcessID == value.VerifierWake.ProcessID ||
		value.AuthorWake.SeatID == value.VerifierWake.SeatID {
		return fmt.Errorf("commercial capability: qualification requires independent fresh wakes")
	}
	if len(value.SourceEvidence) == 0 || len(value.SourceEvidence) > 256 {
		return fmt.Errorf("commercial capability: qualification source evidence is outside bounds")
	}
	seen := make(map[contracts.EvidenceID]bool, len(value.SourceEvidence))
	for _, evidence := range value.SourceEvidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
		if seen[evidence.ID] {
			return fmt.Errorf("commercial capability: qualification source evidence is duplicated")
		}
		seen[evidence.ID] = true
	}
	if err := value.CommitReceipt.Validate(); err != nil {
		return fmt.Errorf("commercial capability: qualification commit receipt: %w", err)
	}
	if value.CommitReceipt.Kind != "commercial_record_commit" || value.SimulationMode ||
		len(value.ExternalEffectIDs) != 0 {
		return fmt.Errorf("commercial capability: qualification cannot use simulation or claim external effects")
	}
	return nil
}

type QualificationResult struct {
	SchemaVersion   string                `json:"schema_version"`
	RecordID        RecordID              `json:"record_id"`
	SkillID         contracts.SkillID     `json:"skill_id"`
	AuthorWakeID    contracts.WakeID      `json:"author_wake_id"`
	VerifierWakeID  contracts.WakeID      `json:"verifier_wake_id"`
	QualifiedAt     time.Time             `json:"qualified_at"`
	EvidenceDigest  contracts.ContentHash `json:"evidence_digest"`
	EffectAuthority string                `json:"effect_authority"`
}

type QualificationEnvelope struct {
	Evidence QualificationEvidence `json:"evidence"`
	Result   QualificationResult   `json:"result"`
}

func (value QualificationEnvelope) Validate() error {
	if err := value.Evidence.Validate(); err != nil {
		return err
	}
	if err := value.Result.Validate(); err != nil {
		return err
	}
	if value.Evidence.RecordID != value.Result.RecordID ||
		value.Evidence.AuthorWake.WakeID != value.Result.AuthorWakeID ||
		value.Evidence.VerifierWake.WakeID != value.Result.VerifierWakeID {
		return fmt.Errorf("commercial capability: qualification envelope bindings are invalid")
	}
	return nil
}

func (value QualificationResult) Validate() error {
	if value.SchemaVersion != SchemaVersion || value.EffectAuthority != "none" ||
		!validUTC(value.QualifiedAt) {
		return fmt.Errorf("commercial capability: qualification result is invalid")
	}
	for _, token := range []struct{ name, value string }{
		{"record_id", string(value.RecordID)}, {"skill_id", string(value.SkillID)},
		{"author_wake_id", string(value.AuthorWakeID)}, {"verifier_wake_id", string(value.VerifierWakeID)},
	} {
		if err := validateToken(token.name, token.value); err != nil {
			return err
		}
	}
	if value.AuthorWakeID == value.VerifierWakeID {
		return fmt.Errorf("commercial capability: qualification wake identities are not independent")
	}
	return value.EvidenceDigest.Validate()
}

func QualifyAt(
	record VerifiedRecord,
	checkpoint Checkpoint,
	evidence QualificationEvidence,
	now time.Time,
) (QualificationResult, error) {
	if err := record.ValidateAt(now); err != nil {
		return QualificationResult{}, err
	}
	if err := checkpoint.Validate(); err != nil {
		return QualificationResult{}, err
	}
	if err := evidence.Validate(); err != nil {
		return QualificationResult{}, err
	}
	body := record.Record.Body
	if evidence.RecordID != body.ID || evidence.CheckpointID != checkpoint.ID ||
		checkpoint.OrganizationID != body.OrganizationID || checkpoint.InitiativeID != body.InitiativeID ||
		checkpoint.SkillID != body.SkillID || checkpoint.RecordChainID != body.ChainID ||
		!contains(checkpoint.CompletedRecordIDs, body.ID) ||
		(checkpoint.Phase != PhaseReviewed && checkpoint.Phase != PhaseHandoffReady && checkpoint.Phase != PhaseClosed) ||
		evidence.AuthorWake.SeatID != body.AuthorSeatID ||
		evidence.VerifierWake.SeatID != record.Review.VerifierSeatID {
		return QualificationResult{}, fmt.Errorf("commercial capability: qualification evidence is not bound to the record and checkpoint")
	}
	available := make(map[contracts.ContentHash]bool, len(evidence.SourceEvidence))
	for _, item := range evidence.SourceEvidence {
		available[item.Hash] = true
	}
	for _, observation := range body.Observations {
		if !available[observation.Primary.Evidence.Hash] {
			return QualificationResult{}, fmt.Errorf("commercial capability: qualification omits primary authoritative evidence")
		}
		if observation.Reconciliation != nil && !available[observation.Reconciliation.Evidence.Hash] {
			return QualificationResult{}, fmt.Errorf("commercial capability: qualification omits reconciliation evidence")
		}
	}
	if body.Customer != nil && !available[body.Customer.Consent.Authority.Hash] {
		return QualificationResult{}, fmt.Errorf("commercial capability: qualification omits consent authority")
	}
	encoded, err := contracts.EncodeCanonical(&evidence)
	if err != nil {
		return QualificationResult{}, err
	}
	result := QualificationResult{
		SchemaVersion: SchemaVersion, RecordID: body.ID, SkillID: body.SkillID,
		AuthorWakeID: evidence.AuthorWake.WakeID, VerifierWakeID: evidence.VerifierWake.WakeID,
		QualifiedAt: now, EvidenceDigest: digestBytes(encoded), EffectAuthority: "none",
	}
	if err := result.Validate(); err != nil {
		return QualificationResult{}, err
	}
	return result, nil
}
