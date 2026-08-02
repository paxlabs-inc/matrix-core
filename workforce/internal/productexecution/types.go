// Package productexecution composes funded company Work Orders, dynamic squads,
// fresh Workforce wakes, verified product records, fenced deployment effects,
// and company lifecycle gates into one restart-safe product delivery.
package productexecution

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/companylifecycle"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/effect"
	"matrix/workforce/internal/portfolio"
	"matrix/workforce/internal/productcapability"
	"matrix/workforce/internal/projectbrain"
	"matrix/workforce/internal/squad"
	"matrix/workforce/internal/workorder"
)

const (
	SchemaVersion      = "workforce.product-execution.v1"
	EventSchemaVersion = "workforce.product-execution-event.v1"
)

type ExecutionID string

type Stage string

const (
	StageProduct      Stage = "product"
	StageDesign       Stage = "design"
	StageBuild        Stage = "build"
	StageVerification Stage = "verification"
	StageDeployment   Stage = "deployment"
	StageTelemetry    Stage = "telemetry"
)

var orderedStages = []Stage{
	StageProduct, StageDesign, StageBuild, StageVerification,
	StageDeployment, StageTelemetry,
}

func (value Stage) Valid() bool { return slices.Contains(orderedStages, value) }

func AllStages() []Stage { return slices.Clone(orderedStages) }

type Phase string

const (
	PhaseProductQueued       Phase = "product_queued"
	PhaseDesignQueued        Phase = "design_queued"
	PhaseHandoffVerified     Phase = "handoff_verified"
	PhaseBuildQueued         Phase = "build_queued"
	PhaseVerifyQueued        Phase = "verify_queued"
	PhaseDeploymentQueued    Phase = "deployment_queued"
	PhaseDeploymentPending   Phase = "deployment_pending"
	PhaseDeploymentAmbiguous Phase = "deployment_ambiguous"
	PhaseDeployed            Phase = "deployed"
	PhaseTelemetryQueued     Phase = "telemetry_queued"
	PhaseLaunchReady         Phase = "launch_ready"
	PhaseLaunched            Phase = "launched"
	PhaseCorrectionRequired  Phase = "correction_required"
	PhaseRollbackPending     Phase = "rollback_pending"
	PhaseRolledBack          Phase = "rolled_back"
	PhaseFailed              Phase = "failed"
)

var phases = []Phase{
	PhaseProductQueued, PhaseDesignQueued, PhaseHandoffVerified,
	PhaseBuildQueued, PhaseVerifyQueued, PhaseDeploymentQueued,
	PhaseDeploymentPending, PhaseDeploymentAmbiguous, PhaseDeployed,
	PhaseTelemetryQueued, PhaseLaunchReady, PhaseLaunched,
	PhaseCorrectionRequired, PhaseRollbackPending, PhaseRolledBack,
	PhaseFailed,
}

func (value Phase) Valid() bool { return slices.Contains(phases, value) }

type StagePlan struct {
	Stage      Stage  `json:"stage"`
	PlanNodeID string `json:"plan_node_id"`
	NeedID     string `json:"need_id"`
}

func (value StagePlan) Validate() error {
	if !value.Stage.Valid() || validateToken("plan node id", value.PlanNodeID) != nil ||
		validateToken("capability need id", value.NeedID) != nil {
		return fmt.Errorf("product execution: stage plan is invalid")
	}
	return nil
}

// StartRequest is a fully bound autonomous execution request. It carries only
// public verification authority; deployment credentials remain inside adapters.
type StartRequest struct {
	SchemaVersion        string                        `json:"schema_version"`
	ID                   ExecutionID                   `json:"execution_id"`
	OrganizationID       contracts.OrganizationID      `json:"organization_id"`
	InitiativeID         companylifecycle.InitiativeID `json:"initiative_id"`
	ProjectID            contracts.ProjectID           `json:"project_id"`
	WorkspaceID          contracts.WorkspaceID         `json:"workspace_id"`
	CompanyAuthority     workorder.CompanyAuthority    `json:"company_authority"`
	PortfolioDecision    portfolio.DecisionReceipt     `json:"portfolio_decision"`
	SquadRequirement     squad.Requirement             `json:"squad_requirement"`
	Stages               []StagePlan                   `json:"stages"`
	HandoffID            productcapability.HandoffID   `json:"handoff_id"`
	BaselineSource       projectbrain.GraphSnapshot    `json:"baseline_source"`
	BrainViewDigest      contracts.ContentHash         `json:"project_brain_view_digest"`
	CompanyStateRecordID string                        `json:"company_state_record_id"`
	IdempotencyKey       string                        `json:"idempotency_key"`
	CreatedAt            time.Time                     `json:"created_at"`
}

