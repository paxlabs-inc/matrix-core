package organization

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

const (
	CapabilitySchemaVersion = "workforce.capability.v1"
	RegistrySchemaVersion   = "workforce.capability-registry.v1"
)

type CapabilityID string

const (
	CapabilityMarketResearch          CapabilityID = "market_research"
	CapabilityProductStrategy         CapabilityID = "product_strategy"
	CapabilityProductDesign           CapabilityID = "product_design"
	CapabilitySoftwareImplementation  CapabilityID = "software_implementation"
	CapabilityQualityVerification     CapabilityID = "quality_verification"
	CapabilityDeploymentReliability   CapabilityID = "deployment_reliability"
	CapabilityMarketingPublication    CapabilityID = "marketing_publication"
	CapabilitySalesGrowth             CapabilityID = "sales_growth"
	CapabilityBillingCollection       CapabilityID = "billing_collection"
	CapabilityCustomerOperations      CapabilityID = "customer_operations"
	CapabilityFinanceTreasuryAnalysis CapabilityID = "finance_treasury_analysis"
	CapabilityLegalComplianceReview   CapabilityID = "legal_compliance_review"
	CapabilityBusinessAnalytics       CapabilityID = "business_analytics"
)

func StartupCapabilities() []CapabilityID {
	return []CapabilityID{
		CapabilityBillingCollection,
		CapabilityBusinessAnalytics,
		CapabilityCustomerOperations,
		CapabilityDeploymentReliability,
		CapabilityFinanceTreasuryAnalysis,
		CapabilityLegalComplianceReview,
		CapabilityMarketResearch,
		CapabilityMarketingPublication,
		CapabilityProductDesign,
		CapabilityProductStrategy,
		CapabilityQualityVerification,
		CapabilitySalesGrowth,
		CapabilitySoftwareImplementation,
	}
}

type CapabilityKind string

const (
	CapabilityAnalysis       CapabilityKind = "analysis"
	CapabilityDecision       CapabilityKind = "decision"
	CapabilityExecution      CapabilityKind = "execution"
	CapabilityObservation    CapabilityKind = "observation"
	CapabilityEffectProposal CapabilityKind = "effect_proposal"
	CapabilityVerification   CapabilityKind = "verification"
)

func (value CapabilityKind) Valid() bool {
	switch value {
	case CapabilityAnalysis, CapabilityDecision, CapabilityExecution,
		CapabilityObservation, CapabilityEffectProposal, CapabilityVerification:
		return true
	default:
		return false
	}
}

type LifecycleStage string

const (
	LifecycleDiscover  LifecycleStage = "DISCOVER"
	LifecycleScreen    LifecycleStage = "SCREEN"
	LifecycleValidate  LifecycleStage = "VALIDATE"
	LifecycleDecide    LifecycleStage = "DECIDE"
	LifecycleFund      LifecycleStage = "FUND"
	LifecycleDesign    LifecycleStage = "DESIGN"
	LifecycleBuild     LifecycleStage = "BUILD"
	LifecycleVerify    LifecycleStage = "VERIFY"
	LifecycleLaunch    LifecycleStage = "LAUNCH"
	LifecycleAcquire   LifecycleStage = "ACQUIRE"
	LifecycleMonetize  LifecycleStage = "MONETIZE"
	LifecycleOperate   LifecycleStage = "OPERATE"
	LifecycleMeasure   LifecycleStage = "MEASURE"
	LifecycleScale     LifecycleStage = "SCALE"
	LifecyclePivot     LifecycleStage = "PIVOT"
	LifecycleMaintain  LifecycleStage = "MAINTAIN"
	LifecycleTerminate LifecycleStage = "TERMINATE"
	LifecyclePaused    LifecycleStage = "PAUSED"
)

func (value LifecycleStage) Valid() bool {
	switch value {
	case LifecycleDiscover, LifecycleScreen, LifecycleValidate, LifecycleDecide,
		LifecycleFund, LifecycleDesign, LifecycleBuild, LifecycleVerify,
		LifecycleLaunch, LifecycleAcquire, LifecycleMonetize, LifecycleOperate,
		LifecycleMeasure, LifecycleScale, LifecyclePivot, LifecycleMaintain,
		LifecycleTerminate, LifecyclePaused:
		return true
	default:
		return false
	}
}

type ResourceVector struct {
	ModelCalls       uint32 `json:"model_calls"`
	ToolCalls        uint32 `json:"tool_calls"`
	EffectDispatches uint16 `json:"effect_dispatches"`
	MemoryBytes      uint64 `json:"memory_bytes"`
	CostMinor        int64  `json:"cost_minor"`
	Currency         string `json:"currency"`
}

func (value ResourceVector) Validate() error {
	if value.CostMinor < 0 {
		return fmt.Errorf("organization: resource cost cannot be negative")
	}
	if value.MemoryBytes > uint64(^uint64(0)>>1) {
		return fmt.Errorf("organization: resource memory exceeds PostgreSQL integer capacity")
	}
	if err := validateID("resource currency", value.Currency); err != nil {
		return err
	}
	return nil
}

func (value ResourceVector) NonZero() bool {
	return value.ModelCalls != 0 || value.ToolCalls != 0 || value.EffectDispatches != 0 ||
		value.MemoryBytes != 0 || value.CostMinor != 0
}

func (value ResourceVector) Fits(limit ResourceVector) bool {
	return value.Currency == limit.Currency && value.ModelCalls <= limit.ModelCalls &&
		value.ToolCalls <= limit.ToolCalls && value.EffectDispatches <= limit.EffectDispatches &&
		value.MemoryBytes <= limit.MemoryBytes && value.CostMinor <= limit.CostMinor
}

func (value ResourceVector) Add(other ResourceVector) (ResourceVector, error) {
	if value.Currency != other.Currency {
		return ResourceVector{}, fmt.Errorf("organization: resource currencies are incompatible")
	}
	modelCalls := uint64(value.ModelCalls) + uint64(other.ModelCalls)
	toolCalls := uint64(value.ToolCalls) + uint64(other.ToolCalls)
	effects := uint32(value.EffectDispatches) + uint32(other.EffectDispatches)
	if modelCalls > uint64(^uint32(0)) || toolCalls > uint64(^uint32(0)) ||
		effects > uint32(^uint16(0)) || ^uint64(0)-value.MemoryBytes < other.MemoryBytes ||
		other.CostMinor > 0 && value.CostMinor > int64(^uint64(0)>>1)-other.CostMinor {
		return ResourceVector{}, fmt.Errorf("organization: resource vector overflow")
	}
	return ResourceVector{
		ModelCalls: uint32(modelCalls), ToolCalls: uint32(toolCalls),
		EffectDispatches: uint16(effects), MemoryBytes: value.MemoryBytes + other.MemoryBytes,
		CostMinor: value.CostMinor + other.CostMinor, Currency: value.Currency,
	}, nil
}

func (value ResourceVector) Subtract(other ResourceVector) (ResourceVector, error) {
	if value.Currency != other.Currency || other.ModelCalls > value.ModelCalls ||
		other.ToolCalls > value.ToolCalls || other.EffectDispatches > value.EffectDispatches ||
		other.MemoryBytes > value.MemoryBytes || other.CostMinor > value.CostMinor {
		return ResourceVector{}, fmt.Errorf("organization: resource reservation exceeds availability")
	}
	return ResourceVector{
		ModelCalls:       value.ModelCalls - other.ModelCalls,
		ToolCalls:        value.ToolCalls - other.ToolCalls,
		EffectDispatches: value.EffectDispatches - other.EffectDispatches,
		MemoryBytes:      value.MemoryBytes - other.MemoryBytes,
		CostMinor:        value.CostMinor - other.CostMinor,
		Currency:         value.Currency,
	}, nil
}

type CapabilityRef struct {
	ID      CapabilityID          `json:"capability_id"`
	Version uint64                `json:"version"`
	Digest  contracts.ContentHash `json:"digest"`
}

func (value CapabilityRef) Validate() error {
	if err := validateID("capability_id", string(value.ID)); err != nil {
		return err
	}
	if value.Version == 0 {
		return fmt.Errorf("organization: capability version must be positive")
	}
	return value.Digest.Validate()
}

type CapabilityDefinition struct {
	SchemaVersion         string                   `json:"schema_version"`
	ID                    CapabilityID             `json:"capability_id"`
	Version               uint64                   `json:"version"`
	OrganizationID        contracts.OrganizationID `json:"organization_id"`
	Name                  string                   `json:"name"`
	Purpose               string                   `json:"purpose"`
	Kind                  CapabilityKind           `json:"kind"`
	LifecycleStages       []LifecycleStage         `json:"lifecycle_stages"`
	AllowedRoles          []contracts.SeatRole     `json:"allowed_roles"`
	RequiredSkills        []contracts.SkillID      `json:"required_skills"`
	RequiredDataScopes    []contracts.DataScope    `json:"required_data_scopes"`
	ReceiptSchemaVersions []string                 `json:"receipt_schema_versions"`
	ResourceEstimate      ResourceVector           `json:"resource_estimate"`
	Previous              *CapabilityRef           `json:"previous"`
	EffectiveAt           time.Time                `json:"effective_at"`
	ExpiresAt             *time.Time               `json:"expires_at"`
	Signature             contracts.Signature      `json:"signature"`
}

func (value CapabilityDefinition) Validate() error {
	if value.SchemaVersion != CapabilitySchemaVersion {
		return fmt.Errorf("organization: unsupported capability schema %q", value.SchemaVersion)
	}
	if err := validateID("capability_id", string(value.ID)); err != nil {
		return err
	}
	if value.Version == 0 || value.OrganizationID == "" {
		return fmt.Errorf("organization: capability identity is incomplete")
	}
	if err := validateText("capability name", value.Name, 160); err != nil {
		return err
	}
	if err := validateText("capability purpose", value.Purpose, 1024); err != nil {
		return err
	}
	if !value.Kind.Valid() {
		return fmt.Errorf("organization: invalid capability kind %q", value.Kind)
	}
	if len(value.LifecycleStages) == 0 || len(value.LifecycleStages) > 18 {
		return fmt.Errorf("organization: capability lifecycle stages must contain 1 to 18 entries")
	}
	previousStage := ""
	for _, stage := range value.LifecycleStages {
		if !stage.Valid() || string(stage) <= previousStage {
			return fmt.Errorf("organization: capability lifecycle stages must be valid, sorted, and unique")
		}
		previousStage = string(stage)
	}
	if len(value.AllowedRoles) == 0 || len(value.AllowedRoles) > 3 {
		return fmt.Errorf("organization: capability allowed roles must contain 1 to 3 entries")
	}
	previousRole := ""
	for _, role := range value.AllowedRoles {
		if !role.Valid() || string(role) <= previousRole {
			return fmt.Errorf("organization: capability allowed roles must be valid, sorted, and unique")
		}
		if value.Kind == CapabilityVerification && role != contracts.SeatAuditor ||
			value.Kind != CapabilityVerification && role == contracts.SeatAuditor {
			return fmt.Errorf("organization: verification and production capability roles cannot be conflated")
		}
		previousRole = string(role)
	}
	skills := make([]string, len(value.RequiredSkills))
	for index, skillID := range value.RequiredSkills {
		skills[index] = string(skillID)
	}
	if err := validateSortedUnique("required skills", skills, 1, 32); err != nil {
		return err
	}
	if len(value.RequiredDataScopes) == 0 || len(value.RequiredDataScopes) > 32 {
		return fmt.Errorf("organization: required data scopes must contain 1 to 32 entries")
	}
	previousScope := ""
	for _, scope := range value.RequiredDataScopes {
		if err := scope.Validate(); err != nil {
			return err
		}
		if scope.Name <= previousScope {
			return fmt.Errorf("organization: required data scopes must be sorted and unique")
		}
		previousScope = scope.Name
	}
	if err := validateSortedUnique(
		"receipt schema versions", value.ReceiptSchemaVersions, 1, 16,
	); err != nil {
		return err
	}
	if err := value.ResourceEstimate.Validate(); err != nil {
		return err
	}
	if !value.ResourceEstimate.NonZero() {
		return fmt.Errorf("organization: capability resource estimate must be non-zero")
	}
	if value.Version == 1 && value.Previous != nil || value.Version > 1 && value.Previous == nil {
		return fmt.Errorf("organization: capability version chain is incomplete")
	}
	if value.Previous != nil {
		if err := value.Previous.Validate(); err != nil {
			return err
		}
		if value.Previous.ID != value.ID || value.Previous.Version+1 != value.Version {
			return fmt.Errorf("organization: capability previous version is not contiguous")
		}
	}
	if err := validateOptionalExpiry(value.EffectiveAt, value.ExpiresAt); err != nil {
		return err
	}
	return value.Signature.Validate()
}

func capabilityDigest(value CapabilityDefinition) (contracts.ContentHash, error) {
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(canonical)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func capabilityRefsSorted(values []CapabilityRef) bool {
	return slices.IsSortedFunc(values, func(left, right CapabilityRef) int {
		if compared := strings.Compare(string(left.ID), string(right.ID)); compared != 0 {
			return compared
		}
		if left.Version < right.Version {
			return -1
		}
		if left.Version > right.Version {
			return 1
		}
		return 0
	})
}
