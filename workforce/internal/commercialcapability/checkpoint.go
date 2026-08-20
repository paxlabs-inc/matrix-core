package commercialcapability

import (
	"fmt"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/skills"
)

type ExecutionPhase string

const (
	PhaseIntake        ExecutionPhase = "intake"
	PhaseObserving     ExecutionPhase = "observing"
	PhaseAnalyzing     ExecutionPhase = "analyzing"
	PhaseReviewPending ExecutionPhase = "review_pending"
	PhaseReviewed      ExecutionPhase = "reviewed"
	PhaseHandoffReady  ExecutionPhase = "handoff_ready"
	PhaseClosed        ExecutionPhase = "closed"
	PhaseBlocked       ExecutionPhase = "blocked"
)

func (value ExecutionPhase) Valid() bool {
	switch value {
	case PhaseIntake, PhaseObserving, PhaseAnalyzing, PhaseReviewPending,
		PhaseReviewed, PhaseHandoffReady, PhaseClosed, PhaseBlocked:
		return true
	default:
		return false
	}
}

type ResourceUsage struct {
	Duration    time.Duration `json:"duration"`
	ModelCalls  uint16        `json:"model_calls"`
	EffectCalls uint16        `json:"effect_calls"`
	CostMicros  uint64        `json:"cost_micros"`
	MemoryBytes uint64        `json:"memory_bytes"`
}

func (value ResourceUsage) Validate() error {
	if value.Duration < 0 || value.Duration > 2*time.Hour || value.MemoryBytes > 2<<30 {
		return fmt.Errorf("commercial capability: resource usage is outside bounds")
	}
	return nil
}

func (value ResourceUsage) Fits(limit skills.ResourceEstimate) bool {
	return value.Duration <= limit.MaxDuration && value.ModelCalls <= limit.ModelCalls &&
		value.EffectCalls <= limit.EffectCalls && value.CostMicros <= limit.CostMicros &&
		value.MemoryBytes <= limit.MemoryBytes
}

func (value ResourceUsage) Preserves(previous ResourceUsage) bool {
	return value.Duration >= previous.Duration && value.ModelCalls >= previous.ModelCalls &&
		value.EffectCalls >= previous.EffectCalls && value.CostMicros >= previous.CostMicros &&
		value.MemoryBytes >= previous.MemoryBytes
}

type SourceSnapshot struct {
	RootDigest   contracts.ContentHash `json:"root_digest"`
	PolicyDigest contracts.ContentHash `json:"policy_digest"`
	Generation   uint64                `json:"generation"`
	CapturedAt   time.Time             `json:"captured_at"`
	FreshUntil   time.Time             `json:"fresh_until"`
	Fresh        bool                  `json:"fresh"`
}

func (value SourceSnapshot) Validate() error {
	if err := value.RootDigest.Validate(); err != nil {
		return fmt.Errorf("commercial capability: checkpoint root digest: %w", err)
	}
	if err := value.PolicyDigest.Validate(); err != nil {
		return fmt.Errorf("commercial capability: checkpoint policy digest: %w", err)
	}
	if value.Generation == 0 || !validUTC(value.CapturedAt) || !validUTC(value.FreshUntil) ||
		!value.FreshUntil.After(value.CapturedAt) {
		return fmt.Errorf("commercial capability: checkpoint source snapshot is invalid")
	}
	return nil
}

type Checkpoint struct {
	SchemaVersion        string                   `json:"schema_version"`
	ID                   CheckpointID             `json:"checkpoint_id"`
	Version              uint64                   `json:"version"`
	OrganizationID       contracts.OrganizationID `json:"organization_id"`
	InitiativeID         InitiativeID             `json:"initiative_id"`
	WorkflowID           WorkflowID               `json:"workflow_id"`
	SkillID              contracts.SkillID        `json:"skill_id"`
	SkillVersion         uint64                   `json:"skill_version"`
	RecordChainID        ChainID                  `json:"record_chain_id"`
	CustomerBoundaryHash *contracts.ContentHash   `json:"customer_boundary_hash"`
	EconomicBoundaryHash *contracts.ContentHash   `json:"economic_boundary_hash"`
	Phase                ExecutionPhase           `json:"phase"`
	ResumePhase          *ExecutionPhase          `json:"resume_phase"`
	BlockedReason        string                   `json:"blocked_reason"`
	CompletedRecordIDs   []RecordID               `json:"completed_record_ids"`
	ObservationIDs       []ObservationID          `json:"observation_ids"`
	HandoffIDs           []HandoffID              `json:"handoff_ids"`
	ExternalEffectIDs    []string                 `json:"external_effect_ids"`
	ResourceLimit        skills.ResourceEstimate  `json:"resource_limit"`
	ResourceUsed         ResourceUsage            `json:"resource_used"`
	Source               SourceSnapshot           `json:"source"`
	IdempotencyKey       string                   `json:"idempotency_key"`
	UpdatedAt            time.Time                `json:"updated_at"`
}

func (value Checkpoint) Validate() error {
	if value.SchemaVersion != CheckpointSchemaVersion || value.Version == 0 ||
		value.SkillVersion != 1 || !value.Phase.Valid() {
		return fmt.Errorf("commercial capability: checkpoint schema, version, skill, or phase is invalid")
	}
	for _, token := range []struct{ name, value string }{
		{"checkpoint_id", string(value.ID)}, {"organization_id", string(value.OrganizationID)},
		{"initiative_id", string(value.InitiativeID)}, {"workflow_id", string(value.WorkflowID)},
		{"skill_id", string(value.SkillID)}, {"record_chain_id", string(value.RecordChainID)},
		{"idempotency_key", value.IdempotencyKey},
	} {
		if err := validateToken(token.name, token.value); err != nil {
			return err
		}
	}
	profile, err := Profile(value.SkillID)
	if err != nil {
		return err
	}
	if value.ResourceLimit != profile.Resources || !value.ResourceUsed.Fits(value.ResourceLimit) {
		return fmt.Errorf("commercial capability: checkpoint resource scope exceeds the signed skill")
	}
	if err := value.ResourceUsed.Validate(); err != nil {
		return err
	}
	for _, hash := range []*contracts.ContentHash{value.CustomerBoundaryHash, value.EconomicBoundaryHash} {
		if hash != nil {
			if err := hash.Validate(); err != nil {
				return err
			}
		}
	}
	if (profile.Domain == DomainSales || profile.Domain == DomainCustomerOperations) &&
		value.CustomerBoundaryHash == nil {
		return fmt.Errorf("commercial capability: customer workflow checkpoint lacks its customer boundary")
	}
	if (profile.Domain == DomainGrowth || profile.Domain == DomainPricing ||
		profile.Domain == DomainFinance || profile.Domain == DomainTreasury ||
		profile.Kind == RecordAcquisition) && value.EconomicBoundaryHash == nil {
		return fmt.Errorf("commercial capability: economic workflow checkpoint lacks its economic boundary")
	}
	if len(value.CompletedRecordIDs) > 64 || len(value.ObservationIDs) > 128 ||
		len(value.HandoffIDs) > 64 || hasDuplicate(value.CompletedRecordIDs) ||
		hasDuplicate(value.ObservationIDs) || hasDuplicate(value.HandoffIDs) {
		return fmt.Errorf("commercial capability: checkpoint completed identities are invalid")
	}
	for _, id := range value.CompletedRecordIDs {
		if err := validateToken("checkpoint record", string(id)); err != nil {
			return err
		}
	}
	for _, id := range value.ObservationIDs {
		if err := validateToken("checkpoint observation", string(id)); err != nil {
			return err
		}
	}
	for _, id := range value.HandoffIDs {
		if err := validateToken("checkpoint handoff", string(id)); err != nil {
			return err
		}
	}
	if len(value.ExternalEffectIDs) != 0 {
		return fmt.Errorf("commercial capability: analysis checkpoint cannot claim external effects")
	}
	if (value.Phase == PhaseReviewPending || value.Phase == PhaseReviewed ||
		value.Phase == PhaseHandoffReady || value.Phase == PhaseClosed) &&
		len(value.ObservationIDs) == 0 {
		return fmt.Errorf("commercial capability: review phases require authoritative observations")
	}
	if (value.Phase == PhaseReviewed || value.Phase == PhaseHandoffReady ||
		value.Phase == PhaseClosed) && len(value.CompletedRecordIDs) == 0 {
		return fmt.Errorf("commercial capability: reviewed phases require a completed verified record")
	}
	if value.Phase == PhaseHandoffReady && len(value.HandoffIDs) == 0 {
		return fmt.Errorf("commercial capability: handoff-ready phase requires a durable handoff")
	}
	if err := value.Source.Validate(); err != nil {
		return err
	}
	if value.Phase == PhaseBlocked {
		if value.ResumePhase == nil || !value.ResumePhase.Valid() || *value.ResumePhase == PhaseBlocked ||
			stringsTrimmedEmpty(value.BlockedReason) || len(value.BlockedReason) > 4096 {
			return fmt.Errorf("commercial capability: blocked checkpoint requires an exact resume phase and reason")
		}
	} else if value.ResumePhase != nil || value.BlockedReason != "" {
		return fmt.Errorf("commercial capability: non-blocked checkpoint cannot carry blocked state")
	}
	if !validUTC(value.UpdatedAt) || value.UpdatedAt.Before(value.Source.CapturedAt) {
		return fmt.Errorf("commercial capability: checkpoint update time is invalid")
	}
	return nil
}

func ValidateResume(previous, next Checkpoint) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if previous.OrganizationID != next.OrganizationID || previous.InitiativeID != next.InitiativeID ||
		previous.WorkflowID != next.WorkflowID || previous.SkillID != next.SkillID ||
		previous.SkillVersion != next.SkillVersion || previous.RecordChainID != next.RecordChainID ||
		previous.CustomerBoundaryHash == nil != (next.CustomerBoundaryHash == nil) ||
		previous.EconomicBoundaryHash == nil != (next.EconomicBoundaryHash == nil) ||
		previous.ResourceLimit != next.ResourceLimit || previous.Source.RootDigest != next.Source.RootDigest ||
		previous.Source.PolicyDigest != next.Source.PolicyDigest || previous.Source.Generation != next.Source.Generation ||
		previous.Source.CapturedAt != next.Source.CapturedAt || previous.Source.FreshUntil != next.Source.FreshUntil ||
		next.Version != previous.Version+1 || next.UpdatedAt.Before(previous.UpdatedAt) ||
		!next.ResourceUsed.Preserves(previous.ResourceUsed) {
		return fmt.Errorf("commercial capability: checkpoint resume changed immutable scope or regressed state")
	}
	if previous.CustomerBoundaryHash != nil && *previous.CustomerBoundaryHash != *next.CustomerBoundaryHash ||
		previous.EconomicBoundaryHash != nil && *previous.EconomicBoundaryHash != *next.EconomicBoundaryHash {
		return fmt.Errorf("commercial capability: checkpoint resume changed a policy boundary")
	}
	if !preserves(previous.CompletedRecordIDs, next.CompletedRecordIDs) ||
		!preserves(previous.ObservationIDs, next.ObservationIDs) ||
		!preserves(previous.HandoffIDs, next.HandoffIDs) {
		return fmt.Errorf("commercial capability: checkpoint resume lost committed work")
	}
	if !validPhaseAdvance(previous, next) {
		return fmt.Errorf("commercial capability: checkpoint phase transition is invalid")
	}
	return nil
}

func validPhaseAdvance(previous, next Checkpoint) bool {
	if previous.Phase == PhaseClosed {
		return false
	}
	if previous.Phase == PhaseBlocked {
		return previous.ResumePhase != nil && next.Phase == *previous.ResumePhase
	}
	if next.Phase == PhaseBlocked {
		return next.ResumePhase != nil && *next.ResumePhase == previous.Phase
	}
	switch previous.Phase {
	case PhaseIntake:
		return next.Phase == PhaseObserving
	case PhaseObserving:
		return next.Phase == PhaseAnalyzing
	case PhaseAnalyzing:
		return next.Phase == PhaseReviewPending
	case PhaseReviewPending:
		return next.Phase == PhaseReviewed
	case PhaseReviewed:
		return next.Phase == PhaseHandoffReady || next.Phase == PhaseClosed
	case PhaseHandoffReady:
		return next.Phase == PhaseClosed
	default:
		return false
	}
}

func preserves[T comparable](previous, next []T) bool {
	for _, value := range previous {
		if !contains(next, value) {
			return false
		}
	}
	return true
}

func stringsTrimmedEmpty(value string) bool {
	for _, char := range value {
		if char != ' ' && char != '\t' && char != '\n' && char != '\r' {
			return false
		}
	}
	return true
}