func (value StartRequest) Validate() error {
	if value.SchemaVersion != SchemaVersion ||
		validateToken("execution id", string(value.ID)) != nil || len(value.ID) > 64 ||
		validateToken("organization id", string(value.OrganizationID)) != nil ||
		validateToken("initiative id", string(value.InitiativeID)) != nil ||
		validateToken("project id", string(value.ProjectID)) != nil ||
		validateToken("workspace id", string(value.WorkspaceID)) != nil ||
		validateToken("handoff id", string(value.HandoffID)) != nil ||
		validateToken("company state record id", value.CompanyStateRecordID) != nil ||
		validateToken("idempotency key", value.IdempotencyKey) != nil ||
		!validUTC(value.CreatedAt) {
		return fmt.Errorf("product execution: start identity or time is invalid")
	}
	if err := value.CompanyAuthority.Validate(value.OrganizationID); err != nil {
		return fmt.Errorf("product execution: company authority: %w", err)
	}
	if err := value.PortfolioDecision.Validate(); err != nil ||
		value.PortfolioDecision.OrganizationID != value.OrganizationID ||
		value.PortfolioDecision.Decision != portfolio.DecisionGO ||
		value.PortfolioDecision.InitiativeID == nil ||
		string(*value.PortfolioDecision.InitiativeID) != string(value.InitiativeID) {
		return fmt.Errorf("product execution: portfolio decision does not fund the initiative")
	}
	if err := value.SquadRequirement.Validate(); err != nil ||
		value.SquadRequirement.OrganizationID != value.OrganizationID ||
		string(value.SquadRequirement.InitiativeID) != string(value.InitiativeID) ||
		value.SquadRequirement.LifecycleStage != "DESIGN" ||
		value.SquadRequirement.IssuedAt.After(value.CreatedAt) ||
		!value.SquadRequirement.ExpiresAt.After(value.CreatedAt) {
		return fmt.Errorf("product execution: squad requirement does not bind current product work")
	}
	if len(value.Stages) != len(orderedStages) {
		return fmt.Errorf("product execution: every product execution stage requires a plan node")
	}
	seenNodes := make(map[string]bool, len(value.Stages))
	seenNeeds := make(map[string]bool, len(value.Stages))
	for index, stage := range value.Stages {
		if err := stage.Validate(); err != nil || stage.Stage != orderedStages[index] ||
			seenNodes[stage.PlanNodeID] || seenNeeds[stage.NeedID] ||
			!slices.Contains(value.SquadRequirement.GraphScopes, stage.PlanNodeID) {
			return fmt.Errorf("product execution: stage plans must be complete, ordered, unique, and squad-scoped")
		}
		seenNodes[stage.PlanNodeID] = true
		seenNeeds[stage.NeedID] = true
	}
	if err := value.BaselineSource.Validate(); err != nil ||
		!value.BaselineSource.Fresh || value.BaselineSource.CapturedAt.After(value.CreatedAt) {
		return fmt.Errorf("product execution: baseline source is not current")
	}
	if err := value.BrainViewDigest.Validate(); err != nil {
		return fmt.Errorf("product execution: Project Brain digest: %w", err)
	}
	return nil
}

type StartRecord struct {
	SchemaVersion string                `json:"schema_version"`
	Request       StartRequest          `json:"request"`
	PlanID        string                `json:"plan_id"`
	PlanVersion   uint64                `json:"plan_version"`
	PlanHash      contracts.ContentHash `json:"plan_hash"`
	Assignment    squad.Assignment      `json:"assignment"`
}

func (value StartRecord) Validate() error {
	if value.SchemaVersion != SchemaVersion || value.Request.Validate() != nil ||
		validateToken("plan id", value.PlanID) != nil || value.PlanVersion == 0 ||
		value.PlanHash.Validate() != nil || value.Assignment.Validate() != nil ||
		value.Assignment.ID != value.Request.SquadRequirement.ID ||
		value.Assignment.OrganizationID != value.Request.OrganizationID ||
		string(value.Assignment.InitiativeID) != string(value.Request.InitiativeID) {
		return fmt.Errorf("product execution: start record is invalid")
	}
	return nil
}

type StageBinding struct {
	Stage          Stage                  `json:"stage"`
	PlanNodeID     string                 `json:"plan_node_id"`
	WorkOrderID    string                 `json:"work_order_id"`
	NeedID         string                 `json:"need_id"`
	SeatID         contracts.SeatID       `json:"seat_id"`
	DepartmentID   contracts.DepartmentID `json:"department_id"`
	Role           contracts.SeatRole     `json:"role"`
	MandateID      contracts.MandateID    `json:"mandate_id"`
	MandateVersion uint64                 `json:"mandate_version"`
	MandateDigest  contracts.ContentHash  `json:"mandate_digest"`
	GoalID         string                 `json:"goal_id"`
	IntentID       contracts.IntentID     `json:"intent_id"`
	WakeID         contracts.WakeID       `json:"wake_id"`
}

func (value StageBinding) Validate() error {
	if !value.Stage.Valid() || validateToken("plan node id", value.PlanNodeID) != nil ||
		validateToken("work order id", value.WorkOrderID) != nil ||
		validateToken("need id", value.NeedID) != nil ||
		validateToken("seat id", string(value.SeatID)) != nil ||
		validateToken("department id", string(value.DepartmentID)) != nil ||
		!value.Role.Valid() || validateToken("mandate id", string(value.MandateID)) != nil ||
		value.MandateVersion == 0 || value.MandateDigest.Validate() != nil ||
		validateToken("goal id", value.GoalID) != nil ||
		validateToken("intent id", string(value.IntentID)) != nil ||
		validateToken("wake id", string(value.WakeID)) != nil {
		return fmt.Errorf("product execution: stage binding is invalid")
	}
	if value.Stage == StageVerification && value.Role != contracts.SeatAuditor ||
		value.Stage != StageVerification && value.Role == contracts.SeatAuditor {
		return fmt.Errorf("product execution: stage binding violates verification independence")
	}
	return nil
}

type StageReceipt struct {
	Stage       Stage                 `json:"stage"`
	ReceiptID   contracts.ReceiptID   `json:"receipt_id"`
	ReceiptHash contracts.ContentHash `json:"receipt_hash"`
	VerdictID   contracts.VerdictID   `json:"verdict_id"`
	SeatID      contracts.SeatID      `json:"seat_id"`
	IntentID    contracts.IntentID    `json:"intent_id"`
	AcceptedAt  time.Time             `json:"accepted_at"`
}

func (value StageReceipt) Validate() error {
	if !value.Stage.Valid() || validateToken("receipt id", string(value.ReceiptID)) != nil ||
		value.ReceiptHash.Validate() != nil || validateToken("verdict id", string(value.VerdictID)) != nil ||
		validateToken("seat id", string(value.SeatID)) != nil ||
		validateToken("intent id", string(value.IntentID)) != nil || !validUTC(value.AcceptedAt) {
		return fmt.Errorf("product execution: stage receipt is invalid")
	}
	return nil
}

type CrossAuditProof struct {
	EpochID           string              `json:"epoch_id"`
	OriginalVerdictID contracts.VerdictID `json:"original_verdict_id"`
	ReauditVerdictID  contracts.VerdictID `json:"reaudit_verdict_id"`
}

func (value CrossAuditProof) Validate() error {
	if validateToken("cross-audit epoch id", value.EpochID) != nil ||
		validateToken("original verdict id", string(value.OriginalVerdictID)) != nil ||
		validateToken("reaudit verdict id", string(value.ReauditVerdictID)) != nil ||
		value.OriginalVerdictID == value.ReauditVerdictID {
		return fmt.Errorf("product execution: cross-audit proof is invalid")
	}
	return nil
}

type ReceiptRequest struct {
	ExecutionID    ExecutionID         `json:"execution_id"`
	ReceiptID      contracts.ReceiptID `json:"receipt_id"`
	IdempotencyKey string              `json:"idempotency_key"`
}

func (value ReceiptRequest) Validate() error {
	if validateToken("execution id", string(value.ExecutionID)) != nil ||
		validateToken("receipt id", string(value.ReceiptID)) != nil ||
		validateToken("idempotency key", value.IdempotencyKey) != nil {
		return fmt.Errorf("product execution: receipt request is invalid")
	}
	return nil
}

type CompleteDesignRequest struct {
	ReceiptRequest
	Record       productcapability.VerifiedRecord   `json:"record"`
	GateEvidence []companylifecycle.EvidenceBinding `json:"gate_evidence"`
}

type CompleteStageRequest struct {
	ReceiptRequest
	GateEvidence []companylifecycle.EvidenceBinding `json:"gate_evidence"`
}

type DeploymentRequest struct {
	ExecutionID    ExecutionID     `json:"execution_id"`
	Proposal       effect.Proposal `json:"proposal"`
	IdempotencyKey string          `json:"idempotency_key"`
}

func (value DeploymentRequest) Validate() error {
	if validateToken("execution id", string(value.ExecutionID)) != nil ||
		validateToken("idempotency key", value.IdempotencyKey) != nil ||
		value.Proposal.Validate() != nil {
		return fmt.Errorf("product execution: deployment request is invalid")
	}
	return nil
}

type CompleteLaunchRequest struct {
	ReceiptRequest
	Record       productcapability.VerifiedRecord   `json:"record"`
	GateEvidence []companylifecycle.EvidenceBinding `json:"gate_evidence"`
	CrossAudit   CrossAuditProof                    `json:"cross_audit"`
}

type ResumeRequest struct {
	ExecutionID ExecutionID                `json:"execution_id"`
	Source      projectbrain.GraphSnapshot `json:"source"`
	Brain       projectbrain.View          `json:"project_brain"`
}

type EffectView struct {
	EffectID     companylifecycle.EffectID `json:"effect_id"`
	ProposalID   string                    `json:"proposal_id"`
	ProposalHash contracts.ContentHash     `json:"proposal_hash"`
	Operation    string                    `json:"operation"`
	State        string                    `json:"state"`
	ExternalID   string                    `json:"external_id"`
	EvidenceHash *contracts.ContentHash    `json:"evidence_hash"`
	PreparedAt   time.Time                 `json:"prepared_at"`
	ReconciledAt *time.Time                `json:"reconciled_at"`
}

type View struct {
	SchemaVersion       string                         `json:"schema_version"`
	ID                  ExecutionID                    `json:"execution_id"`
	OrganizationID      contracts.OrganizationID       `json:"organization_id"`
	InitiativeID        companylifecycle.InitiativeID  `json:"initiative_id"`
	PlanID              string                         `json:"plan_id"`
	PlanVersion         uint64                         `json:"plan_version"`
	PlanHash            contracts.ContentHash          `json:"plan_hash"`
	SquadAssignmentID   squad.AssignmentID             `json:"squad_assignment_id"`
	ProjectID           contracts.ProjectID            `json:"project_id"`
	WorkspaceID         contracts.WorkspaceID          `json:"workspace_id"`
	Phase               Phase                          `json:"phase"`
	Version             uint64                         `json:"version"`
	ProductRecordID     *productcapability.RecordID    `json:"product_record_id"`
	EngineeringRecordID *productcapability.RecordID    `json:"engineering_record_id"`
	DeploymentEffectID  *companylifecycle.EffectID     `json:"deployment_effect_id"`
	LaunchTransitionID  *companylifecycle.TransitionID `json:"launch_transition_id"`
	CheckpointVersion   uint64                         `json:"checkpoint_version"`
	Stages              []StageBinding                 `json:"stages"`
	Receipts            []StageReceipt                 `json:"receipts"`
	Effects             []EffectView                   `json:"effects"`
	CreatedAt           time.Time                      `json:"created_at"`
	UpdatedAt           time.Time                      `json:"updated_at"`
}

type Recovery struct {
	Execution         View                           `json:"execution"`
	ProductCheckpoint productcapability.Checkpoint   `json:"product_checkpoint"`
	Lifecycle         companylifecycle.RecoveryState `json:"lifecycle"`
	SquadState        squad.AssignmentState          `json:"squad_state"`
	RequiresReconcile []companylifecycle.EffectID    `json:"requires_reconciliation"`
}

type Event struct {
	SchemaVersion  string      `json:"schema_version"`
	ExecutionID    ExecutionID `json:"execution_id"`
	Sequence       uint64      `json:"sequence"`
	Phase          Phase       `json:"phase"`
	Kind           string      `json:"kind"`
	Stage          *Stage      `json:"stage"`
	SourceID       string      `json:"source_id"`
	IdempotencyKey string      `json:"idempotency_key"`
	CreatedAt      time.Time   `json:"created_at"`
}

func (value Event) Validate() error {
	if value.SchemaVersion != EventSchemaVersion ||
		validateToken("execution id", string(value.ExecutionID)) != nil || value.Sequence == 0 ||
		!value.Phase.Valid() || validateToken("event kind", value.Kind) != nil ||
		validateToken("event idempotency key", value.IdempotencyKey) != nil ||
		!validUTC(value.CreatedAt) {
		return fmt.Errorf("product execution: event is invalid")
	}
	if value.Stage != nil && !value.Stage.Valid() {
		return fmt.Errorf("product execution: event stage is invalid")
	}
	if value.SourceID != "" && validateToken("event source id", value.SourceID) != nil {
		return fmt.Errorf("product execution: event source is invalid")
	}
	return nil
}

func validateToken(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("product execution: %s must contain 1 to 128 bytes", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-_.:/", character) {
			continue
		}
		return fmt.Errorf("product execution: %s contains an invalid character", name)
	}
	return nil
}

func validUTC(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }
