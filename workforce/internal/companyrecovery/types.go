// Package companyrecovery owns company-wide resource admission, encrypted
// recovery artifacts, clean restore quarantine, retention, and offline merge.
package companyrecovery

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

const (
	SchemaVersion                      = "workforce.company-recovery.v1"
	ArchiveSchemaVersion               = "workforce.company-recovery-archive.v1"
	OfflineSchemaVersion               = "workforce.company-recovery-offline.v1"
	ShutdownSchemaVersion              = "workforce.company-recovery-shutdown.v1"
	RecoveryQualificationSchemaVersion = "workforce.company-recovery-qualification.v1"
	MaximumArchiveTables               = 512
	MaximumRowsPerTable                = 2_000_000
	MaximumOfflineBatch                = 2048
	MaximumStructuredItems             = 256
	MaximumDatabaseValue               = uint64(1<<63 - 1)
	MaximumLogicalArchiveBytes         = uint64(512 << 20)
)

var (
	ErrConflict               = errors.New("company recovery: immutable conflict")
	ErrNotFound               = errors.New("company recovery: record not found")
	ErrUnauthorized           = errors.New("company recovery: unauthorized")
	ErrLimitExceeded          = errors.New("company recovery: limit exceeded")
	ErrNoLimitPolicy          = errors.New("company recovery: limit policy unavailable")
	ErrCircuitOpen            = errors.New("company recovery: resource circuit open")
	ErrDraining               = errors.New("company recovery: company is draining")
	ErrRestoreQuarantined     = errors.New("company recovery: restore reconciliation required")
	ErrRestoreTargetNotClean  = errors.New("company recovery: restore target is not clean")
	ErrSchemaMismatch         = errors.New("company recovery: archive schema mismatch")
	ErrArchiveErased          = errors.New("company recovery: archive key is erased")
	ErrOfflineFork            = errors.New("company recovery: offline sequence fork")
	ErrReconciliationRequired = errors.New("company recovery: reconciliation required")
)

type LimitPolicyID string
type ReservationID string
type MetricID string
type TraceID string
type IncidentID string
type RecoveryPolicyID string
type BackupID string
type RestoreID string
type ErasureID string
type OfflineBatchID string
type MachineKeyID string
type ShutdownID string
type RecoveryQualificationID string

type ScopeKind string

const (
	ScopeCompany          ScopeKind = "company"
	ScopePortfolio        ScopeKind = "portfolio"
	ScopeInitiative       ScopeKind = "initiative"
	ScopeDepartment       ScopeKind = "department"
	ScopeSquad            ScopeKind = "squad"
	ScopeSeat             ScopeKind = "seat"
	ScopeBrowser          ScopeKind = "browser"
	ScopeCustomer         ScopeKind = "customer"
	ScopeFinancialAccount ScopeKind = "financial_account"
	ScopeMail             ScopeKind = "mail"
	ScopeStorage          ScopeKind = "storage"
	ScopeProcess          ScopeKind = "process"
	ScopeModel            ScopeKind = "model"
	ScopeEffect           ScopeKind = "effect"
)

func (value ScopeKind) Valid() bool {
	switch value {
	case ScopeCompany, ScopePortfolio, ScopeInitiative, ScopeDepartment, ScopeSquad,
		ScopeSeat, ScopeBrowser, ScopeCustomer, ScopeFinancialAccount, ScopeMail,
		ScopeStorage, ScopeProcess, ScopeModel, ScopeEffect:
		return true
	default:
		return false
	}
}

type ScopeRef struct {
	Kind ScopeKind `json:"kind"`
	ID   string    `json:"id"`
}

func (value ScopeRef) Validate() error {
	if !value.Kind.Valid() || validateToken("scope_id", value.ID) != nil {
		return fmt.Errorf("company recovery: scope is invalid")
	}
	return nil
}

type ResourceKind string

const (
	ResourceActiveInitiatives ResourceKind = "active_initiatives"
	ResourceActiveDepartments ResourceKind = "active_departments"
	ResourceActiveSeats       ResourceKind = "active_seats"
	ResourceActiveSquads      ResourceKind = "active_squads"
	ResourceActiveWakes       ResourceKind = "active_wakes"
	ResourceBrowserSessions   ResourceKind = "browser_sessions"
	ResourceModelCalls        ResourceKind = "model_calls"
	ResourceToolCalls         ResourceKind = "tool_calls"
	ResourceEffects           ResourceKind = "effects"
	ResourceFinancialExposure ResourceKind = "financial_exposure_microunits"
	ResourceMailMessages      ResourceKind = "mail_messages"
	ResourceStorageBytes      ResourceKind = "storage_bytes"
	ResourceLatencyMicros     ResourceKind = "latency_micros"
	ResourceMemoryBytes       ResourceKind = "memory_bytes"
	ResourceCPUMicros         ResourceKind = "cpu_micros"
	ResourceCostMicrounits    ResourceKind = "cost_microunits"
	ResourceCustomerMessages  ResourceKind = "customer_messages"
	ResourceProcesses         ResourceKind = "processes"
	ResourceQueueDepth        ResourceKind = "queue_depth"
)

func (value ResourceKind) Valid() bool {
	switch value {
	case ResourceActiveInitiatives, ResourceActiveDepartments, ResourceActiveSeats,
		ResourceActiveSquads, ResourceActiveWakes, ResourceBrowserSessions,
		ResourceModelCalls, ResourceToolCalls, ResourceEffects,
		ResourceFinancialExposure, ResourceMailMessages, ResourceStorageBytes,
		ResourceLatencyMicros, ResourceMemoryBytes, ResourceCPUMicros,
		ResourceCostMicrounits, ResourceCustomerMessages, ResourceProcesses,
		ResourceQueueDepth:
		return true
	default:
		return false
	}
}

type LimitPolicyBody struct {
	SchemaVersion           string                   `json:"schema_version"`
	ID                      LimitPolicyID            `json:"policy_id"`
	Version                 uint64                   `json:"version"`
	OrganizationID          contracts.OrganizationID `json:"organization_id"`
	Scope                   ScopeRef                 `json:"scope"`
	Resource                ResourceKind             `json:"resource"`
	SoftLimit               uint64                   `json:"soft_limit"`
	HardLimit               uint64                   `json:"hard_limit"`
	Window                  time.Duration            `json:"window"`
	MaximumReservationAge   time.Duration            `json:"maximum_reservation_age"`
	OpenCircuitOnExhaustion bool                     `json:"open_circuit_on_exhaustion"`
	EffectiveAt             time.Time                `json:"effective_at"`
	ExpiresAt               time.Time                `json:"expires_at"`
	Supersedes              *contracts.ContentHash   `json:"supersedes"`
}

func (value LimitPolicyBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("policy_id", string(value.ID)) != nil ||
		value.Version == 0 || value.Version > MaximumDatabaseValue || validateToken("organization_id", string(value.OrganizationID)) != nil ||
		value.Scope.Validate() != nil || !value.Resource.Valid() || value.SoftLimit == 0 ||
		value.HardLimit < value.SoftLimit || value.HardLimit > MaximumDatabaseValue || value.Window <= 0 || value.Window > 366*24*time.Hour ||
		value.MaximumReservationAge <= 0 || value.MaximumReservationAge > 24*time.Hour ||
		!validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) || !value.ExpiresAt.After(value.EffectiveAt) {
		return fmt.Errorf("company recovery: limit policy is invalid")
	}
	if value.Version == 1 && value.Supersedes != nil || value.Version > 1 && value.Supersedes == nil {
		return fmt.Errorf("company recovery: limit policy version lineage is invalid")
	}
	if value.Supersedes != nil && value.Supersedes.Validate() != nil {
		return fmt.Errorf("company recovery: superseded limit policy hash is invalid")
	}
	return nil
}

type LimitPolicy struct {
	Body        LimitPolicyBody       `json:"body"`
	ContentHash contracts.ContentHash `json:"content_hash"`
	Signature   contracts.Signature   `json:"signature"`
}

func (value LimitPolicy) Validate() error {
	if value.Body.Validate() != nil || value.ContentHash.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: signed limit policy is invalid")
	}
	return nil
}

type UsageRequest struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             ReservationID            `json:"reservation_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	Scopes         []ScopeRef               `json:"scopes"`
	Resource       ResourceKind             `json:"resource"`
	Units          uint64                   `json:"units"`
	Operation      string                   `json:"operation"`
	IdempotencyKey string                   `json:"idempotency_key"`
	Irreversible   bool                     `json:"irreversible"`
	RequestedAt    time.Time                `json:"requested_at"`
	ExpiresAt      time.Time                `json:"expires_at"`
}

func (value UsageRequest) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("reservation_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil ||
		!value.Resource.Valid() || value.Units == 0 || value.Units > MaximumDatabaseValue || validateToken("operation", value.Operation) != nil ||
		validateToken("idempotency_key", value.IdempotencyKey) != nil ||
		!validUTC(value.RequestedAt) || !validUTC(value.ExpiresAt) || !value.ExpiresAt.After(value.RequestedAt) ||
		value.ExpiresAt.Sub(value.RequestedAt) > 24*time.Hour || len(value.Scopes) == 0 || len(value.Scopes) > 32 {
		return fmt.Errorf("company recovery: usage request is invalid")
	}
	for index := range value.Scopes {
		if value.Scopes[index].Validate() != nil ||
			index > 0 && scopeKey(value.Scopes[index-1]) >= scopeKey(value.Scopes[index]) {
			return fmt.Errorf("company recovery: usage scopes must be sorted and unique")
		}
	}
	if !slices.Contains(value.Scopes, ScopeRef{Kind: ScopeCompany, ID: string(value.OrganizationID)}) {
		return fmt.Errorf("company recovery: usage request lacks company scope")
	}
	return nil
}

type ReservationState string

const (
	ReservationReserved  ReservationState = "reserved"
	ReservationCommitted ReservationState = "committed"
	ReservationReleased  ReservationState = "released"
	ReservationExpired   ReservationState = "expired"
	ReservationDenied    ReservationState = "denied"
)

func (value ReservationState) Valid() bool {
	return value == ReservationReserved || value == ReservationCommitted ||
		value == ReservationReleased || value == ReservationExpired || value == ReservationDenied
}

type UsageReceipt struct {
	SchemaVersion string                  `json:"schema_version"`
	Request       UsageRequest            `json:"request"`
	State         ReservationState        `json:"state"`
	ActualUnits   uint64                  `json:"actual_units"`
	PolicyHashes  []contracts.ContentHash `json:"policy_hashes"`
	ReservedAt    time.Time               `json:"reserved_at"`
	FinalizedAt   *time.Time              `json:"finalized_at"`
	ReasonCode    string                  `json:"reason_code"`
}

func (value UsageReceipt) Validate() error {
	if value.SchemaVersion != SchemaVersion || value.Request.Validate() != nil || !value.State.Valid() ||
		!validUTC(value.ReservedAt) || value.ReservedAt.Before(value.Request.RequestedAt) ||
		validateToken("reason_code", value.ReasonCode) != nil || len(value.PolicyHashes) == 0 || len(value.PolicyHashes) > 32 {
		return fmt.Errorf("company recovery: usage receipt is invalid")
	}
	for index := range value.PolicyHashes {
		if value.PolicyHashes[index].Validate() != nil ||
			index > 0 && value.PolicyHashes[index].Digest <= value.PolicyHashes[index-1].Digest {
			return fmt.Errorf("company recovery: usage policy hashes are invalid")
		}
	}
	if value.State == ReservationReserved && value.FinalizedAt != nil ||
		value.State != ReservationReserved && (value.FinalizedAt == nil || !validUTC(*value.FinalizedAt)) {
		return fmt.Errorf("company recovery: usage receipt finalization is invalid")
	}
	if value.ActualUnits > MaximumDatabaseValue || value.ActualUnits > value.Request.Units ||
		value.State == ReservationCommitted && value.ActualUnits == 0 ||
		value.State != ReservationCommitted && value.ActualUnits != 0 {
		return fmt.Errorf("company recovery: usage receipt actual units are invalid")
	}
	return nil
}

type MetricKind string

const (
	MetricCompanyCadence        MetricKind = "company_cadence"
	MetricOpportunityTransition MetricKind = "opportunity_transition"
	MetricLifecycleTransition   MetricKind = "lifecycle_transition"
	MetricPortfolioDecision     MetricKind = "portfolio_decision"
	MetricInitiativeState       MetricKind = "initiative_state"
	MetricSquadState            MetricKind = "squad_state"
	MetricWake                  MetricKind = "wake"
	MetricMail                  MetricKind = "mail"
	MetricLease                 MetricKind = "lease"
	MetricGraphEligibility      MetricKind = "graph_eligibility"
	MetricPolicy                MetricKind = "policy"
	MetricApproval              MetricKind = "approval"
	MetricBrowserEffect         MetricKind = "browser_effect"
	MetricExternalEffect        MetricKind = "external_effect"
	MetricFinancialExposure     MetricKind = "financial_exposure"
	MetricReconciliation        MetricKind = "reconciliation"
	MetricCircuit               MetricKind = "circuit"
	MetricVerdict               MetricKind = "verdict"
	MetricCost                  MetricKind = "cost"
	MetricRevenue               MetricKind = "revenue"
	MetricCustomerOutcome       MetricKind = "customer_outcome"
	MetricProductOutcome        MetricKind = "product_outcome"
	MetricResourceUse           MetricKind = "resource_use"
	MetricTerminalState         MetricKind = "terminal_state"
)

func (value MetricKind) Valid() bool {
	switch value {
	case MetricCompanyCadence, MetricOpportunityTransition, MetricLifecycleTransition,
		MetricPortfolioDecision, MetricInitiativeState, MetricSquadState, MetricWake,
		MetricMail, MetricLease, MetricGraphEligibility, MetricPolicy, MetricApproval,
		MetricBrowserEffect, MetricExternalEffect, MetricFinancialExposure,
		MetricReconciliation, MetricCircuit, MetricVerdict, MetricCost, MetricRevenue,
		MetricCustomerOutcome, MetricProductOutcome, MetricResourceUse, MetricTerminalState:
		return true
	default:
		return false
	}
}

type Dimension struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (value Dimension) Validate() error {
	if validateToken("dimension_name", value.Name) != nil || validateToken("dimension_value", value.Value) != nil {
		return fmt.Errorf("company recovery: metric dimension is invalid")
	}
	return nil
}

type MetricSample struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             MetricID                 `json:"metric_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	Kind           MetricKind               `json:"kind"`
	Resource       ResourceKind             `json:"resource"`
	Value          int64                    `json:"value"`
	Unit           string                   `json:"unit"`
	Dimensions     []Dimension              `json:"dimensions"`
	ObservedAt     time.Time                `json:"observed_at"`
}

func (value MetricSample) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("metric_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil || !value.Kind.Valid() ||
		!value.Resource.Valid() || validateToken("metric_unit", value.Unit) != nil ||
		!validUTC(value.ObservedAt) || len(value.Dimensions) > 32 {
		return fmt.Errorf("company recovery: metric sample is invalid")
	}
	for index := range value.Dimensions {
		if value.Dimensions[index].Validate() != nil ||
			index > 0 && dimensionKey(value.Dimensions[index-1]) >= dimensionKey(value.Dimensions[index]) {
			return fmt.Errorf("company recovery: metric dimensions must be sorted and unique")
		}
	}
	return nil
}

type TraceStatus string

const (
	TraceStarted   TraceStatus = "started"
	TraceSucceeded TraceStatus = "succeeded"
	TraceFailed    TraceStatus = "failed"
	TraceCancelled TraceStatus = "cancelled"
	TraceAmbiguous TraceStatus = "ambiguous"
)

func (value TraceStatus) Valid() bool {
	return value == TraceStarted || value == TraceSucceeded || value == TraceFailed ||
		value == TraceCancelled || value == TraceAmbiguous
}

type TraceSpan struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             TraceID                  `json:"trace_id"`
	ParentID       *TraceID                 `json:"parent_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	Operation      string                   `json:"operation"`
	ResourceKind   string                   `json:"resource_kind"`
	ResourceID     string                   `json:"resource_id"`
	Status         TraceStatus              `json:"status"`
	SafeCode       string                   `json:"safe_code"`
	StartedAt      time.Time                `json:"started_at"`
	FinishedAt     *time.Time               `json:"finished_at"`
}

func (value TraceSpan) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("trace_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil ||
		validateToken("trace operation", value.Operation) != nil ||
		validateToken("trace resource_kind", value.ResourceKind) != nil ||
		validateToken("trace resource_id", value.ResourceID) != nil || !value.Status.Valid() ||
		validateToken("trace safe_code", value.SafeCode) != nil || !validUTC(value.StartedAt) {
		return fmt.Errorf("company recovery: trace span is invalid")
	}
	if value.ParentID != nil && (validateToken("parent_trace_id", string(*value.ParentID)) != nil || *value.ParentID == value.ID) {
		return fmt.Errorf("company recovery: trace parent is invalid")
	}
	if value.Status == TraceStarted && value.FinishedAt != nil ||
		value.Status != TraceStarted && (value.FinishedAt == nil || !validUTC(*value.FinishedAt) || value.FinishedAt.Before(value.StartedAt)) {
		return fmt.Errorf("company recovery: trace chronology is invalid")
	}
	return nil
}

type IncidentKind string

const (
	IncidentResourceExhausted  IncidentKind = "resource_exhausted"
	IncidentOverload           IncidentKind = "overload"
	IncidentQueueSaturation    IncidentKind = "queue_saturation"
	IncidentProviderOutage     IncidentKind = "provider_outage"
	IncidentBrowserLoss        IncidentKind = "browser_loss"
	IncidentFinancialAmbiguity IncidentKind = "financial_ambiguity"
	IncidentCancellation       IncidentKind = "cancellation"
	IncidentShutdown           IncidentKind = "shutdown"
	IncidentRestoreConflict    IncidentKind = "restore_conflict"
	IncidentOfflineConflict    IncidentKind = "offline_conflict"
	IncidentRPOBreach          IncidentKind = "rpo_breach"
	IncidentRTOBreach          IncidentKind = "rto_breach"
)

func (value IncidentKind) Valid() bool {
	switch value {
	case IncidentResourceExhausted, IncidentOverload, IncidentQueueSaturation,
		IncidentProviderOutage, IncidentBrowserLoss, IncidentFinancialAmbiguity,
		IncidentCancellation, IncidentShutdown, IncidentRestoreConflict,
		IncidentOfflineConflict, IncidentRPOBreach, IncidentRTOBreach:
		return true
	default:
		return false
	}
}

type Incident struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             IncidentID               `json:"incident_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	Kind           IncidentKind             `json:"kind"`
	Scope          ScopeRef                 `json:"scope"`
	Resource       ResourceKind             `json:"resource"`
	SafeCode       string                   `json:"safe_code"`
	RecordKind     string                   `json:"record_kind"`
	RecordID       string                   `json:"record_id"`
	Observed       uint64                   `json:"observed"`
	Limit          uint64                   `json:"limit"`
	CreatedAt      time.Time                `json:"created_at"`
}

func (value Incident) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("incident_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil || !value.Kind.Valid() ||
		value.Scope.Validate() != nil || !value.Resource.Valid() || validateToken("safe_code", value.SafeCode) != nil ||
		validateToken("record_kind", value.RecordKind) != nil || validateToken("record_id", value.RecordID) != nil ||
		value.Observed > MaximumDatabaseValue || value.Limit > MaximumDatabaseValue || !validUTC(value.CreatedAt) {
		return fmt.Errorf("company recovery: incident is invalid")
	}
	return nil
}

type RetentionAction string

const (
	RetentionKeep        RetentionAction = "keep"
	RetentionDelete      RetentionAction = "delete"
	RetentionCryptoErase RetentionAction = "cryptographic_erase"
)

func (value RetentionAction) Valid() bool {
	return value == RetentionKeep || value == RetentionDelete || value == RetentionCryptoErase
}

type DataClass string

const (
	DataAuthority       DataClass = "authority"
	DataLedger          DataClass = "ledger"
	DataGraph           DataClass = "graph"
	DataMail            DataClass = "mail"
	DataReceipt         DataClass = "receipt"
	DataProjectBrain    DataClass = "project_brain"
	DataCompanyState    DataClass = "company_state"
	DataCustomer        DataClass = "customer"
	DataFinancial       DataClass = "financial"
	DataBrowsing        DataClass = "browsing"
	DataModelEvidence   DataClass = "model_evidence"
	DataBusinessOutcome DataClass = "business_outcome"
	DataBackup          DataClass = "backup"
)

func (value DataClass) Valid() bool {
	switch value {
	case DataAuthority, DataLedger, DataGraph, DataMail, DataReceipt, DataProjectBrain,
		DataCompanyState, DataCustomer, DataFinancial, DataBrowsing, DataModelEvidence,
		DataBusinessOutcome, DataBackup:
		return true
	default:
		return false
	}
}

type RetentionRule struct {
	Class     DataClass       `json:"class"`
	Retention time.Duration   `json:"retention"`
	Action    RetentionAction `json:"action"`
}

func (value RetentionRule) Validate() error {
	if !value.Class.Valid() || !value.Action.Valid() || value.Retention < 0 || value.Retention > 25*366*24*time.Hour ||
		(value.Action == RetentionKeep && value.Retention != 0) {
		return fmt.Errorf("company recovery: retention rule is invalid")
	}
	return nil
}

type RecoveryPolicyBody struct {
	SchemaVersion       string                   `json:"schema_version"`
	ID                  RecoveryPolicyID         `json:"policy_id"`
	Version             uint64                   `json:"version"`
	OrganizationID      contracts.OrganizationID `json:"organization_id"`
	BackupInterval      time.Duration            `json:"backup_interval"`
	RPO                 time.Duration            `json:"rpo"`
	RTO                 time.Duration            `json:"rto"`
	PITRRequired        bool                     `json:"pitr_required"`
	MaximumArchiveBytes uint64                   `json:"maximum_archive_bytes"`
	Rules               []RetentionRule          `json:"rules"`
	EffectiveAt         time.Time                `json:"effective_at"`
	ExpiresAt           time.Time                `json:"expires_at"`
	Supersedes          *contracts.ContentHash   `json:"supersedes"`
}

func (value RecoveryPolicyBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("recovery_policy_id", string(value.ID)) != nil ||
		value.Version == 0 || value.Version > MaximumDatabaseValue || validateToken("organization_id", string(value.OrganizationID)) != nil ||
		value.BackupInterval <= 0 || value.BackupInterval > 30*24*time.Hour || value.RPO <= 0 || value.RPO > 30*24*time.Hour ||
		value.BackupInterval > value.RPO || value.RTO <= 0 || value.RTO > 30*24*time.Hour ||
		value.MaximumArchiveBytes == 0 || value.MaximumArchiveBytes > MaximumLogicalArchiveBytes ||
		len(value.Rules) == 0 || len(value.Rules) > 64 || !validUTC(value.EffectiveAt) ||
		!validUTC(value.ExpiresAt) || !value.ExpiresAt.After(value.EffectiveAt) {
		return fmt.Errorf("company recovery: recovery policy is invalid")
	}
	requiredClasses := []DataClass{
		DataAuthority, DataBackup, DataBrowsing, DataBusinessOutcome, DataCompanyState,
		DataCustomer, DataFinancial, DataGraph, DataLedger, DataMail, DataModelEvidence,
		DataProjectBrain, DataReceipt,
	}
	if len(value.Rules) != len(requiredClasses) {
		return fmt.Errorf("company recovery: recovery policy must cover every durable data class")
	}
	for index, rule := range value.Rules {
		if rule.Validate() != nil || rule.Class != requiredClasses[index] {
			return fmt.Errorf("company recovery: retention rules must be sorted and unique")
		}
	}
	if value.Version == 1 && value.Supersedes != nil || value.Version > 1 && value.Supersedes == nil {
		return fmt.Errorf("company recovery: recovery policy lineage is invalid")
	}
	if value.Supersedes != nil && value.Supersedes.Validate() != nil {
		return fmt.Errorf("company recovery: recovery policy supersession is invalid")
	}
	return nil
}

type RecoveryPolicy struct {
	Body        RecoveryPolicyBody    `json:"body"`
	ContentHash contracts.ContentHash `json:"content_hash"`
	Signature   contracts.Signature   `json:"signature"`
}

func (value RecoveryPolicy) Validate() error {
	if value.Body.Validate() != nil || value.ContentHash.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: signed recovery policy is invalid")
	}
	return nil
}

type BackupScopeKind string

const (
	BackupOrganization BackupScopeKind = "organization"
	BackupInitiative   BackupScopeKind = "initiative"
	BackupCustomer     BackupScopeKind = "customer"
	BackupProject      BackupScopeKind = "project"
)

func (value BackupScopeKind) Valid() bool {
	return value == BackupOrganization || value == BackupInitiative || value == BackupCustomer || value == BackupProject
}

type BackupScope struct {
	Kind BackupScopeKind `json:"kind"`
	ID   string          `json:"id"`
}

func (value BackupScope) Validate() error {
	if !value.Kind.Valid() || validateToken("backup_scope_id", value.ID) != nil {
		return fmt.Errorf("company recovery: backup scope is invalid")
	}
	return nil
}

type BackupAuthorizationBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             BackupID                 `json:"backup_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	Scope          BackupScope              `json:"scope"`
	Purpose        string                   `json:"purpose"`
	RequestedAt    time.Time                `json:"requested_at"`
	ExpiresAt      time.Time                `json:"expires_at"`
}

func (value BackupAuthorizationBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("backup_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil || value.Scope.Validate() != nil ||
		validateToken("backup purpose", value.Purpose) != nil || !validUTC(value.RequestedAt) ||
		!validUTC(value.ExpiresAt) || !value.ExpiresAt.After(value.RequestedAt) ||
		value.ExpiresAt.Sub(value.RequestedAt) > time.Hour {
		return fmt.Errorf("company recovery: backup authorization is invalid")
	}
	return nil
}

type BackupAuthorization struct {
	Body      BackupAuthorizationBody `json:"body"`
	Signature contracts.Signature     `json:"signature"`
}

func (value BackupAuthorization) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: signed backup authorization is invalid")
	}
	return nil
}

type TableArchive struct {
	Name       string                `json:"name"`
	SchemaHash contracts.ContentHash `json:"schema_hash"`
	RowsHash   contracts.ContentHash `json:"rows_hash"`
	Rows       [][]byte              `json:"rows"`
}

func (value TableArchive) Validate() error {
	if validateToken("archive table", value.Name) != nil || value.SchemaHash.Validate() != nil ||
		value.RowsHash.Validate() != nil || len(value.Rows) > MaximumRowsPerTable {
		return fmt.Errorf("company recovery: table archive is invalid")
	}
	for index, row := range value.Rows {
		if len(row) == 0 || len(row) > contracts.MaxCanonicalBytes ||
			index > 0 && string(value.Rows[index-1]) >= string(row) {
			return fmt.Errorf("company recovery: archived rows must be bounded, sorted, and unique")
		}
	}
	if hashRows(value.Rows) != value.RowsHash {
		return fmt.Errorf("company recovery: archived row hash is invalid")
	}
	return nil
}

type RecoveryArchive struct {
	SchemaVersion  string                   `json:"schema_version"`
	BackupID       BackupID                 `json:"backup_id"`
	TenantID       string                   `json:"tenant_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	Scope          BackupScope              `json:"scope"`
	SnapshotAt     time.Time                `json:"snapshot_at"`
	WALLSN         string                   `json:"wal_lsn"`
	TXSnapshot     string                   `json:"tx_snapshot"`
	Tables         []TableArchive           `json:"tables"`
}

func (value RecoveryArchive) Validate() error {
	if value.SchemaVersion != ArchiveSchemaVersion || validateToken("backup_id", string(value.BackupID)) != nil ||
		validateToken("tenant_id", value.TenantID) != nil || validateToken("organization_id", string(value.OrganizationID)) != nil ||
		value.Scope.Validate() != nil || !validUTC(value.SnapshotAt) || validateBounded("wal_lsn", value.WALLSN, 128) != nil ||
		validateBounded("tx_snapshot", value.TXSnapshot, 512) != nil || len(value.Tables) == 0 || len(value.Tables) > MaximumArchiveTables {
		return fmt.Errorf("company recovery: recovery archive is invalid")
	}
	for index := range value.Tables {
		if value.Tables[index].Validate() != nil || index > 0 && value.Tables[index-1].Name >= value.Tables[index].Name {
			return fmt.Errorf("company recovery: archive tables must be sorted and unique")
		}
	}
	return nil
}

type TableSummary struct {
	Name       string                `json:"name"`
	SchemaHash contracts.ContentHash `json:"schema_hash"`
	RowsHash   contracts.ContentHash `json:"rows_hash"`
	RowCount   uint64                `json:"row_count"`
}

func (value TableSummary) Validate() error {
	if validateToken("table summary name", value.Name) != nil || value.SchemaHash.Validate() != nil || value.RowsHash.Validate() != nil {
		return fmt.Errorf("company recovery: table summary is invalid")
	}
	return nil
}

type BackupManifestBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	BackupID       BackupID                 `json:"backup_id"`
	TenantID       string                   `json:"tenant_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	Scope          BackupScope              `json:"scope"`
	ArchiveHash    contracts.ContentHash    `json:"archive_hash"`
	WALLSN         string                   `json:"wal_lsn"`
	TXSnapshot     string                   `json:"tx_snapshot"`
	Tables         []TableSummary           `json:"tables"`
	PITRPoint      string                   `json:"pitr_point"`
	SnapshotAt     time.Time                `json:"snapshot_at"`
	CompletedAt    time.Time                `json:"completed_at"`
	RPO            time.Duration            `json:"rpo"`
	RPOStatus      string                   `json:"rpo_status"`
	RPOObserved    time.Duration            `json:"rpo_observed"`
	RTO            time.Duration            `json:"rto"`
}

func (value BackupManifestBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("backup_id", string(value.BackupID)) != nil ||
		validateToken("tenant_id", value.TenantID) != nil || validateToken("organization_id", string(value.OrganizationID)) != nil ||
		value.Scope.Validate() != nil || value.ArchiveHash.Validate() != nil ||
		validateBounded("wal_lsn", value.WALLSN, 128) != nil || validateBounded("tx_snapshot", value.TXSnapshot, 512) != nil ||
		len(value.Tables) == 0 || len(value.Tables) > MaximumArchiveTables || !validUTC(value.SnapshotAt) ||
		!validUTC(value.CompletedAt) || value.CompletedAt.Before(value.SnapshotAt) || value.RPO <= 0 ||
		(value.RPOStatus != "baseline" && value.RPOStatus != "met" && value.RPOStatus != "breached") || value.RTO <= 0 {
		return fmt.Errorf("company recovery: backup manifest is invalid")
	}
	for index := range value.Tables {
		if value.Tables[index].Validate() != nil || index > 0 && value.Tables[index-1].Name >= value.Tables[index].Name {
			return fmt.Errorf("company recovery: backup table summaries are invalid")
		}
	}
	if value.PITRPoint != "" && validateBounded("pitr_point", value.PITRPoint, 1024) != nil {
		return fmt.Errorf("company recovery: PITR point is invalid")
	}
	if value.RPOObserved < 0 || value.RPOStatus == "baseline" && value.RPOObserved != 0 ||
		value.RPOStatus != "baseline" && value.RPOObserved <= 0 ||
		value.RPOStatus == "met" && value.RPOObserved > value.RPO ||
		value.RPOStatus == "breached" && value.RPOObserved <= value.RPO {
		return fmt.Errorf("company recovery: RPO observation is invalid")
	}
	return nil
}

type BackupManifest struct {
	Body      BackupManifestBody  `json:"body"`
	Signature contracts.Signature `json:"signature"`
}

func (value BackupManifest) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: signed backup manifest is invalid")
	}
	return nil
}

type RecoveryBundle struct {
	Manifest         BackupManifest `json:"manifest"`
	EncryptedArchive []byte         `json:"encrypted_archive"`
	SealedArchiveKey []byte         `json:"sealed_archive_key"`
}

func (value RecoveryBundle) Validate() error {
	if value.Manifest.Validate() != nil || len(value.EncryptedArchive) == 0 ||
		uint64(len(value.EncryptedArchive)) > MaximumLogicalArchiveBytes+64<<20 ||
		len(value.SealedArchiveKey) == 0 || len(value.SealedArchiveKey) > 1<<20 {
		return fmt.Errorf("company recovery: recovery bundle is invalid")
	}
	return nil
}

type RestoreMode string

const (
	RestoreClean RestoreMode = "clean"
	RestorePITR  RestoreMode = "point_in_time"
)

func (value RestoreMode) Valid() bool { return value == RestoreClean || value == RestorePITR }

type RestoreAuthorizationBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             RestoreID                `json:"restore_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	BackupID       BackupID                 `json:"backup_id"`
	ArchiveHash    contracts.ContentHash    `json:"archive_hash"`
	Mode           RestoreMode              `json:"mode"`
	TargetAt       time.Time                `json:"target_at"`
	RequestedAt    time.Time                `json:"requested_at"`
	ExpiresAt      time.Time                `json:"expires_at"`
}

func (value RestoreAuthorizationBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("restore_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil || validateToken("backup_id", string(value.BackupID)) != nil ||
		value.ArchiveHash.Validate() != nil || !value.Mode.Valid() || !validUTC(value.TargetAt) ||
		!validUTC(value.RequestedAt) || !validUTC(value.ExpiresAt) || value.TargetAt.After(value.RequestedAt) ||
		!value.ExpiresAt.After(value.RequestedAt) || value.ExpiresAt.Sub(value.RequestedAt) > time.Hour {
		return fmt.Errorf("company recovery: restore authorization is invalid")
	}
	return nil
}

type RestoreAuthorization struct {
	Body      RestoreAuthorizationBody `json:"body"`
	Signature contracts.Signature      `json:"signature"`
}

func (value RestoreAuthorization) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: signed restore authorization is invalid")
	}
	return nil
}

type RestoreState string

const (
	RestoreReconciliationRequired RestoreState = "reconciliation_required"
	RestoreReady                  RestoreState = "ready"
	RestoreFailed                 RestoreState = "failed"
)

func (value RestoreState) Valid() bool {
	return value == RestoreReconciliationRequired || value == RestoreReady || value == RestoreFailed
}

type RestoreReceiptBody struct {
	SchemaVersion              string                   `json:"schema_version"`
	ID                         RestoreID                `json:"restore_id"`
	OrganizationID             contracts.OrganizationID `json:"organization_id"`
	BackupID                   BackupID                 `json:"backup_id"`
	ArchiveHash                contracts.ContentHash    `json:"archive_hash"`
	State                      RestoreState             `json:"state"`
	RestoredTables             uint32                   `json:"restored_tables"`
	RestoredRows               uint64                   `json:"restored_rows"`
	CancelledRuntimeLeases     uint64                   `json:"cancelled_runtime_leases"`
	InvalidatedAuthorityLeases uint64                   `json:"invalidated_authority_leases"`
	CoalescedWakes             uint64                   `json:"coalesced_wakes"`
	QuarantinedEffects         uint64                   `json:"quarantined_effects"`
	QuarantinedExternalState   uint64                   `json:"quarantined_external_state"`
	ReconciliationEvidenceHash *contracts.ContentHash   `json:"reconciliation_evidence_hash"`
	StartedAt                  time.Time                `json:"started_at"`
	CompletedAt                time.Time                `json:"completed_at"`
	RTO                        time.Duration            `json:"rto"`
	RTOStatus                  string                   `json:"rto_status"`
}

func (value RestoreReceiptBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("restore_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil || validateToken("backup_id", string(value.BackupID)) != nil ||
		value.ArchiveHash.Validate() != nil || !value.State.Valid() || value.RestoredTables == 0 ||
		!validUTC(value.StartedAt) || !validUTC(value.CompletedAt) || value.CompletedAt.Before(value.StartedAt) ||
		value.RTO <= 0 || (value.RTOStatus != "met" && value.RTOStatus != "breached") {
		return fmt.Errorf("company recovery: restore receipt is invalid")
	}
	if value.State == RestoreReady && (value.ReconciliationEvidenceHash == nil || value.ReconciliationEvidenceHash.Validate() != nil) ||
		value.State != RestoreReady && value.ReconciliationEvidenceHash != nil {
		return fmt.Errorf("company recovery: restore reconciliation evidence is invalid")
	}
	return nil
}

type RestoreReceipt struct {
	Body      RestoreReceiptBody  `json:"body"`
	Signature contracts.Signature `json:"signature"`
}

func (value RestoreReceipt) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: signed restore receipt is invalid")
	}
	return nil
}

type ErasureTargetKind string

const (
	ErasureBackup ErasureTargetKind = "backup"
	ErasureScope  ErasureTargetKind = "scope"
)

func (value ErasureTargetKind) Valid() bool { return value == ErasureBackup || value == ErasureScope }

type ErasureDirectiveBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             ErasureID                `json:"erasure_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	TargetKind     ErasureTargetKind        `json:"target_kind"`
	TargetID       string                   `json:"target_id"`
	Class          DataClass                `json:"class"`
	Action         RetentionAction          `json:"action"`
	Reason         string                   `json:"reason"`
	AuthorizedAt   time.Time                `json:"authorized_at"`
	ExecuteAfter   time.Time                `json:"execute_after"`
}

func (value ErasureDirectiveBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("erasure_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil || !value.TargetKind.Valid() ||
		validateToken("erasure target_id", value.TargetID) != nil || !value.Class.Valid() ||
		(value.Action != RetentionDelete && value.Action != RetentionCryptoErase) ||
		validateText("erasure reason", value.Reason, 4096) != nil || !validUTC(value.AuthorizedAt) ||
		!validUTC(value.ExecuteAfter) || value.ExecuteAfter.Before(value.AuthorizedAt) {
		return fmt.Errorf("company recovery: erasure directive is invalid")
	}
	return nil
}

type ErasureDirective struct {
	Body      ErasureDirectiveBody `json:"body"`
	Signature contracts.Signature  `json:"signature"`
}

func (value ErasureDirective) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: signed erasure directive is invalid")
	}
	return nil
}

type ErasureReceiptBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             ErasureID                `json:"erasure_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	TargetKind     ErasureTargetKind        `json:"target_kind"`
	TargetID       string                   `json:"target_id"`
	Class          DataClass                `json:"class"`
	Action         RetentionAction          `json:"action"`
	DestroyedKeys  uint32                   `json:"destroyed_keys"`
	DeletedObjects uint64                   `json:"deleted_objects"`
	ExecutedAt     time.Time                `json:"executed_at"`
}

func (value ErasureReceiptBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("erasure_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil || !value.TargetKind.Valid() ||
		validateToken("erasure target_id", value.TargetID) != nil || !value.Class.Valid() ||
		(value.Action != RetentionDelete && value.Action != RetentionCryptoErase) ||
		!validUTC(value.ExecutedAt) || value.Action == RetentionCryptoErase && value.DestroyedKeys == 0 {
		return fmt.Errorf("company recovery: erasure receipt is invalid")
	}
	return nil
}

type ErasureReceipt struct {
	Body      ErasureReceiptBody  `json:"body"`
	Signature contracts.Signature `json:"signature"`
}

func (value ErasureReceipt) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: signed erasure receipt is invalid")
	}
	return nil
}

type OfflineClass string

const (
	OfflineEvidence     OfflineClass = "evidence"
	OfflineObservation  OfflineClass = "observation"
	OfflineReceiptClass OfflineClass = "receipt"
	OfflineCheckpoint   OfflineClass = "checkpoint"
)

func (value OfflineClass) Valid() bool {
	return value == OfflineEvidence || value == OfflineObservation || value == OfflineReceiptClass || value == OfflineCheckpoint
}

type OfflineItem struct {
	Class      OfflineClass          `json:"class"`
	RecordKind string                `json:"record_kind"`
	RecordID   string                `json:"record_id"`
	Version    uint64                `json:"version"`
	Hash       contracts.ContentHash `json:"hash"`
	Payload    []byte                `json:"payload"`
	ObservedAt time.Time             `json:"observed_at"`
}

func (value OfflineItem) Validate() error {
	if !value.Class.Valid() || validateToken("offline record_kind", value.RecordKind) != nil ||
		validateToken("offline record_id", value.RecordID) != nil || value.Version == 0 || value.Version > MaximumDatabaseValue ||
		value.Hash.Validate() != nil || len(value.Payload) == 0 || len(value.Payload) > contracts.MaxCanonicalBytes ||
		!validUTC(value.ObservedAt) || hashBytes(value.Payload) != value.Hash {
		return fmt.Errorf("company recovery: offline item is invalid")
	}
	return nil
}

type OfflineBatchBody struct {
	SchemaVersion   string                   `json:"schema_version"`
	ID              OfflineBatchID           `json:"batch_id"`
	TenantID        string                   `json:"tenant_id"`
	OrganizationID  contracts.OrganizationID `json:"organization_id"`
	MachineID       string                   `json:"machine_id"`
	Sequence        uint64                   `json:"sequence"`
	BaseBackupID    BackupID                 `json:"base_backup_id"`
	BaseArchiveHash contracts.ContentHash    `json:"base_archive_hash"`
	Items           []OfflineItem            `json:"items"`
	CreatedAt       time.Time                `json:"created_at"`
}

func (value OfflineBatchBody) Validate() error {
	if value.SchemaVersion != OfflineSchemaVersion || validateToken("offline batch_id", string(value.ID)) != nil ||
		validateToken("tenant_id", value.TenantID) != nil || validateToken("organization_id", string(value.OrganizationID)) != nil ||
		validateToken("machine_id", value.MachineID) != nil || value.Sequence == 0 || value.Sequence > MaximumDatabaseValue ||
		validateToken("base_backup_id", string(value.BaseBackupID)) != nil || value.BaseArchiveHash.Validate() != nil ||
		len(value.Items) == 0 || len(value.Items) > MaximumOfflineBatch || !validUTC(value.CreatedAt) {
		return fmt.Errorf("company recovery: offline batch is invalid")
	}
	for index := range value.Items {
		if value.Items[index].Validate() != nil || index > 0 && offlineItemKey(value.Items[index-1]) >= offlineItemKey(value.Items[index]) {
			return fmt.Errorf("company recovery: offline items must be sorted and unique")
		}
	}
	return nil
}

type OfflineBatch struct {
	Body      OfflineBatchBody    `json:"body"`
	Signature contracts.Signature `json:"signature"`
}

func (value OfflineBatch) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: signed offline batch is invalid")
	}
	return nil
}

type OfflineDisposition string

const (
	OfflineDuplicate           OfflineDisposition = "duplicate"
	OfflineAccepted            OfflineDisposition = "accepted"
	OfflineStale               OfflineDisposition = "stale"
	OfflineConflict            OfflineDisposition = "conflict"
	OfflineNeedsReconciliation OfflineDisposition = "reconciliation_required"
)

func (value OfflineDisposition) Valid() bool {
	return value == OfflineDuplicate || value == OfflineAccepted || value == OfflineStale ||
		value == OfflineConflict || value == OfflineNeedsReconciliation
}

type OfflineItemResult struct {
	RecordKind  string                 `json:"record_kind"`
	RecordID    string                 `json:"record_id"`
	Version     uint64                 `json:"version"`
	Disposition OfflineDisposition     `json:"disposition"`
	CurrentHash *contracts.ContentHash `json:"current_hash"`
}

func (value OfflineItemResult) Validate() error {
	if validateToken("offline result record_kind", value.RecordKind) != nil ||
		validateToken("offline result record_id", value.RecordID) != nil || value.Version == 0 ||
		value.Version > MaximumDatabaseValue || !value.Disposition.Valid() {
		return fmt.Errorf("company recovery: offline item result is invalid")
	}
	if value.CurrentHash != nil && value.CurrentHash.Validate() != nil {
		return fmt.Errorf("company recovery: offline current hash is invalid")
	}
	return nil
}

type OfflineReceiptBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	BatchID        OfflineBatchID           `json:"batch_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	MachineID      string                   `json:"machine_id"`
	Sequence       uint64                   `json:"sequence"`
	Results        []OfflineItemResult      `json:"results"`
	ReconciledAt   time.Time                `json:"reconciled_at"`
}

func (value OfflineReceiptBody) Validate() error {
	if value.SchemaVersion != OfflineSchemaVersion || validateToken("offline batch_id", string(value.BatchID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil || validateToken("machine_id", value.MachineID) != nil ||
		value.Sequence == 0 || len(value.Results) == 0 || len(value.Results) > MaximumOfflineBatch || !validUTC(value.ReconciledAt) {
		return fmt.Errorf("company recovery: offline receipt is invalid")
	}
	for index := range value.Results {
		if value.Results[index].Validate() != nil || index > 0 && offlineResultKey(value.Results[index-1]) >= offlineResultKey(value.Results[index]) {
			return fmt.Errorf("company recovery: offline results must be sorted and unique")
		}
	}
	return nil
}

type OfflineReceipt struct {
	Body      OfflineReceiptBody  `json:"body"`
	Signature contracts.Signature `json:"signature"`
}

type OfflineResolutionDecision string

const (
	OfflineResolutionAcceptEvidence OfflineResolutionDecision = "accept_as_evidence"
	OfflineResolutionReject         OfflineResolutionDecision = "reject"
	OfflineResolutionSupersede      OfflineResolutionDecision = "supersede"
)

func (value OfflineResolutionDecision) Valid() bool {
	return value == OfflineResolutionAcceptEvidence || value == OfflineResolutionReject || value == OfflineResolutionSupersede
}

type OfflineReconciliationResolution struct {
	SchemaVersion    string                    `json:"schema_version"`
	ID               string                    `json:"resolution_id"`
	OrganizationID   contracts.OrganizationID  `json:"organization_id"`
	ReconciliationID string                    `json:"reconciliation_id"`
	BatchID          OfflineBatchID            `json:"batch_id"`
	MachineID        string                    `json:"machine_id"`
	RecordKind       string                    `json:"record_kind"`
	RecordID         string                    `json:"record_id"`
	Version          uint64                    `json:"version"`
	OfflineHash      contracts.ContentHash     `json:"offline_hash"`
	Decision         OfflineResolutionDecision `json:"decision"`
	EvidenceHash     contracts.ContentHash     `json:"evidence_hash"`
	ResolvedAt       time.Time                 `json:"resolved_at"`
	Signature        contracts.Signature       `json:"signature"`
}

func (value OfflineReconciliationResolution) Validate() error {
	if value.SchemaVersion != OfflineSchemaVersion || validateToken("offline resolution_id", value.ID) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil ||
		validateToken("offline reconciliation_id", value.ReconciliationID) != nil ||
		validateToken("offline batch_id", string(value.BatchID)) != nil || validateToken("machine_id", value.MachineID) != nil ||
		validateToken("offline record_kind", value.RecordKind) != nil || validateToken("offline record_id", value.RecordID) != nil ||
		value.Version == 0 || value.Version > MaximumDatabaseValue || value.OfflineHash.Validate() != nil ||
		!value.Decision.Valid() || value.EvidenceHash.Validate() != nil || !validUTC(value.ResolvedAt) || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: offline reconciliation resolution is invalid")
	}
	return nil
}

type MachineKeyRegistrationBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             MachineKeyID             `json:"machine_key_id"`
	Version        uint64                   `json:"version"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	MachineID      string                   `json:"machine_id"`
	KeyID          string                   `json:"key_id"`
	PublicKey      []byte                   `json:"public_key"`
	EffectiveAt    time.Time                `json:"effective_at"`
	ExpiresAt      time.Time                `json:"expires_at"`
	Supersedes     *contracts.ContentHash   `json:"supersedes"`
}

func (value MachineKeyRegistrationBody) Validate() error {
	if value.SchemaVersion != OfflineSchemaVersion || validateToken("machine_key_id", string(value.ID)) != nil ||
		value.Version == 0 || value.Version > MaximumDatabaseValue || validateToken("organization_id", string(value.OrganizationID)) != nil ||
		validateToken("machine_id", value.MachineID) != nil || validateToken("machine key_id", value.KeyID) != nil ||
		len(value.PublicKey) != ed25519.PublicKeySize || !validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) {
		return fmt.Errorf("company recovery: machine key registration is invalid")
	}
	if value.Version == 1 && value.Supersedes != nil || value.Version > 1 && value.Supersedes == nil {
		return fmt.Errorf("company recovery: machine key lineage is invalid")
	}
	if value.Supersedes != nil && value.Supersedes.Validate() != nil {
		return fmt.Errorf("company recovery: superseded machine key hash is invalid")
	}
	return nil
}

type MachineKeyRegistration struct {
	Body        MachineKeyRegistrationBody `json:"body"`
	ContentHash contracts.ContentHash      `json:"content_hash"`
	Signature   contracts.Signature        `json:"signature"`
}

func (value MachineKeyRegistration) Validate() error {
	if value.Body.Validate() != nil || value.ContentHash.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: signed machine key registration is invalid")
	}
	return nil
}

func (value OfflineReceipt) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: signed offline receipt is invalid")
	}
	return nil
}

type ShutdownState string

const (
	ShutdownDraining ShutdownState = "draining"
	ShutdownStopped  ShutdownState = "stopped"
)

func (value ShutdownState) Valid() bool { return value == ShutdownDraining || value == ShutdownStopped }

type ShutdownReceiptBody struct {
	SchemaVersion        string                   `json:"schema_version"`
	ID                   ShutdownID               `json:"shutdown_id"`
	OrganizationID       contracts.OrganizationID `json:"organization_id"`
	State                ShutdownState            `json:"state"`
	ReasonCode           string                   `json:"reason_code"`
	ReleasedReservations uint64                   `json:"released_reservations"`
	CancelledLeases      uint64                   `json:"cancelled_leases"`
	CoalescedWakes       uint64                   `json:"coalesced_wakes"`
	QuarantinedEffects   uint64                   `json:"quarantined_effects"`
	StartedAt            time.Time                `json:"started_at"`
	CompletedAt          *time.Time               `json:"completed_at"`
}

func (value ShutdownReceiptBody) Validate() error {
	if value.SchemaVersion != ShutdownSchemaVersion || validateToken("shutdown_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil || !value.State.Valid() ||
		validateToken("shutdown reason_code", value.ReasonCode) != nil || !validUTC(value.StartedAt) {
		return fmt.Errorf("company recovery: shutdown receipt is invalid")
	}
	if value.State == ShutdownDraining && value.CompletedAt != nil ||
		value.State == ShutdownStopped && (value.CompletedAt == nil || !validUTC(*value.CompletedAt) || value.CompletedAt.Before(value.StartedAt)) {
		return fmt.Errorf("company recovery: shutdown chronology is invalid")
	}
	return nil
}

type ShutdownReceipt struct {
	Body      ShutdownReceiptBody `json:"body"`
	Signature contracts.Signature `json:"signature"`
}

// RecoveryQualification is an explicit, runtime-signed release record. It is
// valid only while every bound policy and recovery artifact remains current.
type RecoveryQualification struct {
	SchemaVersion              string                   `json:"schema_version"`
	ID                         RecoveryQualificationID  `json:"qualification_id"`
	OrganizationID             contracts.OrganizationID `json:"organization_id"`
	RecoveryPolicyID           RecoveryPolicyID         `json:"recovery_policy_id"`
	RecoveryPolicyVersion      uint64                   `json:"recovery_policy_version"`
	RecoveryPolicyHash         contracts.ContentHash    `json:"recovery_policy_hash"`
	BackupID                   BackupID                 `json:"backup_id"`
	BackupManifestHash         contracts.ContentHash    `json:"backup_manifest_hash"`
	ArchiveHash                contracts.ContentHash    `json:"archive_hash"`
	RestoreID                  RestoreID                `json:"restore_id"`
	RestoreReceiptHash         contracts.ContentHash    `json:"restore_receipt_hash"`
	OfflineBatchID             OfflineBatchID           `json:"offline_batch_id"`
	OfflineReceiptHash         contracts.ContentHash    `json:"offline_receipt_hash"`
	RestoredTables             uint32                   `json:"restored_tables"`
	RestoredRows               uint64                   `json:"restored_rows"`
	CancelledRuntimeLeases     uint64                   `json:"cancelled_runtime_leases"`
	InvalidatedAuthorityLeases uint64                   `json:"invalidated_authority_leases"`
	CoalescedWakes             uint64                   `json:"coalesced_wakes"`
	QuarantinedEffects         uint64                   `json:"quarantined_effects"`
	QuarantinedExternalState   uint64                   `json:"quarantined_external_state"`
	OfflineResultCount         uint32                   `json:"offline_result_count"`
	OfflineReconciliationCount uint32                   `json:"offline_reconciliation_count"`
	CleanRestoreReady          bool                     `json:"clean_restore_ready"`
	RPO                        time.Duration            `json:"rpo"`
	RPOStatus                  string                   `json:"rpo_status"`
	RTO                        time.Duration            `json:"rto"`
	RTOStatus                  string                   `json:"rto_status"`
	QualifiedAt                time.Time                `json:"qualified_at"`
	ExpiresAt                  time.Time                `json:"expires_at"`
	Signature                  contracts.Signature      `json:"signature"`
}

func (value RecoveryQualification) Validate() error {
	if value.SchemaVersion != RecoveryQualificationSchemaVersion ||
		validateToken("qualification_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil ||
		validateToken("recovery_policy_id", string(value.RecoveryPolicyID)) != nil ||
		value.RecoveryPolicyVersion == 0 || value.RecoveryPolicyVersion > MaximumDatabaseValue ||
		value.RecoveryPolicyHash.Validate() != nil || validateToken("backup_id", string(value.BackupID)) != nil ||
		value.BackupManifestHash.Validate() != nil || value.ArchiveHash.Validate() != nil ||
		validateToken("restore_id", string(value.RestoreID)) != nil || value.RestoreReceiptHash.Validate() != nil ||
		validateToken("offline_batch_id", string(value.OfflineBatchID)) != nil || value.OfflineReceiptHash.Validate() != nil ||
		value.RestoredTables == 0 || value.RestoredRows > MaximumDatabaseValue ||
		value.CancelledRuntimeLeases > MaximumDatabaseValue || value.InvalidatedAuthorityLeases > MaximumDatabaseValue ||
		value.CoalescedWakes > MaximumDatabaseValue || value.QuarantinedEffects > MaximumDatabaseValue ||
		value.QuarantinedExternalState > MaximumDatabaseValue || value.OfflineResultCount == 0 ||
		value.OfflineReconciliationCount != 0 || !value.CleanRestoreReady || value.RPO <= 0 ||
		value.RPOStatus != "met" || value.RTO <= 0 || value.RTOStatus != "met" ||
		!validUTC(value.QualifiedAt) || !validUTC(value.ExpiresAt) || !value.ExpiresAt.After(value.QualifiedAt) ||
		value.ExpiresAt.Sub(value.QualifiedAt) > 30*24*time.Hour || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: recovery qualification is incomplete")
	}
	return nil
}

func (value ShutdownReceipt) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("company recovery: signed shutdown receipt is invalid")
	}
	return nil
}

type PITRBackend interface {
	Capture(context.Context, string, contracts.OrganizationID, string, time.Time) (string, error)
	Restore(context.Context, string, contracts.OrganizationID, string, time.Time) error
}

type ErasureBackend interface {
	Erase(context.Context, ErasureDirectiveBody) (uint32, uint64, error)
}

type MachineKeyResolver interface {
	ResolveMachineKey(context.Context, string, string) (ed25519.PublicKey, error)
}

func SignLimitPolicy(value *LimitPolicy, keyID string, privateKey ed25519.PrivateKey) error {
	return signHashed(value, &value.Body, keyID, privateKey, func(hash contracts.ContentHash, signature contracts.Signature) {
		value.ContentHash, value.Signature = hash, signature
	})
}

func VerifyLimitPolicy(value LimitPolicy, publicKey ed25519.PublicKey) error {
	return verifyHashed(&value, &value.Body, value.ContentHash, value.Signature, publicKey, func() {
		value.Signature = signingShape(value.Signature.KeyID)
	})
}

func SignRecoveryPolicy(value *RecoveryPolicy, keyID string, privateKey ed25519.PrivateKey) error {
	return signHashed(value, &value.Body, keyID, privateKey, func(hash contracts.ContentHash, signature contracts.Signature) {
		value.ContentHash, value.Signature = hash, signature
	})
}

func VerifyRecoveryPolicy(value RecoveryPolicy, publicKey ed25519.PublicKey) error {
	return verifyHashed(&value, &value.Body, value.ContentHash, value.Signature, publicKey, func() {
		value.Signature = signingShape(value.Signature.KeyID)
	})
}

func SignBackupAuthorization(value *BackupAuthorization, keyID string, privateKey ed25519.PrivateKey) error {
	return signSimple(value, keyID, privateKey, func(signature contracts.Signature) { value.Signature = signature })
}

func VerifyBackupAuthorization(value BackupAuthorization, publicKey ed25519.PublicKey) error {
	return verifySimple(&value, value.Signature, publicKey, func() { value.Signature = signingShape(value.Signature.KeyID) })
}

func VerifyBackupManifest(value BackupManifest, publicKey ed25519.PublicKey) error {
	return verifySimple(&value, value.Signature, publicKey, func() { value.Signature = signingShape(value.Signature.KeyID) })
}

func SignRestoreAuthorization(value *RestoreAuthorization, keyID string, privateKey ed25519.PrivateKey) error {
	return signSimple(value, keyID, privateKey, func(signature contracts.Signature) { value.Signature = signature })
}

func VerifyRestoreAuthorization(value RestoreAuthorization, publicKey ed25519.PublicKey) error {
	return verifySimple(&value, value.Signature, publicKey, func() { value.Signature = signingShape(value.Signature.KeyID) })
}

func VerifyRestoreReceipt(value RestoreReceipt, publicKey ed25519.PublicKey) error {
	return verifySimple(&value, value.Signature, publicKey, func() { value.Signature = signingShape(value.Signature.KeyID) })
}

func SignErasureDirective(value *ErasureDirective, keyID string, privateKey ed25519.PrivateKey) error {
	return signSimple(value, keyID, privateKey, func(signature contracts.Signature) { value.Signature = signature })
}

func VerifyErasureDirective(value ErasureDirective, publicKey ed25519.PublicKey) error {
	return verifySimple(&value, value.Signature, publicKey, func() { value.Signature = signingShape(value.Signature.KeyID) })
}

func VerifyErasureReceipt(value ErasureReceipt, publicKey ed25519.PublicKey) error {
	return verifySimple(&value, value.Signature, publicKey, func() { value.Signature = signingShape(value.Signature.KeyID) })
}

func SignOfflineBatch(value *OfflineBatch, keyID string, privateKey ed25519.PrivateKey) error {
	return signSimple(value, keyID, privateKey, func(signature contracts.Signature) { value.Signature = signature })
}

func VerifyOfflineBatch(value OfflineBatch, publicKey ed25519.PublicKey) error {
	return verifySimple(&value, value.Signature, publicKey, func() { value.Signature = signingShape(value.Signature.KeyID) })
}

func VerifyOfflineReceipt(value OfflineReceipt, publicKey ed25519.PublicKey) error {
	return verifySimple(&value, value.Signature, publicKey, func() { value.Signature = signingShape(value.Signature.KeyID) })
}

func SignOfflineReconciliationResolution(value *OfflineReconciliationResolution, keyID string, privateKey ed25519.PrivateKey) error {
	return signSimple(value, keyID, privateKey, func(signature contracts.Signature) { value.Signature = signature })
}

func VerifyOfflineReconciliationResolution(value OfflineReconciliationResolution, publicKey ed25519.PublicKey) error {
	return verifySimple(&value, value.Signature, publicKey, func() { value.Signature = signingShape(value.Signature.KeyID) })
}

func VerifyShutdownReceipt(value ShutdownReceipt, publicKey ed25519.PublicKey) error {
	return verifySimple(&value, value.Signature, publicKey, func() { value.Signature = signingShape(value.Signature.KeyID) })
}

func SignRecoveryQualification(value *RecoveryQualification, keyID string, privateKey ed25519.PrivateKey) error {
	return signSimple(value, keyID, privateKey, func(signature contracts.Signature) { value.Signature = signature })
}

func VerifyRecoveryQualification(value RecoveryQualification, publicKey ed25519.PublicKey) error {
	return verifySimple(&value, value.Signature, publicKey, func() { value.Signature = signingShape(value.Signature.KeyID) })
}

func SignMachineKeyRegistration(value *MachineKeyRegistration, keyID string, privateKey ed25519.PrivateKey) error {
	return signHashed(value, &value.Body, keyID, privateKey, func(hash contracts.ContentHash, signature contracts.Signature) {
		value.ContentHash, value.Signature = hash, signature
	})
}

func VerifyMachineKeyRegistration(value MachineKeyRegistration, publicKey ed25519.PublicKey) error {
	return verifyHashed(&value, &value.Body, value.ContentHash, value.Signature, publicKey, func() {
		value.Signature = signingShape(value.Signature.KeyID)
	})
}

func signHashed[T interface{ Validate() error }, B contracts.Validatable](value *T, body B, keyID string, privateKey ed25519.PrivateKey, assign func(contracts.ContentHash, contracts.Signature)) error {
	if value == nil || len(privateKey) != ed25519.PrivateKeySize || validateToken("signature key_id", keyID) != nil {
		return ErrUnauthorized
	}
	hash, err := contracts.HashCanonical(body)
	if err != nil {
		return err
	}
	assign(hash, signingShape(keyID))
	payload, err := contracts.EncodeCanonical(*value)
	if err != nil {
		return err
	}
	signature := signingShape(keyID)
	signature.Value = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	assign(hash, signature)
	return (*value).Validate()
}

func verifyHashed[T contracts.Validatable, B contracts.Validatable](value T, body B, hash contracts.ContentHash, signature contracts.Signature, publicKey ed25519.PublicKey, prepare func()) error {
	if value.Validate() != nil {
		return ErrUnauthorized
	}
	expected, err := contracts.HashCanonical(body)
	if err != nil || expected != hash {
		return ErrUnauthorized
	}
	prepare()
	payload, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	return verifySignature(payload, signature, publicKey)
}

func signSimple[T interface{ Validate() error }](value *T, keyID string, privateKey ed25519.PrivateKey, assign func(contracts.Signature)) error {
	if value == nil || len(privateKey) != ed25519.PrivateKeySize || validateToken("signature key_id", keyID) != nil {
		return ErrUnauthorized
	}
	assign(signingShape(keyID))
	payload, err := contracts.EncodeCanonical(*value)
	if err != nil {
		return err
	}
	signature := signingShape(keyID)
	signature.Value = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	assign(signature)
	return (*value).Validate()
}

func verifySimple[T contracts.Validatable](value T, signature contracts.Signature, publicKey ed25519.PublicKey, prepare func()) error {
	if value.Validate() != nil {
		return ErrUnauthorized
	}
	prepare()
	payload, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	return verifySignature(payload, signature, publicKey)
}

func signingShape(keyID string) contracts.Signature {
	return contracts.Signature{Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))}
}

func verifySignature(payload []byte, signature contracts.Signature, publicKey ed25519.PublicKey) error {
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || len(decoded) != ed25519.SignatureSize || len(publicKey) != ed25519.PublicKeySize ||
		!ed25519.Verify(publicKey, payload, decoded) {
		return ErrUnauthorized
	}
	return nil
}

func hashBytes(value []byte) contracts.ContentHash {
	sum := sha256.Sum256(value)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func hashRows(rows [][]byte) contracts.ContentHash {
	hash := sha256.New()
	for _, row := range rows {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(row)))
		hash.Write(length[:])
		hash.Write(row)
	}
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(hash.Sum(nil))}
}

func validateToken(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 {
		return fmt.Errorf("company recovery: %s must contain 1 to 256 bytes", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-_.:/@", character) {
			continue
		}
		return fmt.Errorf("company recovery: %s contains an invalid character", name)
	}
	return nil
}

func validateBounded(name, value string, maximum int) error {
	if strings.TrimSpace(value) == "" || len(value) > maximum {
		return fmt.Errorf("company recovery: %s is empty or oversized", name)
	}
	return nil
}

func validateText(name, value string, maximum int) error {
	if strings.TrimSpace(value) == "" || len(value) > maximum {
		return fmt.Errorf("company recovery: %s is empty or oversized", name)
	}
	return nil
}

func validUTC(value time.Time) bool       { return !value.IsZero() && value.Location() == time.UTC }
func scopeKey(value ScopeRef) string      { return string(value.Kind) + "/" + value.ID }
func dimensionKey(value Dimension) string { return value.Name + "/" + value.Value }
func offlineItemKey(value OfflineItem) string {
	return value.RecordKind + "/" + value.RecordID + fmt.Sprintf("/%020d", value.Version)
}
func offlineResultKey(value OfflineItemResult) string {
	return value.RecordKind + "/" + value.RecordID + fmt.Sprintf("/%020d", value.Version)
}
