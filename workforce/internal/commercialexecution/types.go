package commercialexecution

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"matrix/workforce/internal/businessoutcome"
	"matrix/workforce/internal/contracts"
)

const SchemaVersion = "workforce.commercial-execution.v1"

var (
	ErrConflict     = errors.New("commercial execution: durable identity conflict")
	ErrUnauthorized = errors.New("commercial execution: authority is not current")
	ErrOutOfOrder   = errors.New("commercial execution: phase is not currently eligible")
	ErrPending      = errors.New("commercial execution: external outcome remains pending")
	ErrReconciling  = errors.New("commercial execution: authoritative reconciliation is required")
	ErrFailed       = errors.New("commercial execution: workflow failed")
	ErrIntegrity    = errors.New("commercial execution: sealed lineage integrity failure")
)

type ExecutionID string
type EvidenceID string
type CorrectionID string
type RecoveryID string

type State string

const (
	StatePendingExternal State = "pending_external"
	StateReconciling     State = "reconciling"
	StateCompleted       State = "completed"
	StateFailed          State = "failed"
)

func (value State) Valid() bool {
	return value == StatePendingExternal || value == StateReconciling ||
		value == StateCompleted || value == StateFailed
}

type Phase string

const (
	PhaseAcquisition             Phase = "acquisition"
	PhaseCustomerQualification   Phase = "customer_qualification"
	PhaseSale                    Phase = "sale"
	PhaseFinancialIntent         Phase = "financial_intent"
	PhaseFinancialReconciliation Phase = "financial_reconciliation"
	PhaseSupport                 Phase = "support"
	PhaseMeasurement             Phase = "measurement"
)

var phaseOrder = []Phase{
	PhaseAcquisition,
	PhaseCustomerQualification,
	PhaseSale,
	PhaseFinancialIntent,
	PhaseFinancialReconciliation,
	PhaseSupport,
	PhaseMeasurement,
}

func (value Phase) Valid() bool { return phaseOrdinal(value) >= 0 }

func phaseOrdinal(value Phase) int {
	for index, phase := range phaseOrder {
		if value == phase {
			return index
		}
	}
	return -1
}

func nextPhase(value Phase) (Phase, bool) {
	index := phaseOrdinal(value)
	if index < 0 || index+1 >= len(phaseOrder) {
		return "", false
	}
	return phaseOrder[index+1], true
}

type SourceKind string

const (
	SourceProductExecution     SourceKind = "product_execution"
	SourceCustomerScope        SourceKind = "customer_scope"
	SourceCustomerObservation  SourceKind = "customer_observation"
	SourceFinancialReservation SourceKind = "financial_reservation"
	SourceFinancialObservation SourceKind = "financial_observation"
	SourceFinancialAccounting  SourceKind = "financial_accounting"
	SourceBusinessMetric       SourceKind = "business_metric"
	SourceBusinessObservation  SourceKind = "business_observation"
	SourceBusinessOutcome      SourceKind = "business_outcome"
	SourceBusinessGate         SourceKind = "business_gate"
	SourceBusinessCorrection   SourceKind = "business_correction"
)

func (value SourceKind) Valid() bool {
	switch value {
	case SourceProductExecution, SourceCustomerScope, SourceCustomerObservation,
		SourceFinancialReservation, SourceFinancialObservation, SourceFinancialAccounting,
		SourceBusinessMetric, SourceBusinessObservation, SourceBusinessOutcome,
		SourceBusinessGate, SourceBusinessCorrection:
		return true
	default:
		return false
	}
}

func (value SourceKind) Versioned() bool {
	return value == SourceProductExecution || value == SourceCustomerScope ||
		value == SourceBusinessMetric || value == SourceBusinessOutcome
}

type SourceRole string

const (
	RoleLaunchedProduct     SourceRole = "launched_product"
	RoleAcquisitionEvent    SourceRole = "acquisition_event"
	RoleQualifiedCustomer   SourceRole = "qualified_customer"
	RoleCRMRecord           SourceRole = "crm_record"
	RoleSalesOrder          SourceRole = "sales_order"
	RoleContract            SourceRole = "contract"
	RoleFinancialIntent     SourceRole = "financial_intent"
	RoleFinancialSettlement SourceRole = "financial_settlement"
	RoleBalancedAccounting  SourceRole = "balanced_accounting"
	RoleSupportCycle        SourceRole = "support_cycle"
	RoleMetricDefinition    SourceRole = "metric_definition"
	RoleMetricObservation   SourceRole = "metric_observation"
	RoleCommercialOutcome   SourceRole = "commercial_outcome"
	RoleBusinessGate        SourceRole = "business_gate"
	RoleCorrectionEvidence  SourceRole = "correction_evidence"
)

func (value SourceRole) Valid() bool {
	switch value {
	case RoleLaunchedProduct, RoleAcquisitionEvent, RoleQualifiedCustomer,
		RoleCRMRecord, RoleSalesOrder, RoleContract, RoleFinancialIntent,
		RoleFinancialSettlement, RoleBalancedAccounting, RoleSupportCycle,
		RoleMetricDefinition, RoleMetricObservation, RoleCommercialOutcome,
		RoleBusinessGate, RoleCorrectionEvidence:
		return true
	default:
		return false
	}
}

type SourceAuthority string

const (
	AuthorityUntrusted             SourceAuthority = "untrusted_external_data"
	AuthorityInternalVerified      SourceAuthority = "internal_verified"
	AuthorityProviderAuthoritative SourceAuthority = "provider_authoritative"
	AuthorityControlPlane          SourceAuthority = "control_plane_authoritative"
	AuthorityReconciledFinancial   SourceAuthority = "reconciled_financial"
	AuthorityIndependentOutcome    SourceAuthority = "independent_outcome"
)

func (value SourceAuthority) Valid() bool {
	return value == AuthorityUntrusted || value == AuthorityInternalVerified || value == AuthorityProviderAuthoritative ||
		value == AuthorityControlPlane || value == AuthorityReconciledFinancial ||
		value == AuthorityIndependentOutcome
}

type SourceState string

const (
	SourcePending    SourceState = "pending"
	SourceAmbiguous  SourceState = "ambiguous"
	SourceCompleted  SourceState = "completed"
	SourceReconciled SourceState = "reconciled"
	SourceFailed     SourceState = "failed"
	SourceReversed   SourceState = "reversed"
	SourceSatisfied  SourceState = "satisfied"
)

func (value SourceState) Valid() bool {
	return value == SourcePending || value == SourceAmbiguous || value == SourceCompleted ||
		value == SourceReconciled || value == SourceFailed || value == SourceReversed ||
		value == SourceSatisfied
}

type SourceRef struct {
	Role          SourceRole            `json:"role"`
	Kind          SourceKind            `json:"kind"`
	RecordID      string                `json:"record_id"`
	Version       uint64                `json:"version"`
	Hash          contracts.ContentHash `json:"hash"`
	Operation     string                `json:"operation"`
	Provider      string                `json:"provider"`
	AccountRef    string                `json:"account_ref"`
	ExternalRef   string                `json:"external_ref"`
	RelatedID     string                `json:"related_id"`
	ValuationTime *time.Time            `json:"valuation_time"`
	State         SourceState           `json:"state"`
	Authority     SourceAuthority       `json:"authority"`
	ObservedAt    time.Time             `json:"observed_at"`
}

func (value SourceRef) Validate() error {
	if !value.Role.Valid() || !value.Kind.Valid() || token("source record id", value.RecordID) != nil ||
		value.Hash.Validate() != nil || !value.State.Valid() || !value.Authority.Valid() ||
		!validUTC(value.ObservedAt) {
		return fmt.Errorf("commercial execution: evidence source is invalid")
	}
	if value.Kind.Versioned() != (value.Version > 0) {
		return fmt.Errorf("commercial execution: evidence source version is invalid")
	}
	if value.Operation != "" && token("source operation", value.Operation) != nil {
		return fmt.Errorf("commercial execution: source operation is invalid")
	}
	for name, field := range map[string]string{
		"provider": value.Provider, "account ref": value.AccountRef,
		"external ref": value.ExternalRef, "related id": value.RelatedID,
	} {
		if field != "" && bounded(name, field) != nil {
			return fmt.Errorf("commercial execution: source provider binding is invalid")
		}
	}
	if value.ValuationTime != nil && !validUTC(*value.ValuationTime) {
		return fmt.Errorf("commercial execution: source valuation time is invalid")
	}
	switch value.Kind {
	case SourceCustomerObservation:
		if value.Provider == "" || value.ExternalRef == "" || value.AccountRef != "" ||
			value.RelatedID != "" || value.ValuationTime != nil {
			return fmt.Errorf("commercial execution: customer provider binding is incomplete")
		}
	case SourceFinancialReservation:
		if value.AccountRef == "" || value.Provider != "" || value.ExternalRef != "" ||
			value.RelatedID != "" || value.ValuationTime != nil {
			return fmt.Errorf("commercial execution: financial intent account binding is incomplete")
		}
	case SourceFinancialObservation:
		if value.Provider == "" || value.AccountRef == "" || value.ExternalRef == "" ||
			value.RelatedID != "" || value.ValuationTime != nil {
			return fmt.Errorf("commercial execution: financial observation binding is incomplete")
		}
	case SourceFinancialAccounting:
		if value.AccountRef == "" || value.RelatedID == "" || value.ValuationTime == nil ||
			value.Provider != "" || value.ExternalRef != "" || value.Operation != "" {
			return fmt.Errorf("commercial execution: accounting entry binding is incomplete")
		}
	default:
		if value.Provider != "" || value.AccountRef != "" || value.ExternalRef != "" ||
			value.RelatedID != "" || value.ValuationTime != nil {
			return fmt.Errorf("commercial execution: source has inapplicable provider fields")
		}
	}
	if expectedKind(value.Role) != "" && expectedKind(value.Role) != value.Kind {
		return fmt.Errorf("commercial execution: evidence role and source kind disagree")
	}
	return nil
}

func expectedKind(role SourceRole) SourceKind {
	switch role {
	case RoleLaunchedProduct:
		return SourceProductExecution
	case RoleQualifiedCustomer:
		return SourceCustomerScope
	case RoleAcquisitionEvent, RoleCRMRecord, RoleSalesOrder, RoleContract, RoleSupportCycle:
		return SourceCustomerObservation
	case RoleFinancialIntent:
		return SourceFinancialReservation
	case RoleFinancialSettlement:
		return SourceFinancialObservation
	case RoleBalancedAccounting:
		return SourceFinancialAccounting
	case RoleMetricDefinition:
		return SourceBusinessMetric
	case RoleMetricObservation:
		return SourceBusinessObservation
	case RoleCommercialOutcome:
		return SourceBusinessOutcome
	case RoleBusinessGate:
		return SourceBusinessGate
	default:
		return ""
	}
}

type LeaseBinding struct {
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	SeatID         contracts.SeatID         `json:"seat_id"`
	LeaseID        contracts.LeaseID        `json:"lease_id"`
	Fence          contracts.FenceToken     `json:"fence"`
}

func (value LeaseBinding) Validate() error {
	if token("organization id", string(value.OrganizationID)) != nil ||
		token("seat id", string(value.SeatID)) != nil || token("lease id", string(value.LeaseID)) != nil ||
		value.Fence.Validate() != nil {
		return fmt.Errorf("commercial execution: lease binding is invalid")
	}
	return nil
}

type OperationBinding struct {
	Phase      Phase    `json:"phase"`
	Operations []string `json:"operations"`
}

func (value OperationBinding) Validate() error {
	if !value.Phase.Valid() || len(value.Operations) == 0 || len(value.Operations) > 8 ||
		!sort.StringsAreSorted(value.Operations) {
		return fmt.Errorf("commercial execution: operation binding is invalid")
	}
	for index, operation := range value.Operations {
		if token("operation", operation) != nil || index > 0 && value.Operations[index-1] == operation {
			return fmt.Errorf("commercial execution: phase operations must be sorted and unique")
		}
	}
	return nil
}

type Scope struct {
	ProductExecutionID           string                          `json:"product_execution_id"`
	ProductExecutionHash         contracts.ContentHash           `json:"product_execution_hash"`
	CustomerConnectionID         string                          `json:"customer_connection_id"`
	CustomerConnectionVersion    uint64                          `json:"customer_connection_version"`
	FinancialConnectionID        string                          `json:"financial_connection_id"`
	FinancialConnectionVersion   uint64                          `json:"financial_connection_version"`
	AudienceHash                 contracts.ContentHash           `json:"audience_hash"`
	Jurisdiction                 string                          `json:"jurisdiction"`
	Currency                     string                          `json:"currency"`
	MaximumTransactionMicrounits uint64                          `json:"maximum_transaction_microunits"`
	Gate                         businessoutcome.GateRequirement `json:"gate"`
	Policies                     []contracts.PolicyRef           `json:"policies"`
	Operations                   []OperationBinding              `json:"operations"`
}

func (value Scope) Validate() error {
	if token("product execution id", value.ProductExecutionID) != nil ||
		value.ProductExecutionHash.Validate() != nil ||
		token("customer connection id", value.CustomerConnectionID) != nil ||
		value.CustomerConnectionVersion == 0 ||
		token("financial connection id", value.FinancialConnectionID) != nil ||
		value.FinancialConnectionVersion == 0 || value.AudienceHash.Validate() != nil ||
		bounded("jurisdiction", value.Jurisdiction) != nil || token("currency", value.Currency) != nil ||
		value.MaximumTransactionMicrounits == 0 || value.MaximumTransactionMicrounits > 1<<62 ||
		value.Gate.Validate() != nil || len(value.Policies) == 0 || len(value.Policies) > 64 ||
		len(value.Operations) != len(phaseOrder) {
		return fmt.Errorf("commercial execution: commercial scope is invalid")
	}
	for index, policy := range value.Policies {
		if policy.Validate() != nil || index > 0 && policyKey(value.Policies[index-1]) >= policyKey(policy) {
			return fmt.Errorf("commercial execution: policy bindings must be sorted and unique")
		}
	}
	for index, binding := range value.Operations {
		if binding.Validate() != nil || binding.Phase != phaseOrder[index] {
			return fmt.Errorf("commercial execution: every phase requires one ordered operation binding")
		}
	}
	return nil
}

func (value Scope) OperationAllowed(phase Phase, operation string) bool {
	for _, binding := range value.Operations {
		if binding.Phase == phase {
			return slicesContain(binding.Operations, operation)
		}
	}
	return false
}

type PlanBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             ExecutionID              `json:"execution_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   string                   `json:"initiative_id"`
	WorkOrderID    contracts.WorkOrderID    `json:"work_order_id"`
	WorkOrderHash  contracts.ContentHash    `json:"work_order_hash"`
	Scope          Scope                    `json:"scope"`
	Authority      LeaseBinding             `json:"authority"`
	IdempotencyKey string                   `json:"idempotency_key"`
	CreatedAt      time.Time                `json:"created_at"`
	Deadline       time.Time                `json:"deadline"`
}

func (value PlanBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("execution id", string(value.ID)) != nil ||
		token("organization id", string(value.OrganizationID)) != nil ||
		token("initiative id", value.InitiativeID) != nil ||
		token("work order id", string(value.WorkOrderID)) != nil || value.WorkOrderHash.Validate() != nil ||
		value.Scope.Validate() != nil || value.Authority.Validate() != nil ||
		value.Authority.OrganizationID != value.OrganizationID ||
		token("idempotency key", value.IdempotencyKey) != nil || !validUTC(value.CreatedAt) ||
		!validUTC(value.Deadline) || !value.Deadline.After(value.CreatedAt) ||
		value.Deadline.Sub(value.CreatedAt) > 90*24*time.Hour ||
		value.Scope.Gate.OrganizationID != value.OrganizationID ||
		value.Scope.Gate.InitiativeID != value.InitiativeID ||
		value.Scope.Gate.Purpose != businessoutcome.GateBusinessSuccess ||
		(value.Scope.Gate.OutcomeKind != businessoutcome.OutcomeCommercial &&
			value.Scope.Gate.OutcomeKind != businessoutcome.OutcomeEconomic) {
		return fmt.Errorf("commercial execution: plan identity, scope, or chronology is invalid")
	}
	return nil
}

type Plan struct {
	Body      PlanBody            `json:"body"`
	Signature contracts.Signature `json:"signature"`
}

func (value Plan) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("commercial execution: signed plan is invalid")
	}
	return nil
}

type EvidenceBody struct {
	SchemaVersion      string                   `json:"schema_version"`
	ID                 EvidenceID               `json:"evidence_id"`
	ExecutionID        ExecutionID              `json:"execution_id"`
	OrganizationID     contracts.OrganizationID `json:"organization_id"`
	InitiativeID       string                   `json:"initiative_id"`
	WorkOrderID        contracts.WorkOrderID    `json:"work_order_id"`
	WorkOrderHash      contracts.ContentHash    `json:"work_order_hash"`
	Phase              Phase                    `json:"phase"`
	Attempt            uint32                   `json:"attempt"`
	Disposition        State                    `json:"disposition"`
	Authority          LeaseBinding             `json:"authority"`
	SubjectHash        contracts.ContentHash    `json:"subject_hash"`
	Sources            []SourceRef              `json:"sources"`
	PreviousEvidenceID *EvidenceID              `json:"previous_evidence_id"`
	PreviousHash       *contracts.ContentHash   `json:"previous_hash"`
	KnownGaps          []string                 `json:"known_gaps"`
	ReasonCode         string                   `json:"reason_code"`
	IdempotencyKey     string                   `json:"idempotency_key"`
	ObservedAt         time.Time                `json:"observed_at"`
	CapturedAt         time.Time                `json:"captured_at"`
}

func (value EvidenceBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("evidence id", string(value.ID)) != nil ||
		token("execution id", string(value.ExecutionID)) != nil ||
		token("organization id", string(value.OrganizationID)) != nil ||
		token("initiative id", value.InitiativeID) != nil || token("work order id", string(value.WorkOrderID)) != nil ||
		value.WorkOrderHash.Validate() != nil || !value.Phase.Valid() || value.Attempt == 0 ||
		!value.Disposition.Valid() || value.Authority.Validate() != nil ||
		value.Authority.OrganizationID != value.OrganizationID || value.SubjectHash.Validate() != nil ||
		len(value.Sources) == 0 || len(value.Sources) > 64 || len(value.KnownGaps) > 64 ||
		token("idempotency key", value.IdempotencyKey) != nil || !validUTC(value.ObservedAt) ||
		!validUTC(value.CapturedAt) || value.CapturedAt.Before(value.ObservedAt) ||
		value.CapturedAt.Sub(value.ObservedAt) > 30*24*time.Hour {
		return fmt.Errorf("commercial execution: evidence identity or chronology is invalid")
	}
	previous := ""
	for _, source := range value.Sources {
		key := sourceKey(source)
		if source.Validate() != nil || previous != "" && key <= previous || source.ObservedAt.After(value.CapturedAt) {
			return fmt.Errorf("commercial execution: evidence sources must be valid, ordered, and unique")
		}
		previous = key
	}
	if (value.PreviousEvidenceID == nil) != (value.PreviousHash == nil) {
		return fmt.Errorf("commercial execution: prior evidence identity is incomplete")
	}
	if value.PreviousEvidenceID != nil && (token("previous evidence id", string(*value.PreviousEvidenceID)) != nil ||
		value.PreviousHash.Validate() != nil || *value.PreviousEvidenceID == value.ID) {
		return fmt.Errorf("commercial execution: prior evidence identity is invalid")
	}
	if !sortedUniqueText(value.KnownGaps, 1024) {
		return fmt.Errorf("commercial execution: evidence gaps must be sorted and unique")
	}
	if value.Disposition == StateCompleted {
		if value.ReasonCode != "" || len(value.KnownGaps) != 0 || !hasRequiredRoles(value.Phase, value.Sources) {
			return fmt.Errorf("commercial execution: completed phase lacks its authoritative evidence chain")
		}
	} else if token("reason code", value.ReasonCode) != nil || len(value.KnownGaps) == 0 {
		return fmt.Errorf("commercial execution: non-completed evidence requires an honest reason and known gap")
	}
	return nil
}

type Evidence struct {
	Body      EvidenceBody        `json:"body"`
	Signature contracts.Signature `json:"signature"`
}

func (value Evidence) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("commercial execution: signed evidence is invalid")
	}
	return nil
}

type CorrectionKind string

const (
	CorrectionInvalidate CorrectionKind = "invalidate"
	CorrectionSupersede  CorrectionKind = "supersede"
	CorrectionRetry      CorrectionKind = "retry"
	CorrectionCompensate CorrectionKind = "compensate"
)

func (value CorrectionKind) Valid() bool {
	return value == CorrectionInvalidate || value == CorrectionSupersede ||
		value == CorrectionRetry || value == CorrectionCompensate
}

type CorrectionBody struct {
	SchemaVersion    string                   `json:"schema_version"`
	ID               CorrectionID             `json:"correction_id"`
	ExecutionID      ExecutionID              `json:"execution_id"`
	OrganizationID   contracts.OrganizationID `json:"organization_id"`
	InitiativeID     string                   `json:"initiative_id"`
	TargetPhase      Phase                    `json:"target_phase"`
	TargetEvidenceID EvidenceID               `json:"target_evidence_id"`
	TargetHash       contracts.ContentHash    `json:"target_hash"`
	Kind             CorrectionKind           `json:"kind"`
	Sources          []SourceRef              `json:"sources"`
	Authority        LeaseBinding             `json:"authority"`
	Reason           string                   `json:"reason"`
	IdempotencyKey   string                   `json:"idempotency_key"`
	IssuedAt         time.Time                `json:"issued_at"`
}

func (value CorrectionBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("correction id", string(value.ID)) != nil ||
		token("execution id", string(value.ExecutionID)) != nil ||
		token("organization id", string(value.OrganizationID)) != nil ||
		token("initiative id", value.InitiativeID) != nil || !value.TargetPhase.Valid() ||
		token("target evidence id", string(value.TargetEvidenceID)) != nil || value.TargetHash.Validate() != nil ||
		!value.Kind.Valid() || len(value.Sources) == 0 || len(value.Sources) > 32 ||
		value.Authority.Validate() != nil || value.Authority.OrganizationID != value.OrganizationID ||
		bounded("correction reason", value.Reason) != nil || token("idempotency key", value.IdempotencyKey) != nil ||
		!validUTC(value.IssuedAt) {
		return fmt.Errorf("commercial execution: correction is invalid")
	}
	previous := ""
	for _, source := range value.Sources {
		key := sourceKey(source)
		if source.Validate() != nil || previous != "" && key <= previous {
			return fmt.Errorf("commercial execution: correction sources are invalid")
		}
		previous = key
	}
	return nil
}

type Correction struct {
	Body      CorrectionBody      `json:"body"`
	Signature contracts.Signature `json:"signature"`
}

func (value Correction) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("commercial execution: signed correction is invalid")
	}
	return nil
}

type RecoveryStrategy string

const (
	RecoveryRetry      RecoveryStrategy = "retry"
	RecoveryReconcile  RecoveryStrategy = "reconcile"
	RecoveryCompensate RecoveryStrategy = "compensate"
)

func (value RecoveryStrategy) Valid() bool {
	return value == RecoveryRetry || value == RecoveryReconcile || value == RecoveryCompensate
}

type RecoveryBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             RecoveryID               `json:"recovery_id"`
	ExecutionID    ExecutionID              `json:"execution_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   string                   `json:"initiative_id"`
	TargetPhase    Phase                    `json:"target_phase"`
	CorrectionID   CorrectionID             `json:"correction_id"`
	Strategy       RecoveryStrategy         `json:"strategy"`
	Authority      LeaseBinding             `json:"authority"`
	IdempotencyKey string                   `json:"idempotency_key"`
	IssuedAt       time.Time                `json:"issued_at"`
}

func (value RecoveryBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("recovery id", string(value.ID)) != nil ||
		token("execution id", string(value.ExecutionID)) != nil ||
		token("organization id", string(value.OrganizationID)) != nil ||
		token("initiative id", value.InitiativeID) != nil || !value.TargetPhase.Valid() ||
		token("correction id", string(value.CorrectionID)) != nil || !value.Strategy.Valid() ||
		value.Authority.Validate() != nil || value.Authority.OrganizationID != value.OrganizationID ||
		token("idempotency key", value.IdempotencyKey) != nil || !validUTC(value.IssuedAt) {
		return fmt.Errorf("commercial execution: recovery is invalid")
	}
	return nil
}

type Recovery struct {
	Body      RecoveryBody        `json:"body"`
	Signature contracts.Signature `json:"signature"`
}

func (value Recovery) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("commercial execution: signed recovery is invalid")
	}
	return nil
}

type StepView struct {
	Phase            Phase       `json:"phase"`
	Ordinal          uint8       `json:"ordinal"`
	State            State       `json:"state"`
	Attempt          uint32      `json:"attempt"`
	ActiveEvidenceID *EvidenceID `json:"active_evidence_id"`
	UpdatedAt        time.Time   `json:"updated_at"`
}

type Snapshot struct {
	Plan         Plan       `json:"plan"`
	State        State      `json:"state"`
	CurrentPhase Phase      `json:"current_phase"`
	Version      uint64     `json:"version"`
	Steps        []StepView `json:"steps"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type IncidentView struct {
	ID          string      `json:"id"`
	ExecutionID ExecutionID `json:"execution_id"`
	Phase       Phase       `json:"phase"`
	Kind        string      `json:"kind"`
	SafeCode    string      `json:"safe_code"`
	State       string      `json:"state"`
	CreatedAt   time.Time   `json:"created_at"`
	ResolvedAt  *time.Time  `json:"resolved_at"`
}

type MeasurementProof struct {
	Requirement  businessoutcome.GateRequirement `json:"requirement"`
	Decision     businessoutcome.GateDecision    `json:"decision"`
	Outcome      businessoutcome.VerifiedOutcome `json:"outcome"`
	Observations []businessoutcome.Observation   `json:"observations"`
	Sources      []SourceRef                     `json:"sources"`
}

func (value MeasurementProof) Validate() error {
	if value.Requirement.Validate() != nil || value.Decision.Validate() != nil ||
		value.Decision.State != businessoutcome.GateSatisfied ||
		!sameGateRequirement(value.Requirement, value.Decision.Requirement) ||
		value.Outcome.ValidateAt(value.Decision.EvaluatedAt) != nil {
		return fmt.Errorf("commercial execution: measurement proof is not gate-safe")
	}
	recordHash, err := businessoutcome.OutcomeRecordHash(value.Outcome.Record)
	body := value.Outcome.Record.Body
	if err != nil || recordHash != value.Decision.OutcomeHash || body.ID != value.Requirement.OutcomeID ||
		body.OrganizationID != value.Requirement.OrganizationID ||
		body.InitiativeID != value.Requirement.InitiativeID || body.Metric != value.Requirement.Metric ||
		body.Kind != value.Requirement.OutcomeKind ||
		!sameObservationBindings(body.Observations, value.Decision.Observations) ||
		len(value.Observations) != len(value.Decision.Observations) || len(value.Observations) == 0 {
		return fmt.Errorf("commercial execution: measurement outcome lineage is incomplete")
	}
	observationSources := make(map[string]SourceRef, len(value.Observations))
	var metricSource, outcomeSource, gateSource *SourceRef
	previous := ""
	for index := range value.Sources {
		source := value.Sources[index]
		key := sourceKey(source)
		if source.Validate() != nil || previous != "" && key <= previous || !sourceCompletes(source) {
			return fmt.Errorf("commercial execution: measurement sources are invalid")
		}
		previous = key
		switch source.Role {
		case RoleMetricDefinition:
			if metricSource != nil {
				return fmt.Errorf("commercial execution: measurement metric source is duplicated")
			}
			metricSource = &value.Sources[index]
		case RoleMetricObservation:
			observationSources[source.RecordID] = source
		case RoleCommercialOutcome:
			if outcomeSource != nil {
				return fmt.Errorf("commercial execution: measurement outcome source is duplicated")
			}
			outcomeSource = &value.Sources[index]
		case RoleBusinessGate:
			if gateSource != nil {
				return fmt.Errorf("commercial execution: measurement gate source is duplicated")
			}
			gateSource = &value.Sources[index]
		default:
			return fmt.Errorf("commercial execution: measurement proof contains an unrelated source")
		}
	}
	if metricSource == nil || outcomeSource == nil || gateSource == nil ||
		metricSource.RecordID != string(value.Requirement.Metric.ID) ||
		metricSource.Version != value.Requirement.Metric.Version ||
		metricSource.Hash != value.Requirement.Metric.DefinitionHash ||
		outcomeSource.RecordID != string(body.ID) || outcomeSource.Version != body.Version ||
		outcomeSource.Hash != recordHash || gateSource.RecordID != string(value.Requirement.ID) ||
		gateSource.Hash != value.Decision.DecisionHash {
		return fmt.Errorf("commercial execution: measurement sources do not bind the gate identity")
	}
	for index, observation := range value.Observations {
		binding := value.Decision.Observations[index]
		if observation.Validate() != nil || observation.Body.ID != binding.ID ||
			observation.ContentHash != binding.Hash {
			return fmt.Errorf("commercial execution: measurement observation is not exact")
		}
		source, found := observationSources[string(binding.ID)]
		if !found || source.Hash != binding.Hash || !source.ObservedAt.Equal(observation.Body.ObservedAt) {
			return fmt.Errorf("commercial execution: measurement observation source is missing")
		}
	}
	if len(observationSources) != len(value.Observations) || !hasRequiredRoles(PhaseMeasurement, value.Sources) {
		return fmt.Errorf("commercial execution: measurement proof is incomplete")
	}
	return nil
}

func hasRequiredRoles(phase Phase, sources []SourceRef) bool {
	required := map[Phase][]SourceRole{
		PhaseAcquisition:             {RoleLaunchedProduct, RoleAcquisitionEvent},
		PhaseCustomerQualification:   {RoleQualifiedCustomer, RoleCRMRecord},
		PhaseSale:                    {RoleSalesOrder, RoleContract},
		PhaseFinancialIntent:         {RoleFinancialIntent},
		PhaseFinancialReconciliation: {RoleFinancialSettlement, RoleBalancedAccounting},
		PhaseSupport:                 {RoleSupportCycle},
		PhaseMeasurement:             {RoleMetricDefinition, RoleMetricObservation, RoleCommercialOutcome, RoleBusinessGate},
	}
	seen := make(map[SourceRole]bool, len(sources))
	for _, source := range sources {
		seen[source.Role] = true
	}
	for _, role := range required[phase] {
		if !seen[role] {
			return false
		}
	}
	return true
}

func sourceKey(value SourceRef) string {
	return string(value.Role) + "|" + string(value.Kind) + "|" + value.RecordID + "|" + fmt.Sprint(value.Version)
}

func policyKey(value contracts.PolicyRef) string {
	return string(value.ID) + "|" + fmt.Sprint(value.Version) + "|" + value.Hash.Digest
}

func slicesContain(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	return index < len(values) && values[index] == target
}

func token(name, value string) error {
	if strings.TrimSpace(value) != value || value == "" || len(value) > 128 {
		return fmt.Errorf("%s must contain 1 to 128 bytes", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' ||
			character == '.' || character == ':' || character == '/' {
			continue
		}
		return fmt.Errorf("%s contains an invalid character", name)
	}
	return nil
}

func bounded(name, value string) error {
	if strings.TrimSpace(value) != value || value == "" || len(value) > 4096 ||
		strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func sortedUniqueText(values []string, maximum int) bool {
	if len(values) > 64 || !sort.StringsAreSorted(values) {
		return false
	}
	for index, value := range values {
		if strings.TrimSpace(value) != value || value == "" || len(value) > maximum ||
			strings.ContainsAny(value, "\r\n\x00") || index > 0 && values[index-1] == value {
			return false
		}
	}
	return true
}

func validUTC(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }
