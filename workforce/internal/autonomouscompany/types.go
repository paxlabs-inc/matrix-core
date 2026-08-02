// Package autonomouscompany records the cumulative autonomous-company release
// property and coordinates the evidence-backed next company cycle.
package autonomouscompany

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

const (
	PropertySchemaVersion = "workforce.autonomous-company-property.v1"
	EvidenceSchemaVersion = "workforce.autonomous-company-evidence.v1"
	ProcessSchemaVersion  = "workforce.autonomous-company-process.v1"
	LineageSchemaVersion  = "workforce.autonomous-company-lineage.v1"
)

type PropertyKind string

const (
	PropertyCompanyControl      PropertyKind = "COMPANY-CONTROL"
	PropertyProductExecution    PropertyKind = "PRODUCT-EXECUTION"
	PropertyCommercialExecution PropertyKind = "COMMERCIAL-EXECUTION"
	PropertyAutonomousCompany   PropertyKind = "AUTONOMOUS-COMPANY"
)

var allPropertyKinds = []PropertyKind{
	PropertyAutonomousCompany,
	PropertyCommercialExecution,
	PropertyCompanyControl,
	PropertyProductExecution,
}

func AllPropertyKinds() []PropertyKind { return append([]PropertyKind(nil), allPropertyKinds...) }

func (value PropertyKind) Valid() bool { return slices.Contains(allPropertyKinds, value) }

type PropertyState string

const (
	StatePending   PropertyState = "pending"
	StateRunning   PropertyState = "running"
	StateBlocked   PropertyState = "blocked"
	StatePassed    PropertyState = "passed"
	StateFailed    PropertyState = "failed"
	StateUncertain PropertyState = "uncertain"
)

func (value PropertyState) Valid() bool {
	switch value {
	case StatePending, StateRunning, StateBlocked, StatePassed, StateFailed, StateUncertain:
		return true
	default:
		return false
	}
}

type EvidenceKind string

const (
	EvidenceMissionAuthority           EvidenceKind = "mission_authority"
	EvidenceCompanyFunding             EvidenceKind = "company_funding"
	EvidenceCompanyLifecycle           EvidenceKind = "company_lifecycle"
	EvidenceMailReceipt                EvidenceKind = "mail_receipt"
	EvidenceApprovalReceipt            EvidenceKind = "approval_receipt"
	EvidenceProductExecution           EvidenceKind = "product_execution"
	EvidenceDeploymentReceipt          EvidenceKind = "deployment_receipt"
	EvidenceIndependentAudit           EvidenceKind = "independent_audit"
	EvidenceCommercialExecution        EvidenceKind = "commercial_execution"
	EvidenceCustomerTransaction        EvidenceKind = "customer_transaction"
	EvidenceCustomerOperation          EvidenceKind = "customer_operation"
	EvidenceFinancialReconciliation    EvidenceKind = "financial_reconciliation"
	EvidenceBusinessOutcome            EvidenceKind = "business_outcome"
	EvidenceLearningConclusion         EvidenceKind = "learning_conclusion"
	EvidenceCompanyControlProperty     EvidenceKind = "company_control_property"
	EvidenceProductExecutionProperty   EvidenceKind = "product_execution_property"
	EvidenceCommercialProperty         EvidenceKind = "commercial_execution_property"
	EvidenceSecurityQualification      EvidenceKind = "security_qualification"
	EvidenceRecoveryQualification      EvidenceKind = "recovery_qualification"
	EvidenceCleanRestoreReceipt        EvidenceKind = "clean_restore_receipt"
	EvidenceExternalAmbiguityReceipt   EvidenceKind = "external_ambiguity_receipt"
	EvidenceFinancialAmbiguityReceipt  EvidenceKind = "financial_ambiguity_receipt"
	EvidenceCorrectionReceipt          EvidenceKind = "correction_receipt"
	EvidenceCrossAuditReceipt          EvidenceKind = "cross_audit_receipt"
	EvidenceOfflineCoalescingReceipt   EvidenceKind = "offline_coalescing_receipt"
	EvidenceRestartReceipt             EvidenceKind = "restart_receipt"
	EvidenceFreshProcessReceipt        EvidenceKind = "fresh_process_receipt"
	EvidenceMemorylessAuditorReceipt   EvidenceKind = "memoryless_auditor_receipt"
	EvidenceFounderProjectionReceipt   EvidenceKind = "founder_ui_projection_receipt"
	EvidenceNextCycleDispatchReceipt   EvidenceKind = "next_cycle_dispatch_receipt"
	EvidenceNextCycleCompletionReceipt EvidenceKind = "next_cycle_completion_receipt"
)

var allEvidenceKinds = []EvidenceKind{
	EvidenceApprovalReceipt,
	EvidenceBusinessOutcome,
	EvidenceCleanRestoreReceipt,
	EvidenceCommercialExecution,
	EvidenceCommercialProperty,
	EvidenceCompanyControlProperty,
	EvidenceCompanyFunding,
	EvidenceCompanyLifecycle,
	EvidenceCorrectionReceipt,
	EvidenceCrossAuditReceipt,
	EvidenceCustomerOperation,
	EvidenceCustomerTransaction,
	EvidenceDeploymentReceipt,
	EvidenceExternalAmbiguityReceipt,
	EvidenceFinancialAmbiguityReceipt,
	EvidenceFinancialReconciliation,
	EvidenceFreshProcessReceipt,
	EvidenceFounderProjectionReceipt,
	EvidenceIndependentAudit,
	EvidenceLearningConclusion,
	EvidenceMailReceipt,
	EvidenceMemorylessAuditorReceipt,
	EvidenceMissionAuthority,
	EvidenceNextCycleCompletionReceipt,
	EvidenceNextCycleDispatchReceipt,
	EvidenceOfflineCoalescingReceipt,
	EvidenceProductExecution,
	EvidenceProductExecutionProperty,
	EvidenceRecoveryQualification,
	EvidenceRestartReceipt,
	EvidenceSecurityQualification,
}

func AllEvidenceKinds() []EvidenceKind { return append([]EvidenceKind(nil), allEvidenceKinds...) }

func (value EvidenceKind) Valid() bool { return slices.Contains(allEvidenceKinds, value) }

func RequiredEvidenceKinds(kind PropertyKind) []EvidenceKind {
	return append([]EvidenceKind(nil), requiredEvidence(kind)...)
}

type ReconciliationState string

const (
	ReconciliationNotApplicable ReconciliationState = "not_applicable"
	ReconciliationReconciled    ReconciliationState = "reconciled"
	ReconciliationUnreconciled  ReconciliationState = "unreconciled"
	ReconciliationAmbiguous     ReconciliationState = "ambiguous"
)

func (value ReconciliationState) Valid() bool {
	switch value {
	case ReconciliationNotApplicable, ReconciliationReconciled,
		ReconciliationUnreconciled, ReconciliationAmbiguous:
		return true
	default:
		return false
	}
}

type EvidenceBinding struct {
	SchemaVersion  string                   `json:"schema_version"`
	Kind           EvidenceKind             `json:"kind"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   string                   `json:"initiative_id"`
	RecordID       string                   `json:"record_id"`
	RecordVersion  uint64                   `json:"record_version"`
	RecordHash     contracts.ContentHash    `json:"record_hash"`
	Authority      string                   `json:"authority"`
	SourceState    string                   `json:"source_state"`
	Validity       contracts.Validity       `json:"validity"`
	Reconciliation ReconciliationState      `json:"reconciliation"`
	Contaminated   bool                     `json:"contaminated"`
	ObservedAt     time.Time                `json:"observed_at"`
	FreshUntil     time.Time                `json:"fresh_until"`
}

func (value EvidenceBinding) Validate() error {
	if value.SchemaVersion != EvidenceSchemaVersion || !value.Kind.Valid() ||
		token(string(value.OrganizationID)) != nil || token(value.InitiativeID) != nil ||
		token(value.RecordID) != nil || value.RecordVersion == 0 ||
		token(value.Authority) != nil || token(value.SourceState) != nil ||
		!value.Validity.Valid() || !value.Reconciliation.Valid() ||
		!utc(value.ObservedAt) || !utc(value.FreshUntil) ||
		!value.FreshUntil.After(value.ObservedAt) {
		return fmt.Errorf("autonomous company: evidence binding is invalid")
	}
	if err := value.RecordHash.Validate(); err != nil {
		return fmt.Errorf("autonomous company: evidence record hash: %w", err)
	}
	return nil
}

func (value EvidenceBinding) currentAt(at time.Time) bool {
	return value.Validity == contracts.ValidityActive && !value.Contaminated &&
		value.Reconciliation != ReconciliationUnreconciled &&
		value.Reconciliation != ReconciliationAmbiguous &&
		!value.ObservedAt.After(at) && value.FreshUntil.After(at)
}

type ProcessRole string

const (
	ProcessExecutor ProcessRole = "executor"
	ProcessAuditor  ProcessRole = "auditor"
)

type ProcessIdentity struct {
	SchemaVersion   string                   `json:"schema_version"`
	OrganizationID  contracts.OrganizationID `json:"organization_id"`
	ProcessID       string                   `json:"process_id"`
	WakeID          string                   `json:"wake_id"`
	SeatID          contracts.SeatID         `json:"seat_id"`
	DepartmentID    contracts.DepartmentID   `json:"department_id"`
	Role            ProcessRole              `json:"role"`
	Memoryless      bool                     `json:"memoryless"`
	FreshProcess    bool                     `json:"fresh_process"`
	EvidenceID      string                   `json:"evidence_id"`
	EvidenceVersion uint64                   `json:"evidence_version"`
	EvidenceHash    contracts.ContentHash    `json:"evidence_hash"`
	StartedAt       time.Time                `json:"started_at"`
	ObservedAt      time.Time                `json:"observed_at"`
}

func (value ProcessIdentity) Validate() error {
	if value.SchemaVersion != ProcessSchemaVersion ||
		token(string(value.OrganizationID)) != nil || token(value.ProcessID) != nil ||
		token(value.WakeID) != nil || token(string(value.SeatID)) != nil ||
		token(string(value.DepartmentID)) != nil ||
		(value.Role != ProcessExecutor && value.Role != ProcessAuditor) ||
		token(value.EvidenceID) != nil || value.EvidenceVersion == 0 || !utc(value.StartedAt) ||
		!utc(value.ObservedAt) || value.ObservedAt.Before(value.StartedAt) {
		return fmt.Errorf("autonomous company: process identity is invalid")
	}
	if value.Role == ProcessAuditor && !value.Memoryless {
		return fmt.Errorf("autonomous company: auditor process is not memoryless")
	}
	return value.EvidenceHash.Validate()
}

type LineageStage string

const (
	LineageMission    LineageStage = "mission"
	LineageFunding    LineageStage = "funding"
	LineageProduct    LineageStage = "product"
	LineageCommercial LineageStage = "commercial"
	LineageOutcome    LineageStage = "outcome"
	LineageLearning   LineageStage = "learning"
	LineageNextCycle  LineageStage = "next_cycle"
)

var completeLineage = []LineageStage{
	LineageMission,
	LineageFunding,
	LineageProduct,
	LineageCommercial,
	LineageOutcome,
	LineageLearning,
	LineageNextCycle,
}

func CompleteLineageStages() []LineageStage {
	return append([]LineageStage(nil), completeLineage...)
}

type LineageNode struct {
	SchemaVersion string                `json:"schema_version"`
	Position      uint16                `json:"position"`
	Stage         LineageStage          `json:"stage"`
	RecordID      string                `json:"record_id"`
	RecordVersion uint64                `json:"record_version"`
	RecordHash    contracts.ContentHash `json:"record_hash"`
	ObservedAt    time.Time             `json:"observed_at"`
}

func (value LineageNode) Validate() error {
	if value.SchemaVersion != LineageSchemaVersion || value.Position == 0 ||
		!slices.Contains(completeLineage, value.Stage) || token(value.RecordID) != nil ||
		value.RecordVersion == 0 || !utc(value.ObservedAt) {
		return fmt.Errorf("autonomous company: lineage node is invalid")
	}
	return value.RecordHash.Validate()
}

type PropertyDraft struct {
	ID             string                   `json:"property_id"`
	Kind           PropertyKind             `json:"property_kind"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   string                   `json:"initiative_id"`
	State          PropertyState            `json:"state"`
	Evidence       []EvidenceBinding        `json:"evidence"`
	Processes      []ProcessIdentity        `json:"processes"`
	Lineage        []LineageNode            `json:"lineage"`
	ReasonCodes    []string                 `json:"reason_codes"`
	StartedAt      time.Time                `json:"started_at"`
	EvaluatedAt    time.Time                `json:"evaluated_at"`
	FreshUntil     time.Time                `json:"fresh_until"`
	CompletedAt    *time.Time               `json:"completed_at"`
	IdempotencyKey string                   `json:"idempotency_key"`
}

func (value PropertyDraft) Validate() error {
	return validateProperty(
		value.ID, value.Kind, value.OrganizationID, value.InitiativeID, value.State,
		value.Evidence, value.Processes, value.Lineage, value.ReasonCodes,
		value.StartedAt, value.EvaluatedAt, value.FreshUntil, value.CompletedAt,
		value.IdempotencyKey,
	)
}

type PropertyRecord struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"property_id"`
	Version        uint64                   `json:"version"`
	Kind           PropertyKind             `json:"property_kind"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   string                   `json:"initiative_id"`
	State          PropertyState            `json:"state"`
	Evidence       []EvidenceBinding        `json:"evidence"`
	Processes      []ProcessIdentity        `json:"processes"`
	Lineage        []LineageNode            `json:"lineage"`
	ReasonCodes    []string                 `json:"reason_codes"`
	StartedAt      time.Time                `json:"started_at"`
	EvaluatedAt    time.Time                `json:"evaluated_at"`
	FreshUntil     time.Time                `json:"fresh_until"`
	CompletedAt    *time.Time               `json:"completed_at"`
	IdempotencyKey string                   `json:"idempotency_key"`
	ContentHash    contracts.ContentHash    `json:"content_hash"`
	Signature      contracts.Signature      `json:"signature"`
}

func (value PropertyRecord) Validate() error {
	if value.SchemaVersion != PropertySchemaVersion || value.Version == 0 {
		return fmt.Errorf("autonomous company: property record identity is invalid")
	}
	if err := validateProperty(
		value.ID, value.Kind, value.OrganizationID, value.InitiativeID, value.State,
		value.Evidence, value.Processes, value.Lineage, value.ReasonCodes,
		value.StartedAt, value.EvaluatedAt, value.FreshUntil, value.CompletedAt,
		value.IdempotencyKey,
	); err != nil {
		return err
	}
	if err := value.ContentHash.Validate(); err != nil {
		return err
	}
	return value.Signature.Validate()
}

type PropertySnapshot struct {
	Record        PropertyRecord        `json:"record"`
	CanonicalHash contracts.ContentHash `json:"canonical_hash"`
}

type ReleasePropertySet struct {
	CompanyControl      PropertySnapshot `json:"company_control"`
	ProductExecution    PropertySnapshot `json:"product_execution"`
	CommercialExecution PropertySnapshot `json:"commercial_execution"`
	AutonomousCompany   PropertySnapshot `json:"autonomous_company"`
}

