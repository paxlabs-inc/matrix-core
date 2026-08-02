package productcapability

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"matrix/workforce/internal/companylifecycle"
	"matrix/workforce/internal/companystate"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/developer"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/projectbrain"
)

const (
	// SchemaVersion is the canonical product-capability record schema.
	SchemaVersion = "workforce.product-capability.v1"
	// CheckpointSchemaVersion is the restart checkpoint schema.
	CheckpointSchemaVersion = "workforce.product-capability-checkpoint.v1"
)

// RecordID identifies one immutable capability record.
type RecordID string

// ChainID identifies one version chain without conflating successor records.
type ChainID string

// HandoffID identifies one Product and Design handoff to Developer.
type HandoffID string

// CheckpointID identifies one restart-safe initiative execution checkpoint.
type CheckpointID string

// InitiativeID identifies the initiative owning product work.
type InitiativeID string

// RecordKind is the closed durable product-capability record set.
type RecordKind string

const (
	// RecordProductDesignHandoff contains verified Product and Design inputs.
	RecordProductDesignHandoff RecordKind = "product_design_handoff"
	// RecordEngineeringResult contains source-through-customer execution evidence.
	RecordEngineeringResult RecordKind = "engineering_result"
	// RecordMetricDefinition contains one comparable analytics contract.
	RecordMetricDefinition RecordKind = "metric_definition"
	// RecordReliabilityIncident contains one operational incident lifecycle.
	RecordReliabilityIncident RecordKind = "reliability_incident"
)

// Valid reports whether the record kind is executable by this schema.
func (value RecordKind) Valid() bool {
	switch value {
	case RecordProductDesignHandoff, RecordEngineeringResult,
		RecordMetricDefinition, RecordReliabilityIncident:
		return true
	default:
		return false
	}
}

// ArtifactKind is the closed set of typed product execution evidence.
type ArtifactKind string

const (
	ArtifactCustomerProblem           ArtifactKind = "customer_problem"
	ArtifactValueProposition          ArtifactKind = "value_proposition"
	ArtifactRequirements              ArtifactKind = "requirements"
	ArtifactRoadmap                   ArtifactKind = "roadmap"
	ArtifactPriorityDecision          ArtifactKind = "priority_decision"
	ArtifactUserResearch              ArtifactKind = "user_research"
	ArtifactProductAnalytics          ArtifactKind = "product_analytics"
	ArtifactCustomerOutcomeAcceptance ArtifactKind = "customer_outcome_acceptance"
	ArtifactUserJourney               ArtifactKind = "user_journey"
	ArtifactInteractionModel          ArtifactKind = "interaction_model"
	ArtifactPrototype                 ArtifactKind = "prototype"
	ArtifactUsabilityEvidence         ArtifactKind = "usability_evidence"
	ArtifactAccessibilityEvidence     ArtifactKind = "accessibility_evidence"
	ArtifactDesignSystemDecision      ArtifactKind = "design_system_decision"
	ArtifactDesignHandoff             ArtifactKind = "design_handoff"
	ArtifactImplementationPlan        ArtifactKind = "implementation_plan"
	ArtifactSourceState               ArtifactKind = "source_state"
	ArtifactBuildEvidence             ArtifactKind = "build_evidence"
	ArtifactTestEvidence              ArtifactKind = "test_evidence"
	ArtifactReviewEvidence            ArtifactKind = "review_evidence"
	ArtifactQualityEvidence           ArtifactKind = "quality_evidence"
	ArtifactSecurityEvidence          ArtifactKind = "security_evidence"
	ArtifactReleasePlan               ArtifactKind = "release_plan"
	ArtifactDeploymentState           ArtifactKind = "deployment_state"
	ArtifactRollbackEvidence          ArtifactKind = "rollback_evidence"
	ArtifactHealthEvidence            ArtifactKind = "health_evidence"
	ArtifactIncidentEvidence          ArtifactKind = "incident_evidence"
	ArtifactCapacityEvidence          ArtifactKind = "capacity_evidence"
	ArtifactObservabilityEvidence     ArtifactKind = "observability_evidence"
	ArtifactReliabilityEvidence       ArtifactKind = "reliability_evidence"
	ArtifactOperationsReadiness       ArtifactKind = "operations_readiness"
	ArtifactLegalEvidence             ArtifactKind = "legal_evidence"
	ArtifactPricingEvidence           ArtifactKind = "pricing_evidence"
	ArtifactClaimsEvidence            ArtifactKind = "claims_evidence"
	ArtifactLaunchReadiness           ArtifactKind = "launch_readiness"
	ArtifactTelemetryEvidence         ArtifactKind = "telemetry_evidence"
	ArtifactCustomerEvidence          ArtifactKind = "customer_evidence"
	ArtifactIndependentReview         ArtifactKind = "independent_review"
)

var allArtifactKinds = []ArtifactKind{
	ArtifactCustomerProblem, ArtifactValueProposition, ArtifactRequirements,
	ArtifactRoadmap, ArtifactPriorityDecision, ArtifactUserResearch,
	ArtifactProductAnalytics, ArtifactCustomerOutcomeAcceptance,
	ArtifactUserJourney, ArtifactInteractionModel, ArtifactPrototype,
	ArtifactUsabilityEvidence, ArtifactAccessibilityEvidence,
	ArtifactDesignSystemDecision, ArtifactDesignHandoff,
	ArtifactImplementationPlan, ArtifactSourceState, ArtifactBuildEvidence,
	ArtifactTestEvidence, ArtifactReviewEvidence, ArtifactQualityEvidence,
	ArtifactSecurityEvidence, ArtifactReleasePlan, ArtifactDeploymentState,
	ArtifactRollbackEvidence, ArtifactHealthEvidence, ArtifactIncidentEvidence,
	ArtifactCapacityEvidence, ArtifactObservabilityEvidence,
	ArtifactReliabilityEvidence, ArtifactOperationsReadiness,
	ArtifactLegalEvidence, ArtifactPricingEvidence, ArtifactClaimsEvidence,
	ArtifactLaunchReadiness, ArtifactTelemetryEvidence,
	ArtifactCustomerEvidence, ArtifactIndependentReview,
}

// AllArtifactKinds returns an isolated copy of the canonical artifact registry.
func AllArtifactKinds() []ArtifactKind {
	return append([]ArtifactKind(nil), allArtifactKinds...)
}

// Valid reports whether the artifact kind is part of the closed registry.
func (value ArtifactKind) Valid() bool {
	return slices.Contains(allArtifactKinds, value)
}

// Artifact is one content-addressed, evidence-backed product execution output.
// Source is required for engineering and deployment evidence and absent for
// business records whose authority comes from their evidence references.
type Artifact struct {
	SchemaVersion  string                      `json:"schema_version"`
	ID             string                      `json:"artifact_record_id"`
	Kind           ArtifactKind                `json:"kind"`
	OrganizationID contracts.OrganizationID    `json:"organization_id"`
	InitiativeID   InitiativeID                `json:"initiative_id"`
	ProjectID      contracts.ProjectID         `json:"project_id"`
	WorkspaceID    contracts.WorkspaceID       `json:"workspace_id"`
	AuthorSeatID   contracts.SeatID            `json:"author_seat_id"`
	Summary        string                      `json:"summary"`
	Artifact       contracts.ArtifactRef       `json:"artifact"`
	Evidence       []contracts.EvidenceRef     `json:"evidence"`
	DataScopes     []string                    `json:"data_scopes"`
	Source         *projectbrain.GraphSnapshot `json:"source"`
	ObservedAt     time.Time                   `json:"observed_at"`
	EffectiveAt    time.Time                   `json:"effective_at"`
	FreshUntil     time.Time                   `json:"fresh_until"`
}

// ValidateAt rejects ungrounded, stale, cross-scope, or unbounded artifacts.
func (value Artifact) ValidateAt(now time.Time) error {
	if value.SchemaVersion != SchemaVersion || !value.Kind.Valid() {
		return fmt.Errorf("product capability: artifact schema or kind is invalid")
	}
	for name, tokenValue := range map[string]string{
		"artifact_record_id": value.ID,
		"organization_id":    string(value.OrganizationID),
		"initiative_id":      string(value.InitiativeID),
		"project_id":         string(value.ProjectID),
		"workspace_id":       string(value.WorkspaceID),
		"author_seat_id":     string(value.AuthorSeatID),
	} {
		if err := validateToken(name, tokenValue); err != nil {
			return err
		}
	}
	if strings.TrimSpace(value.Summary) == "" || len(value.Summary) > 4096 {
		return fmt.Errorf("product capability: artifact summary must contain 1 to 4096 bytes")
	}
	if err := value.Artifact.Validate(); err != nil {
		return fmt.Errorf("product capability: artifact body: %w", err)
	}
	if len(value.Evidence) == 0 || len(value.Evidence) > 128 {
		return fmt.Errorf("product capability: artifact requires 1 to 128 evidence references")
	}
	seenEvidence := make(map[contracts.EvidenceID]bool, len(value.Evidence))
	for _, evidence := range value.Evidence {
		if err := evidence.Validate(); err != nil {
			return fmt.Errorf("product capability: artifact evidence: %w", err)
		}
		if seenEvidence[evidence.ID] {
			return fmt.Errorf("product capability: artifact evidence is duplicated")
		}
		if evidence.ObservedAt.After(value.ObservedAt) {
			return fmt.Errorf("product capability: artifact evidence is from the future")
		}
		seenEvidence[evidence.ID] = true
	}
	if err := validateTokenSet("data scope", value.DataScopes, 1, 32); err != nil {
		return err
	}
	if !validUTC(value.ObservedAt) || !validUTC(value.EffectiveAt) ||
		!validUTC(value.FreshUntil) || value.EffectiveAt.Before(value.ObservedAt) ||
		!value.FreshUntil.After(value.EffectiveAt) || !validUTC(now) ||
		value.EffectiveAt.After(now) ||
		!value.FreshUntil.After(now) {
		return fmt.Errorf("product capability: artifact chronology or freshness is invalid")
	}
	requiresSource := engineeringArtifact(value.Kind)
	if requiresSource != (value.Source != nil) {
		return fmt.Errorf("product capability: artifact source binding is inconsistent")
	}
	if value.Source != nil {
		if err := value.Source.Validate(); err != nil {
			return fmt.Errorf("product capability: artifact source: %w", err)
		}
		if !value.Source.Fresh || value.Source.CapturedAt.After(value.EffectiveAt) {
			return fmt.Errorf("product capability: artifact source is stale or from the future")
		}
	}
	return nil
}

// CompanyStateBinding identifies one exact typed Company State input.
type CompanyStateBinding struct {
	Kind      companystate.RecordKind      `json:"kind"`
	Reference companystate.RecordReference `json:"reference"`
}

// Validate enforces the expected Company State kind and content identity.
func (value CompanyStateBinding) Validate(expected companystate.RecordKind) error {
	if value.Kind != expected {
		return fmt.Errorf("product capability: Company State kind %q does not match %q", value.Kind, expected)
	}
	return value.Reference.Validate()
}

// ProductDesignHandoff is the complete typed Product and Design contract that
// a fresh Developer wake may consume.
type ProductDesignHandoff struct {
	SchemaVersion         string                   `json:"schema_version"`
	ID                    HandoffID                `json:"handoff_id"`
	OrganizationID        contracts.OrganizationID `json:"organization_id"`
	InitiativeID          InitiativeID             `json:"initiative_id"`
	ProjectID             contracts.ProjectID      `json:"project_id"`
	WorkspaceID           contracts.WorkspaceID    `json:"workspace_id"`
	DeveloperIntentID     contracts.IntentID       `json:"developer_intent_id"`
	ProductState          CompanyStateBinding      `json:"product_state"`
	TargetSegmentState    CompanyStateBinding      `json:"target_segment_state"`
	ValuePropositionState CompanyStateBinding      `json:"value_proposition_state"`
	Artifacts             []Artifact               `json:"artifacts"`
	AcceptanceCriteria    []string                 `json:"acceptance_criteria"`
	ExperimentIDs         []string                 `json:"experiment_ids"`
	CreatedAt             time.Time                `json:"created_at"`
	ExpiresAt             time.Time                `json:"expires_at"`
}

// ValidateAt enforces complete Product and Design coverage and exact scope.
func (value ProductDesignHandoff) ValidateAt(now time.Time) error {
	if value.SchemaVersion != SchemaVersion {
		return fmt.Errorf("product capability: handoff schema is unsupported")
	}
	for name, tokenValue := range map[string]string{
		"handoff_id":          string(value.ID),
		"organization_id":     string(value.OrganizationID),
		"initiative_id":       string(value.InitiativeID),
		"project_id":          string(value.ProjectID),
		"workspace_id":        string(value.WorkspaceID),
		"developer_intent_id": string(value.DeveloperIntentID),
	} {
		if err := validateToken(name, tokenValue); err != nil {
			return err
		}
	}
	if len(value.Artifacts) == 0 || len(value.Artifacts) > 64 {
		return fmt.Errorf("product capability: handoff artifacts are outside bounds")
	}
	for _, binding := range []struct {
		name     string
		value    CompanyStateBinding
		expected companystate.RecordKind
	}{
		{"product state", value.ProductState, companystate.RecordProduct},
		{"target segment state", value.TargetSegmentState, companystate.RecordCustomerSegment},
		{"value proposition state", value.ValuePropositionState, companystate.RecordValueProposition},
	} {
		if err := binding.value.Validate(binding.expected); err != nil {
			return fmt.Errorf("product capability: %s: %w", binding.name, err)
		}
	}
	required := requiredHandoffKinds()
	seen := make(map[ArtifactKind]bool, len(value.Artifacts))
	for _, artifact := range value.Artifacts {
		if err := artifact.ValidateAt(now); err != nil {
			return err
		}
		if artifact.OrganizationID != value.OrganizationID ||
			artifact.InitiativeID != value.InitiativeID ||
			artifact.ProjectID != value.ProjectID || artifact.WorkspaceID != value.WorkspaceID {
			return fmt.Errorf("product capability: handoff artifact crosses scope")
		}
		if value.ExpiresAt.After(artifact.FreshUntil) {
			return fmt.Errorf("product capability: handoff outlives its evidence")
		}
		if seen[artifact.Kind] {
			return fmt.Errorf("product capability: handoff artifact kind %q is duplicated", artifact.Kind)
		}
		seen[artifact.Kind] = true
	}
	for _, kind := range required {
		if !seen[kind] {
			return fmt.Errorf("product capability: handoff is missing %q", kind)
		}
	}
	if err := validateTextSet("acceptance criterion", value.AcceptanceCriteria, 1, 128, 2048); err != nil {
		return err
	}
	if err := validateTokenSet("experiment id", value.ExperimentIDs, 1, 64); err != nil {
		return err
	}
	if !validUTC(value.CreatedAt) || !validUTC(value.ExpiresAt) ||
		!validUTC(now) || value.CreatedAt.After(now) || !value.ExpiresAt.After(value.CreatedAt) ||
		!value.ExpiresAt.After(now) {
		return fmt.Errorf("product capability: handoff times are invalid")
	}
	return nil
}

// DeveloperExecutionInput is the closed Project Brain-backed projection for a
// fresh Developer wake. It contains no prior transcript or private reasoning.
type DeveloperExecutionInput struct {
	SchemaVersion        string                `json:"schema_version"`
	Handoff              ProductDesignHandoff  `json:"handoff"`
	HandoffBrainRecordID projectbrain.RecordID `json:"handoff_brain_record_id"`
	Brain                projectbrain.View     `json:"project_brain"`
	Grant                developer.Grant       `json:"developer_grant"`
	AssembledAt          time.Time             `json:"assembled_at"`
}

// ValidateAt enforces the handoff, current Project Brain record, source, scope,
// lease, and fence bindings used by Developer execution.
func (value DeveloperExecutionInput) ValidateAt(now time.Time) error {
	if value.SchemaVersion != SchemaVersion || !validUTC(value.AssembledAt) ||
		!validUTC(now) || value.AssembledAt.After(now) {
		return fmt.Errorf("product capability: developer input identity or time is invalid")
	}
	if err := value.Handoff.ValidateAt(now); err != nil {
		return err
	}
	if err := value.Brain.Validate(); err != nil {
		return fmt.Errorf("product capability: Project Brain view: %w", err)
	}
	if !value.Brain.ExpiresAt.After(now) || !value.Brain.Source.Fresh ||
		value.Brain.OrganizationID != value.Handoff.OrganizationID ||
		value.Brain.ProjectID != value.Handoff.ProjectID ||
		value.Brain.WorkspaceID != value.Handoff.WorkspaceID {
		return fmt.Errorf("product capability: Project Brain view is stale or unrelated")
	}
	if err := value.Grant.Scope.Validate(); err != nil {
		return fmt.Errorf("product capability: Developer scope: %w", err)
	}
	if err := value.Grant.Lease.Request.Validate(); err != nil {
		return fmt.Errorf("product capability: Developer lease: %w", err)
	}
	grant := value.Grant.Lease
	if grant.State != lease.StateActive || grant.Fence == 0 ||
		!grant.ExpiresAt.After(now) || grant.OrganizationID != value.Handoff.OrganizationID ||
		contracts.IntentID(grant.NodeID) != value.Handoff.DeveloperIntentID ||
		value.Grant.Scope.TaskNodeID != grant.NodeID ||
		value.Grant.Scope.ProjectID != value.Handoff.ProjectID ||
		value.Grant.Scope.WorkspaceID != value.Handoff.WorkspaceID {
		return fmt.Errorf("product capability: Developer lease does not authorize the handoff")
	}
	if value.Grant.Scope.Source.RootDigest != value.Brain.Source.RootDigest ||
		value.Grant.Scope.Source.GraphDigest != value.Brain.Source.GraphDigest ||
		value.Grant.Scope.Source.Generation != value.Brain.Source.Generation {
		return fmt.Errorf("product capability: Developer scope and Project Brain source disagree")
	}
	if value.AssembledAt.Before(value.Brain.Source.CapturedAt) ||
		value.AssembledAt.Before(value.Grant.Scope.ResolvedAt) {
		return fmt.Errorf("product capability: Developer input predates its source or scope")
	}
	if !brainContainsHandoff(value.Brain, value.HandoffBrainRecordID, value.Handoff) {
		return fmt.Errorf("product capability: verified handoff is absent from Project Brain")
	}
	return nil
}

// ValidateCompatibleChanges rejects concurrent incompatible file or symbol
// scopes unless both inputs bind the same verified coordination plan.
func ValidateCompatibleChanges(values []DeveloperExecutionInput, now time.Time) error {
	if len(values) == 0 || len(values) > 8 {
		return fmt.Errorf("product capability: active Developer input count is outside bounds")
	}
	for index := range values {
		if err := values[index].ValidateAt(now); err != nil {
			return fmt.Errorf("product capability: Developer input %d: %w", index, err)
		}
	}
	for left := 0; left < len(values); left++ {
		for right := left + 1; right < len(values); right++ {
			if scopesOverlap(values[left].Grant.Scope, values[right].Grant.Scope) &&
				!inputsCoordinated(values[left], values[right]) {
				return fmt.Errorf("product capability: incompatible Developer scopes overlap")
			}
		}
	}
	return nil
}

// EngineeringResult binds verified source, build, test, deployment, telemetry,
// and customer evidence returned from Developer execution.
type EngineeringResult struct {
	SchemaVersion     string                     `json:"schema_version"`
	HandoffID         HandoffID                  `json:"handoff_id"`
	OrganizationID    contracts.OrganizationID   `json:"organization_id"`
	InitiativeID      InitiativeID               `json:"initiative_id"`
	ProjectID         contracts.ProjectID        `json:"project_id"`
	WorkspaceID       contracts.WorkspaceID      `json:"workspace_id"`
	DeveloperIntentID contracts.IntentID         `json:"developer_intent_id"`
	LeaseID           contracts.LeaseID          `json:"lease_id"`
	Fence             contracts.FenceToken       `json:"fence"`
	BrainViewDigest   contracts.ContentHash      `json:"project_brain_view_digest"`
	Source            projectbrain.GraphSnapshot `json:"source"`
	Artifacts         []Artifact                 `json:"artifacts"`
	CompletedAt       time.Time                  `json:"completed_at"`
}

// ValidateAt rejects result claims that are stale, cross-scope, or lack the
// minimum source, build, test, review, deployment, and operations evidence.
func (value EngineeringResult) ValidateAt(now time.Time) error {
	if value.SchemaVersion != SchemaVersion || value.Fence == 0 {
		return fmt.Errorf("product capability: engineering result schema or fence is invalid")
	}
	for name, tokenValue := range map[string]string{
		"handoff_id":          string(value.HandoffID),
		"organization_id":     string(value.OrganizationID),
		"initiative_id":       string(value.InitiativeID),
		"project_id":          string(value.ProjectID),
		"workspace_id":        string(value.WorkspaceID),
		"developer_intent_id": string(value.DeveloperIntentID),
		"lease_id":            string(value.LeaseID),
	} {
		if err := validateToken(name, tokenValue); err != nil {
			return err
		}
	}
	if err := value.BrainViewDigest.Validate(); err != nil {
		return fmt.Errorf("product capability: Project Brain digest: %w", err)
	}
	if err := value.Source.Validate(); err != nil {
		return fmt.Errorf("product capability: result source: %w", err)
	}
	if !value.Source.Fresh || !validUTC(value.CompletedAt) || !validUTC(now) ||
		value.CompletedAt.After(now) || value.CompletedAt.Before(value.Source.CapturedAt) {
		return fmt.Errorf("product capability: engineering result source or chronology is stale")
	}
	if len(value.Artifacts) == 0 || len(value.Artifacts) > 64 {
		return fmt.Errorf("product capability: engineering result artifacts are outside bounds")
	}
	seen := make(map[ArtifactKind]bool, len(value.Artifacts))
	byKind := make(map[ArtifactKind]Artifact, len(value.Artifacts))
	for _, artifact := range value.Artifacts {
		if err := artifact.ValidateAt(now); err != nil {
			return err
		}
		if artifact.OrganizationID != value.OrganizationID ||
			artifact.InitiativeID != value.InitiativeID ||
			artifact.ProjectID != value.ProjectID || artifact.WorkspaceID != value.WorkspaceID {
			return fmt.Errorf("product capability: engineering artifact crosses scope")
		}
		if artifact.Source != nil &&
			(artifact.Source.RootDigest != value.Source.RootDigest ||
				artifact.Source.GraphDigest != value.Source.GraphDigest ||
				artifact.Source.Generation != value.Source.Generation) {
			return fmt.Errorf("product capability: engineering artifact source is incompatible")
		}
		if seen[artifact.Kind] {
			return fmt.Errorf("product capability: engineering artifact kind %q is duplicated", artifact.Kind)
		}
		seen[artifact.Kind] = true
		byKind[artifact.Kind] = artifact
	}
	for _, kind := range requiredEngineeringKinds() {
		if !seen[kind] {
			return fmt.Errorf("product capability: engineering result is missing %q", kind)
		}
	}
	if !evidenceContainsHash(byKind[ArtifactDeploymentState], byKind[ArtifactSourceState].Artifact.Hash) ||
		!evidenceContainsHash(byKind[ArtifactDeploymentState], byKind[ArtifactBuildEvidence].Artifact.Hash) ||
		!evidenceContainsHash(byKind[ArtifactDeploymentState], byKind[ArtifactTestEvidence].Artifact.Hash) {
		return fmt.Errorf("product capability: deployment evidence does not bind source build and tests")
	}
	if !evidenceContainsHash(byKind[ArtifactQualityEvidence], byKind[ArtifactBuildEvidence].Artifact.Hash) ||
		!evidenceContainsHash(byKind[ArtifactQualityEvidence], byKind[ArtifactTestEvidence].Artifact.Hash) {
		return fmt.Errorf("product capability: quality evidence does not bind build and tests")
	}
	if byKind[ArtifactIndependentReview].AuthorSeatID == byKind[ArtifactImplementationPlan].AuthorSeatID ||
		!evidenceContainsHash(byKind[ArtifactIndependentReview], byKind[ArtifactReviewEvidence].Artifact.Hash) {
		return fmt.Errorf("product capability: engineering independent review is captured or unrelated")
	}
	return nil
}

// ValidateEngineeringResult binds returned source/build/test/deployment
// evidence to the exact live Project Brain-backed Developer input and lease.
func ValidateEngineeringResult(
	input DeveloperExecutionInput,
	result EngineeringResult,
	now time.Time,
) error {
	if err := input.ValidateAt(now); err != nil {
		return err
	}
	if err := result.ValidateAt(now); err != nil {
		return err
	}
	if result.HandoffID != input.Handoff.ID ||
		result.OrganizationID != input.Handoff.OrganizationID ||
		result.InitiativeID != input.Handoff.InitiativeID ||
		result.ProjectID != input.Handoff.ProjectID ||
		result.WorkspaceID != input.Handoff.WorkspaceID ||
		result.DeveloperIntentID != input.Handoff.DeveloperIntentID ||
		result.LeaseID != input.Grant.Lease.ID || result.Fence != input.Grant.Lease.Fence ||
		result.BrainViewDigest != input.Brain.Digest ||
		result.Source.RootDigest != input.Brain.Source.RootDigest ||
		result.Source.GraphDigest != input.Brain.Source.GraphDigest ||
		result.Source.Generation != input.Brain.Source.Generation ||
		result.CompletedAt.Before(input.AssembledAt) ||
		result.CompletedAt.After(input.Grant.Lease.ExpiresAt) {
		return fmt.Errorf("product capability: engineering result does not bind the authorized Developer input")
	}
	return nil
}

// LaunchState is the deterministic result of evaluating launch evidence.
type LaunchState string

const (
	LaunchBlocked       LaunchState = "blocked"
	LaunchReady         LaunchState = "ready"
	LaunchRequiresHuman LaunchState = "requires_human"
)

// LaunchAssessment is machine-readable and cannot be set by the executing seat.
type LaunchAssessment struct {
	SchemaVersion string                `json:"schema_version"`
	State         LaunchState           `json:"state"`
	Missing       []ArtifactKind        `json:"missing"`
	EvidenceHash  contracts.ContentHash `json:"evidence_hash"`
}

// EvaluateLaunch returns ready only when every non-code launch obligation is
// present and independently evidenced. Code, build, and tests alone stay blocked.
func EvaluateLaunch(result EngineeringResult, now time.Time) (LaunchAssessment, error) {
	if err := result.ValidateAt(now); err != nil {
		return LaunchAssessment{}, err
	}
	seen := make(map[ArtifactKind]bool, len(result.Artifacts))
	artifacts := make(map[ArtifactKind]Artifact, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		seen[artifact.Kind] = true
		artifacts[artifact.Kind] = artifact
	}
	required := launchKinds()
	missing := make([]ArtifactKind, 0)
	for _, kind := range required {
		if !seen[kind] {
			missing = append(missing, kind)
		}
	}
	if len(missing) == 0 {
		launch := artifacts[ArtifactLaunchReadiness]
		for _, kind := range required {
			if kind == ArtifactLaunchReadiness {
				continue
			}
			if !evidenceContainsHash(launch, artifacts[kind].Artifact.Hash) {
				missing = append(missing, ArtifactLaunchReadiness)
				break
			}
		}
		independent := artifacts[ArtifactIndependentReview]
		if !evidenceContainsHash(independent, launch.Artifact.Hash) {
			missing = append(missing, ArtifactIndependentReview)
		}
	}
	hash, err := canonicalHash(result)
	if err != nil {
		return LaunchAssessment{}, err
	}
	state := LaunchReady
	if len(missing) > 0 {
		state = LaunchBlocked
	}
	return LaunchAssessment{
		SchemaVersion: SchemaVersion, State: state, Missing: missing,
		EvidenceHash: hash,
	}, nil
}

// LifecycleBindings converts only independently verified capability evidence
// into the closed lifecycle gate taxonomy.
func LifecycleBindings(record VerifiedRecord, now time.Time) ([]companylifecycle.EvidenceBinding, error) {
	if err := record.ValidateAt(now); err != nil {
		return nil, err
	}
	artifacts := record.Record.Body.artifacts()
	if len(artifacts) == 0 {
		return nil, fmt.Errorf("product capability: record has no lifecycle artifacts")
	}
	recordHash, err := RecordHash(record.Record)
	if err != nil {
		return nil, err
	}
	verdictHash, err := canonicalHash(record.Verification)
	if err != nil {
		return nil, err
	}
	result := make([]companylifecycle.EvidenceBinding, 0, len(artifacts))
	for _, artifact := range artifacts {
		kind, mapped := lifecycleKind(artifact.Kind)
		if !mapped {
			continue
		}
		verdictID := record.Verification.ID
		result = append(result, companylifecycle.EvidenceBinding{
			SchemaVersion:          contracts.SchemaVersionV1,
			ID:                     contracts.EvidenceID(artifact.ID),
			Kind:                   kind,
			SourceRecordID:         string(record.Record.Body.ID),
			SourceRecordVersion:    record.Record.Body.Version,
			SourceRecordHash:       recordHash,
			EvidenceHash:           artifact.Artifact.Hash,
			Validity:               contracts.ValidityActive,
			ObservedAt:             artifact.ObservedAt,
			EffectiveAt:            artifact.EffectiveAt,
			FreshUntil:             artifact.FreshUntil,
			Contaminated:           false,
			IndependentVerdictID:   &verdictID,
			IndependentVerdictHash: &verdictHash,
		})
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Kind == result[right].Kind {
			return result[left].ID < result[right].ID
		}
		return result[left].Kind < result[right].Kind
	})
	return result, nil
}

func requiredHandoffKinds() []ArtifactKind {
	return []ArtifactKind{
		ArtifactCustomerProblem, ArtifactValueProposition, ArtifactRequirements,
		ArtifactRoadmap, ArtifactPriorityDecision, ArtifactUserResearch,
		ArtifactProductAnalytics, ArtifactCustomerOutcomeAcceptance,
		ArtifactUserJourney, ArtifactInteractionModel, ArtifactPrototype,
		ArtifactUsabilityEvidence, ArtifactAccessibilityEvidence,
		ArtifactDesignSystemDecision, ArtifactDesignHandoff,
	}
}

func requiredEngineeringKinds() []ArtifactKind {
	return []ArtifactKind{
		ArtifactImplementationPlan, ArtifactSourceState, ArtifactBuildEvidence,
		ArtifactTestEvidence, ArtifactReviewEvidence, ArtifactQualityEvidence,
		ArtifactSecurityEvidence, ArtifactReleasePlan, ArtifactDeploymentState,
		ArtifactRollbackEvidence, ArtifactHealthEvidence,
		ArtifactOperationsReadiness, ArtifactIndependentReview,
	}
}

func launchKinds() []ArtifactKind {
	return []ArtifactKind{
		ArtifactSourceState, ArtifactBuildEvidence, ArtifactTestEvidence,
		ArtifactReviewEvidence, ArtifactQualityEvidence, ArtifactSecurityEvidence,
		ArtifactDeploymentState, ArtifactRollbackEvidence, ArtifactHealthEvidence,
		ArtifactOperationsReadiness, ArtifactLegalEvidence, ArtifactPricingEvidence,
		ArtifactClaimsEvidence, ArtifactLaunchReadiness, ArtifactIndependentReview,
	}
}

func engineeringArtifact(kind ArtifactKind) bool {
	switch kind {
	case ArtifactImplementationPlan, ArtifactSourceState, ArtifactBuildEvidence,
		ArtifactTestEvidence, ArtifactReviewEvidence, ArtifactQualityEvidence,
		ArtifactSecurityEvidence, ArtifactReleasePlan, ArtifactDeploymentState,
		ArtifactRollbackEvidence, ArtifactHealthEvidence, ArtifactIncidentEvidence,
		ArtifactCapacityEvidence, ArtifactObservabilityEvidence,
		ArtifactReliabilityEvidence, ArtifactOperationsReadiness,
		ArtifactLaunchReadiness, ArtifactTelemetryEvidence,
		ArtifactIndependentReview:
		return true
	default:
		return false
	}
}

func lifecycleKind(kind ArtifactKind) (companylifecycle.EvidenceKind, bool) {
	switch kind {
	case ArtifactCustomerProblem:
		return companylifecycle.EvidenceCustomerProblem, true
	case ArtifactRequirements:
		return companylifecycle.EvidenceRequirements, true
	case ArtifactUserJourney:
		return companylifecycle.EvidenceUserJourney, true
	case ArtifactImplementationPlan:
		return companylifecycle.EvidenceImplementationPlan, true
	case ArtifactSourceState:
		return companylifecycle.EvidenceSourceState, true
	case ArtifactDeploymentState:
		return companylifecycle.EvidenceDeploymentState, true
	case ArtifactQualityEvidence:
		return companylifecycle.EvidenceQuality, true
	case ArtifactSecurityEvidence:
		return companylifecycle.EvidenceSecurity, true
	case ArtifactOperationsReadiness, ArtifactReliabilityEvidence:
		return companylifecycle.EvidenceOperationsReadiness, true
	case ArtifactClaimsEvidence:
		return companylifecycle.EvidenceClaims, true
	case ArtifactLegalEvidence:
		return companylifecycle.EvidenceLegal, true
	case ArtifactPricingEvidence:
		return companylifecycle.EvidencePricing, true
	case ArtifactLaunchReadiness:
		return companylifecycle.EvidenceLaunchReadiness, true
	case ArtifactTelemetryEvidence:
		return companylifecycle.EvidenceProductUsage, true
	case ArtifactCustomerEvidence:
		return companylifecycle.EvidenceCustomer, true
	case ArtifactIndependentReview:
		return companylifecycle.EvidenceIndependentReview, true
	default:
		return "", false
	}
}

func brainContainsHandoff(view projectbrain.View, id projectbrain.RecordID, handoff ProductDesignHandoff) bool {
	for _, record := range view.Records {
		if record.Proposal.ID != id || record.Proposal.Kind != projectbrain.KindHandoff ||
			record.Proposal.Origin != projectbrain.OriginReceipt {
			continue
		}
		for _, claim := range record.Proposal.Content.Claims {
			for _, evidence := range claim.Evidence {
				if evidence.Kind == "product_design_handoff" &&
					strings.Contains(claim.Statement, string(handoff.ID)) {
					return true
				}
			}
		}
	}
	return false
}

func scopesOverlap(left, right developer.ResolvedScope) bool {
	files := make(map[string]bool, len(left.Files))
	for _, file := range left.Files {
		files[file.Path] = true
	}
	for _, file := range right.Files {
		if files[file.Path] {
			return true
		}
	}
	symbols := make(map[string]bool, len(left.Symbols))
	for _, symbol := range left.Symbols {
		symbols[symbol] = true
	}
	for _, symbol := range right.Symbols {
		if symbols[symbol] {
			return true
		}
	}
	return false
}

func inputsCoordinated(left, right DeveloperExecutionInput) bool {
	leftScope := left.Grant.Scope
	rightScope := right.Grant.Scope
	if leftScope.CoordinationPlanID == nil || rightScope.CoordinationPlanID == nil ||
		*leftScope.CoordinationPlanID != *rightScope.CoordinationPlanID ||
		leftScope.Coordination == nil || rightScope.Coordination == nil ||
		leftScope.Coordination.Digest != rightScope.Coordination.Digest {
		return false
	}
	plan := leftScope.Coordination
	return slices.Contains(plan.Tasks, leftScope.TaskNodeID) &&
		slices.Contains(plan.Tasks, rightScope.TaskNodeID) &&
		slices.Contains(plan.Seats, left.Grant.Lease.SeatID) &&
		slices.Contains(plan.Seats, right.Grant.Lease.SeatID)
}

func evidenceContainsHash(value Artifact, wanted contracts.ContentHash) bool {
	for _, evidence := range value.Evidence {
		if evidence.Hash == wanted {
			return true
		}
	}
	return false
}

func validateToken(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("product capability: %s must contain 1 to 128 bytes", name)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' ||
			char == '.' || char == ':' || char == '/' {
			continue
		}
		return fmt.Errorf("product capability: %s contains an invalid character", name)
	}
	return nil
}

func validateTokenSet(name string, values []string, minimum, maximum int) error {
	if len(values) < minimum || len(values) > maximum {
		return fmt.Errorf("product capability: %s count is outside bounds", name)
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if err := validateToken(name, value); err != nil {
			return err
		}
		if seen[value] {
			return fmt.Errorf("product capability: %s is duplicated", name)
		}
		seen[value] = true
	}
	return nil
}

func validateTextSet(name string, values []string, minimum, maximum, bytes int) error {
	if len(values) < minimum || len(values) > maximum {
		return fmt.Errorf("product capability: %s count is outside bounds", name)
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > bytes || seen[value] {
			return fmt.Errorf("product capability: %s is empty, duplicated, or oversized", name)
		}
		seen[value] = true
	}
	return nil
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}