func validateProperty(
	id string,
	kind PropertyKind,
	organizationID contracts.OrganizationID,
	initiativeID string,
	state PropertyState,
	evidence []EvidenceBinding,
	processes []ProcessIdentity,
	lineage []LineageNode,
	reasonCodes []string,
	startedAt, evaluatedAt, freshUntil time.Time,
	completedAt *time.Time,
	idempotencyKey string,
) error {
	if token(id) != nil || !kind.Valid() || token(string(organizationID)) != nil ||
		token(initiativeID) != nil || !state.Valid() || token(idempotencyKey) != nil ||
		!utc(startedAt) || !utc(evaluatedAt) || !utc(freshUntil) ||
		evaluatedAt.Before(startedAt) || !freshUntil.After(evaluatedAt) ||
		len(evidence) > 256 || len(processes) > 128 || len(lineage) > 64 ||
		!sortedTokens(reasonCodes) {
		return fmt.Errorf("autonomous company: property record is invalid")
	}
	terminal := state == StatePassed || state == StateFailed
	if terminal != (completedAt != nil) {
		return fmt.Errorf("autonomous company: property completion does not match state")
	}
	if completedAt != nil && (!utc(*completedAt) || completedAt.Before(evaluatedAt)) {
		return fmt.Errorf("autonomous company: property completion time is invalid")
	}
	if (state == StateBlocked || state == StateFailed || state == StateUncertain) &&
		len(reasonCodes) == 0 {
		return fmt.Errorf("autonomous company: non-success state requires a reason code")
	}
	if state != StatePending && len(evidence) == 0 {
		return fmt.Errorf("autonomous company: evaluated property requires evidence")
	}
	previous := ""
	for index, binding := range evidence {
		if binding.OrganizationID != organizationID || binding.InitiativeID != initiativeID ||
			binding.Validate() != nil {
			return fmt.Errorf("autonomous company: evidence %d is invalid", index)
		}
		key := string(binding.Kind) + "\x00" + binding.RecordID + "\x00" +
			fmt.Sprintf("%020d", binding.RecordVersion)
		if key <= previous {
			return fmt.Errorf("autonomous company: evidence must be sorted and unique")
		}
		previous = key
	}
	previous = ""
	for index, process := range processes {
		if process.OrganizationID != organizationID || process.Validate() != nil ||
			process.ProcessID <= previous ||
			!processIdentityMatchesEvidence(process, evidence) {
			return fmt.Errorf("autonomous company: process identity %d is invalid or unsorted", index)
		}
		previous = process.ProcessID
	}
	for index, node := range lineage {
		if node.Validate() != nil || node.Position != uint16(index+1) ||
			node.ObservedAt.After(evaluatedAt) {
			return fmt.Errorf("autonomous company: lineage node %d is invalid", index)
		}
	}
	if state == StatePassed {
		if err := validatePassed(kind, evidence, processes, lineage, evaluatedAt); err != nil {
			return err
		}
	}
	return nil
}

func validatePassed(
	kind PropertyKind,
	evidence []EvidenceBinding,
	processes []ProcessIdentity,
	lineage []LineageNode,
	evaluatedAt time.Time,
) error {
	present := make(map[EvidenceKind]bool, len(evidence))
	for _, binding := range evidence {
		if !binding.currentAt(evaluatedAt) {
			return fmt.Errorf("autonomous company: passed property contains stale, contaminated, or unreconciled evidence")
		}
		present[binding.Kind] = true
	}
	required := requiredEvidence(kind)
	for _, item := range required {
		if !present[item] {
			return fmt.Errorf("autonomous company: passed %s property lacks %s", kind, item)
		}
	}
	if kind != PropertyAutonomousCompany {
		return nil
	}
	if len(lineage) != len(completeLineage) {
		return fmt.Errorf("autonomous company: release lineage is incomplete")
	}
	for index, stage := range completeLineage {
		if lineage[index].Stage != stage || !lineageNodeMatchesEvidence(lineage[index], evidence) {
			return fmt.Errorf("autonomous company: release lineage is out of order")
		}
	}
	processIDs := make(map[string]bool, len(processes))
	seatIDs := make(map[contracts.SeatID]bool, len(processes))
	auditors := 0
	executors := 0
	for _, process := range processes {
		if !process.FreshProcess || process.ObservedAt.After(evaluatedAt) ||
			processIDs[process.ProcessID] {
			return fmt.Errorf("autonomous company: release process identity is not fresh and unique")
		}
		processIDs[process.ProcessID] = true
		seatIDs[process.SeatID] = true
		if process.Role == ProcessAuditor {
			auditors++
		} else {
			executors++
		}
	}
	if auditors < 2 || executors < 2 || len(processIDs) < 4 || len(seatIDs) < 4 {
		return fmt.Errorf("autonomous company: release lacks independent fresh executor and auditor processes")
	}
	return nil
}

func requiredEvidence(kind PropertyKind) []EvidenceKind {
	switch kind {
	case PropertyCompanyControl:
		return []EvidenceKind{
			EvidenceApprovalReceipt,
			EvidenceCompanyFunding,
			EvidenceCompanyLifecycle,
			EvidenceMailReceipt,
			EvidenceMissionAuthority,
		}
	case PropertyProductExecution:
		return []EvidenceKind{
			EvidenceDeploymentReceipt,
			EvidenceIndependentAudit,
			EvidenceProductExecution,
		}
	case PropertyCommercialExecution:
		return []EvidenceKind{
			EvidenceBusinessOutcome,
			EvidenceCommercialExecution,
			EvidenceCustomerOperation,
			EvidenceCustomerTransaction,
			EvidenceFinancialReconciliation,
		}
	case PropertyAutonomousCompany:
		return []EvidenceKind{
			EvidenceBusinessOutcome,
			EvidenceCleanRestoreReceipt,
			EvidenceCommercialExecution,
			EvidenceCommercialProperty,
			EvidenceCompanyControlProperty,
			EvidenceCompanyFunding,
			EvidenceCorrectionReceipt,
			EvidenceCrossAuditReceipt,
			EvidenceExternalAmbiguityReceipt,
			EvidenceFinancialAmbiguityReceipt,
			EvidenceFounderProjectionReceipt,
			EvidenceFreshProcessReceipt,
			EvidenceLearningConclusion,
			EvidenceMissionAuthority,
			EvidenceMemorylessAuditorReceipt,
			EvidenceNextCycleDispatchReceipt,
			EvidenceOfflineCoalescingReceipt,
			EvidenceProductExecution,
			EvidenceProductExecutionProperty,
			EvidenceRecoveryQualification,
			EvidenceRestartReceipt,
			EvidenceSecurityQualification,
		}
	default:
		return nil
	}
}

func processIdentityMatchesEvidence(
	process ProcessIdentity,
	evidence []EvidenceBinding,
) bool {
	kind := EvidenceFreshProcessReceipt
	sourceState := "goal_completed"
	if process.Role == ProcessAuditor {
		kind = EvidenceMemorylessAuditorReceipt
		sourceState = "pass"
	}
	for _, binding := range evidence {
		if binding.Kind == kind && binding.RecordID == process.EvidenceID &&
			binding.RecordVersion == process.EvidenceVersion &&
			binding.RecordHash == process.EvidenceHash &&
			binding.OrganizationID == process.OrganizationID &&
			binding.SourceState == sourceState &&
			binding.ObservedAt.Equal(process.ObservedAt) {
			return true
		}
	}
	return false
}

func lineageNodeMatchesEvidence(node LineageNode, evidence []EvidenceBinding) bool {
	kind := EvidenceMissionAuthority
	switch node.Stage {
	case LineageFunding:
		kind = EvidenceCompanyFunding
	case LineageProduct:
		kind = EvidenceProductExecution
	case LineageCommercial:
		kind = EvidenceCommercialExecution
	case LineageOutcome:
		kind = EvidenceBusinessOutcome
	case LineageLearning:
		kind = EvidenceLearningConclusion
	case LineageNextCycle:
		kind = EvidenceNextCycleDispatchReceipt
	}
	for _, binding := range evidence {
		if binding.Kind == kind && binding.RecordID == node.RecordID &&
			binding.RecordVersion == node.RecordVersion && binding.RecordHash == node.RecordHash &&
			binding.ObservedAt.Equal(node.ObservedAt) {
			return true
		}
	}
	return false
}

func sortedTokens(values []string) bool {
	if !slices.IsSorted(values) {
		return false
	}
	for index, value := range values {
		if token(value) != nil || index > 0 && value == values[index-1] {
			return false
		}
	}
	return true
}

func token(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 160 {
		return fmt.Errorf("autonomous company: identity is invalid")
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("-_.:/", character) {
			continue
		}
		return fmt.Errorf("autonomous company: identity is invalid")
	}
	return nil
}

func utc(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }
