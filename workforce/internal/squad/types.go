package squad

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/organization"
)

const (
	RequirementSchemaVersion  = "workforce.squad-requirement.v1"
	RuntimeStateSchemaVersion = "workforce.squad-seat-runtime-state.v1"
	AssignmentSchemaVersion   = "workforce.squad-assignment.v1"
	DefaultActiveSquads       = 4
	MaximumActiveSquads       = 8
	MaximumCandidates         = 48
	MaximumNeeds              = 64
	MaximumSearchNodes        = 500000
)

type AssignmentID string
type InitiativeID string

type NeedKind string

const (
	NeedWork         NeedKind = "work"
	NeedVerification NeedKind = "verification"
)

func (value NeedKind) Valid() bool {
	return value == NeedWork || value == NeedVerification
}

type CapabilityNeed struct {
	ID                    string                         `json:"need_id"`
	Kind                  NeedKind                       `json:"kind"`
	Capability            organization.CapabilityRef     `json:"capability"`
	AllowedRoles          []contracts.SeatRole           `json:"allowed_roles"`
	DataScopes            []contracts.DataScope          `json:"data_scopes"`
	Skills                []contracts.SkillRef           `json:"skills"`
	ModelBindings         []organization.ModelBindingRef `json:"model_bindings"`
	ReceiptSchemaVersions []string                       `json:"receipt_schema_versions"`
	Resources             organization.ResourceVector    `json:"resources"`
}

func (value CapabilityNeed) Validate() error {
	if err := validateID("need_id", value.ID); err != nil {
		return err
	}
	if !value.Kind.Valid() {
		return fmt.Errorf("squad: invalid need kind %q", value.Kind)
	}
	if err := value.Capability.Validate(); err != nil {
		return err
	}
	if len(value.AllowedRoles) == 0 || len(value.AllowedRoles) > 2 {
		return fmt.Errorf("squad: need allowed roles must contain 1 to 2 entries")
	}
	previousRole := ""
	for _, role := range value.AllowedRoles {
		if !role.Valid() || string(role) <= previousRole {
			return fmt.Errorf("squad: need allowed roles must be valid, sorted, and unique")
		}
		if value.Kind == NeedVerification && role != contracts.SeatAuditor ||
			value.Kind == NeedWork && role == contracts.SeatAuditor {
			return fmt.Errorf("squad: verification and production roles cannot be conflated")
		}
		previousRole = string(role)
	}
	if len(value.DataScopes) == 0 || len(value.DataScopes) > 32 {
		return fmt.Errorf("squad: need data scopes must contain 1 to 32 entries")
	}
	previousScope := ""
	for _, scope := range value.DataScopes {
		if err := scope.Validate(); err != nil {
			return err
		}
		if scope.Name <= previousScope {
			return fmt.Errorf("squad: need data scopes must be sorted and unique")
		}
		previousScope = scope.Name
	}
	if len(value.Skills) == 0 || len(value.Skills) > 32 {
		return fmt.Errorf("squad: need skills must contain 1 to 32 entries")
	}
	previousSkill := ""
	for _, skill := range value.Skills {
		if err := skill.Validate(); err != nil {
			return err
		}
		if string(skill.ID) <= previousSkill {
			return fmt.Errorf("squad: need skills must be sorted and unique")
		}
		previousSkill = string(skill.ID)
	}
	if len(value.ModelBindings) == 0 || len(value.ModelBindings) > 16 {
		return fmt.Errorf("squad: need model bindings must contain 1 to 16 entries")
	}
	previousModel := ""
	for _, binding := range value.ModelBindings {
		if err := binding.Validate(); err != nil {
			return err
		}
		identity := fmt.Sprintf("%s@%020d", binding.ID, binding.Version)
		if identity <= previousModel {
			return fmt.Errorf("squad: need model bindings must be sorted and unique")
		}
		previousModel = identity
	}
	if err := validateSortedUnique(
		"receipt schema versions", value.ReceiptSchemaVersions, 1, 16,
	); err != nil {
		return err
	}
	if err := value.Resources.Validate(); err != nil || !value.Resources.NonZero() {
		return fmt.Errorf("squad: need resource request is invalid")
	}
	return nil
}

type SegregationKind string

const (
	SegregationIndependentAuditor SegregationKind = "independent_auditor"
	SegregationDistinctDepartment SegregationKind = "distinct_audit_department"
	SegregationProhibitedPair     SegregationKind = "prohibited_seat_pair"
)

func (value SegregationKind) Valid() bool {
	return value == SegregationIndependentAuditor ||
		value == SegregationDistinctDepartment || value == SegregationProhibitedPair
}

type SeatPair struct {
	First  contracts.SeatID `json:"first_seat_id"`
	Second contracts.SeatID `json:"second_seat_id"`
}

func (value SeatPair) Validate() error {
	if err := validateID("first_seat_id", string(value.First)); err != nil {
		return err
	}
	if err := validateID("second_seat_id", string(value.Second)); err != nil {
		return err
	}
	if value.First >= value.Second {
		return fmt.Errorf("squad: seat pair must be canonically ordered and distinct")
	}
	return nil
}

type SegregationRule struct {
	ID   string          `json:"rule_id"`
	Kind SegregationKind `json:"kind"`
	Pair *SeatPair       `json:"pair"`
}

func (value SegregationRule) Validate() error {
	if err := validateID("segregation rule_id", value.ID); err != nil {
		return err
	}
	if !value.Kind.Valid() {
		return fmt.Errorf("squad: segregation rule kind is invalid")
	}
	if value.Kind == SegregationProhibitedPair && value.Pair == nil ||
		value.Kind != SegregationProhibitedPair && value.Pair != nil {
		return fmt.Errorf("squad: segregation rule pair does not match its kind")
	}
	if value.Pair != nil {
		return value.Pair.Validate()
	}
	return nil
}

type Requirement struct {
	SchemaVersion         string                      `json:"schema_version"`
	ID                    AssignmentID                `json:"assignment_id"`
	OrganizationID        contracts.OrganizationID    `json:"organization_id"`
	InitiativeID          InitiativeID                `json:"initiative_id"`
	LifecycleStage        organization.LifecycleStage `json:"lifecycle_stage"`
	GraphScopes           []string                    `json:"graph_scopes"`
	ConflictDomains       []string                    `json:"conflict_domains"`
	TemplateID            organization.TemplateID     `json:"template_id"`
	TemplateVersion       uint64                      `json:"template_version"`
	TemplateDigest        contracts.ContentHash       `json:"template_digest"`
	Needs                 []CapabilityNeed            `json:"needs"`
	RequiredRoles         []contracts.SeatRole        `json:"required_roles"`
	SegregationRules      []SegregationRule           `json:"segregation_rules"`
	ReceiptSchemaVersions []string                    `json:"receipt_schema_versions"`
	MaximumMembers        uint16                      `json:"maximum_members"`
	IssuedAt              time.Time                   `json:"issued_at"`
	ExpiresAt             time.Time                   `json:"expires_at"`
	IdempotencyKey        string                      `json:"idempotency_key"`
}

func (value Requirement) Validate() error {
	if value.SchemaVersion != RequirementSchemaVersion || value.OrganizationID == "" ||
		!value.LifecycleStage.Valid() || value.TemplateVersion == 0 {
		return fmt.Errorf("squad: requirement identity is incomplete")
	}
	for _, field := range []struct{ name, value string }{
		{"assignment_id", string(value.ID)},
		{"initiative_id", string(value.InitiativeID)},
		{"template_id", string(value.TemplateID)},
		{"idempotency_key", value.IdempotencyKey},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := value.TemplateDigest.Validate(); err != nil {
		return err
	}
	if err := validateSortedUnique("graph scopes", value.GraphScopes, 1, 64); err != nil {
		return err
	}
	if value.ConflictDomains == nil {
		return fmt.Errorf("squad: conflict domains must be an explicit array")
	}
	if err := validateSortedUnique("conflict domains", value.ConflictDomains, 0, 64); err != nil {
		return err
	}
	if len(value.Needs) < 2 || len(value.Needs) > MaximumNeeds {
		return fmt.Errorf("squad: requirement must contain 2 to %d capability needs", MaximumNeeds)
	}
	previousNeed := ""
	hasWork := false
	hasVerification := false
	rolesCovered := make(map[contracts.SeatRole]bool)
	for _, need := range value.Needs {
		if err := need.Validate(); err != nil {
			return err
		}
		if need.ID <= previousNeed {
			return fmt.Errorf("squad: needs must be sorted and unique")
		}
		previousNeed = need.ID
		hasWork = hasWork || need.Kind == NeedWork
		hasVerification = hasVerification || need.Kind == NeedVerification
		for _, role := range need.AllowedRoles {
			rolesCovered[role] = true
		}
	}
	if !hasWork || !hasVerification {
		return fmt.Errorf("squad: requirement must contain production and independent verification work")
	}
	if len(value.RequiredRoles) == 0 || len(value.RequiredRoles) > 3 {
		return fmt.Errorf("squad: required roles must contain 1 to 3 entries")
	}
	previousRole := ""
	for _, role := range value.RequiredRoles {
		if !role.Valid() || string(role) <= previousRole || !rolesCovered[role] {
			return fmt.Errorf("squad: required roles must be valid, sorted, unique, and supported by a need")
		}
		previousRole = string(role)
	}
	if !slices.Contains(value.RequiredRoles, contracts.SeatAuditor) {
		return fmt.Errorf("squad: independent Auditor role is required")
	}
	if len(value.SegregationRules) == 0 || len(value.SegregationRules) > 32 {
		return fmt.Errorf("squad: segregation rules must contain 1 to 32 entries")
	}
	previousRule := ""
	hasIndependentAuditor := false
	for _, rule := range value.SegregationRules {
		if err := rule.Validate(); err != nil {
			return err
		}
		if rule.ID <= previousRule {
			return fmt.Errorf("squad: segregation rules must be sorted and unique")
		}
		previousRule = rule.ID
		hasIndependentAuditor = hasIndependentAuditor || rule.Kind == SegregationIndependentAuditor
	}
	if !hasIndependentAuditor {
		return fmt.Errorf("squad: independent Auditor segregation rule is required")
	}
	if err := validateSortedUnique(
		"receipt schema versions", value.ReceiptSchemaVersions, 1, 16,
	); err != nil {
		return err
	}
	if value.MaximumMembers < 2 || value.MaximumMembers > MaximumCandidates {
		return fmt.Errorf("squad: maximum members must be between 2 and %d", MaximumCandidates)
	}
	if !validUTC(value.IssuedAt) || !validUTC(value.ExpiresAt) || !value.ExpiresAt.After(value.IssuedAt) {
		return fmt.Errorf("squad: requirement times are invalid")
	}
	return nil
}

type RuntimeAvailability string

const (
	RuntimeAvailable   RuntimeAvailability = "available"
	RuntimeUnavailable RuntimeAvailability = "unavailable"
)

func (value RuntimeAvailability) Valid() bool {
	return value == RuntimeAvailable || value == RuntimeUnavailable
}

type SeatRuntimeState struct {
	SchemaVersion      string                      `json:"schema_version"`
	ID                 string                      `json:"runtime_state_id"`
	Version            uint64                      `json:"version"`
	OrganizationID     contracts.OrganizationID    `json:"organization_id"`
	SeatID             contracts.SeatID            `json:"seat_id"`
	TemplateID         organization.TemplateID     `json:"template_id"`
	TemplateVersion    uint64                      `json:"template_version"`
	MandateDigest      contracts.ContentHash       `json:"mandate_digest"`
	Availability       RuntimeAvailability         `json:"availability"`
	AvailableFrom      time.Time                   `json:"available_from"`
	AvailableUntil     time.Time                   `json:"available_until"`
	HeldConflictScopes []string                    `json:"held_conflict_scopes"`
	ResourceAvailable  organization.ResourceVector `json:"resource_available"`
	ObservedAt         time.Time                   `json:"observed_at"`
	ExpiresAt          time.Time                   `json:"expires_at"`
	Signature          contracts.Signature         `json:"signature"`
}

func (value SeatRuntimeState) Validate() error {
	if value.SchemaVersion != RuntimeStateSchemaVersion || value.Version == 0 ||
		value.OrganizationID == "" || value.TemplateVersion == 0 || !value.Availability.Valid() {
		return fmt.Errorf("squad: runtime seat state identity is incomplete")
	}
	for _, field := range []struct{ name, value string }{
		{"runtime_state_id", value.ID},
		{"seat_id", string(value.SeatID)},
		{"template_id", string(value.TemplateID)},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := value.MandateDigest.Validate(); err != nil {
		return err
	}
	if !validUTC(value.AvailableFrom) || !validUTC(value.AvailableUntil) ||
		!value.AvailableUntil.After(value.AvailableFrom) || !validUTC(value.ObservedAt) ||
		!validUTC(value.ExpiresAt) || value.ObservedAt.Before(value.AvailableFrom) ||
		!value.ExpiresAt.After(value.ObservedAt) || value.ExpiresAt.After(value.AvailableUntil) {
		return fmt.Errorf("squad: runtime seat state times are invalid")
	}
	if value.HeldConflictScopes == nil {
		return fmt.Errorf("squad: held conflict scopes must be an explicit array")
	}
	if err := validateSortedUnique("held conflict scopes", value.HeldConflictScopes, 0, 64); err != nil {
		return err
	}
	if err := value.ResourceAvailable.Validate(); err != nil || !value.ResourceAvailable.NonZero() {
		return fmt.Errorf("squad: runtime seat resources are invalid")
	}
	return value.Signature.Validate()
}

type Candidate struct {
	DepartmentID         contracts.DepartmentID      `json:"department_id"`
	Mandate              organization.SeatMandate    `json:"mandate"`
	Runtime              SeatRuntimeState            `json:"runtime"`
	ReservedResources    organization.ResourceVector `json:"reserved_resources"`
	ActiveConflictScopes []string                    `json:"active_conflict_scopes"`
}

func (value Candidate) Validate() error {
	if err := value.Mandate.Validate(); err != nil {
		return err
	}
	if err := value.Runtime.Validate(); err != nil {
		return err
	}
	mandateDigest, err := organization.SeatMandateDigest(value.Mandate)
	if err != nil {
		return err
	}
	if value.DepartmentID != value.Mandate.DepartmentID ||
		value.Mandate.SeatID != value.Runtime.SeatID ||
		value.Mandate.OrganizationID != value.Runtime.OrganizationID ||
		value.Runtime.MandateDigest != mandateDigest {
		return fmt.Errorf("squad: candidate authority and runtime state do not match")
	}
	if err := value.ReservedResources.Validate(); err != nil {
		return err
	}
	if _, err := value.Runtime.ResourceAvailable.Subtract(value.ReservedResources); err != nil {
		return err
	}
	if err := validateSortedUnique("active conflict scopes", value.ActiveConflictScopes, 0, 128); err != nil {
		return err
	}
	return nil
}

func (value Candidate) RemainingResources() (organization.ResourceVector, error) {
	return value.Runtime.ResourceAvailable.Subtract(value.ReservedResources)
}

type AuthorityEffect string

const AuthorityEffectNone AuthorityEffect = "none"

type AssignmentMember struct {
	SeatID             contracts.SeatID             `json:"seat_id"`
	DepartmentID       contracts.DepartmentID       `json:"department_id"`
	Role               contracts.SeatRole           `json:"role"`
	MandateID          contracts.MandateID          `json:"mandate_id"`
	MandateVersion     uint64                       `json:"mandate_version"`
	MandateDigest      contracts.ContentHash        `json:"mandate_digest"`
	ModelBinding       organization.ModelBindingRef `json:"model_binding"`
	IndependenceDomain string                       `json:"independence_domain"`
	NeedIDs            []string                     `json:"need_ids"`
	AllocatedResources organization.ResourceVector  `json:"allocated_resources"`
}

func (value AssignmentMember) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"seat_id", string(value.SeatID)},
		{"department_id", string(value.DepartmentID)},
		{"mandate_id", string(value.MandateID)},
		{"independence_domain", value.IndependenceDomain},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if !value.Role.Valid() || value.MandateVersion == 0 {
		return fmt.Errorf("squad: assignment member role or mandate version is invalid")
	}
	if err := value.MandateDigest.Validate(); err != nil {
		return err
	}
	if err := value.ModelBinding.Validate(); err != nil {
		return err
	}
	if err := validateSortedUnique("assignment member need IDs", value.NeedIDs, 1, MaximumNeeds); err != nil {
		return err
	}
	return value.AllocatedResources.Validate()
}

type Assignment struct {
	SchemaVersion         string                      `json:"schema_version"`
	ID                    AssignmentID                `json:"assignment_id"`
	OrganizationID        contracts.OrganizationID    `json:"organization_id"`
	InitiativeID          InitiativeID                `json:"initiative_id"`
	LifecycleStage        organization.LifecycleStage `json:"lifecycle_stage"`
	GraphScopes           []string                    `json:"graph_scopes"`
	ConflictDomains       []string                    `json:"conflict_domains"`
	TemplateID            organization.TemplateID     `json:"template_id"`
	TemplateVersion       uint64                      `json:"template_version"`
	TemplateDigest        contracts.ContentHash       `json:"template_digest"`
	RequirementDigest     contracts.ContentHash       `json:"requirement_digest"`
	Members               []AssignmentMember          `json:"members"`
	SatisfiedRuleIDs      []string                    `json:"satisfied_rule_ids"`
	ReceiptSchemaVersions []string                    `json:"receipt_schema_versions"`
	AuthorityEffect       AuthorityEffect             `json:"authority_effect"`
	IssuedAt              time.Time                   `json:"issued_at"`
	ExpiresAt             time.Time                   `json:"expires_at"`
	Signature             contracts.Signature         `json:"signature"`
}

func (value Assignment) Validate() error {
	if value.SchemaVersion != AssignmentSchemaVersion || value.OrganizationID == "" ||
		!value.LifecycleStage.Valid() || value.TemplateVersion == 0 ||
		value.AuthorityEffect != AuthorityEffectNone {
		return fmt.Errorf("squad: assignment identity or authority effect is invalid")
	}
	for _, field := range []struct{ name, value string }{
		{"assignment_id", string(value.ID)},
		{"initiative_id", string(value.InitiativeID)},
		{"template_id", string(value.TemplateID)},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := value.TemplateDigest.Validate(); err != nil {
		return err
	}
	if err := value.RequirementDigest.Validate(); err != nil {
		return err
	}
	if err := validateSortedUnique("graph scopes", value.GraphScopes, 1, 64); err != nil {
		return err
	}
	if value.ConflictDomains == nil {
		return fmt.Errorf("squad: conflict domains must be an explicit array")
	}
	if err := validateSortedUnique("conflict domains", value.ConflictDomains, 0, 64); err != nil {
		return err
	}
	if len(value.Members) < 2 || len(value.Members) > MaximumCandidates {
		return fmt.Errorf("squad: assignment must contain 2 to %d members", MaximumCandidates)
	}
	previousSeat := ""
	for _, member := range value.Members {
		if err := member.Validate(); err != nil {
			return err
		}
		if string(member.SeatID) <= previousSeat {
			return fmt.Errorf("squad: assignment members must be sorted and unique")
		}
		previousSeat = string(member.SeatID)
	}
	if err := validateSortedUnique("satisfied segregation rules", value.SatisfiedRuleIDs, 1, 32); err != nil {
		return err
	}
	if err := validateSortedUnique(
		"receipt schema versions", value.ReceiptSchemaVersions, 1, 16,
	); err != nil {
		return err
	}
	if !validUTC(value.IssuedAt) || !validUTC(value.ExpiresAt) || !value.ExpiresAt.After(value.IssuedAt) {
		return fmt.Errorf("squad: assignment times are invalid")
	}
	return value.Signature.Validate()
}

func validateID(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("squad: %s must contain 1 to 128 bytes", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' ||
			character == '.' || character == ':' || character == '/' {
			continue
		}
		return fmt.Errorf("squad: %s contains an invalid character", name)
	}
	return nil
}

func validateSortedUnique(name string, values []string, minimum, maximum int) error {
	if len(values) < minimum || len(values) > maximum || !slices.IsSorted(values) {
		return fmt.Errorf("squad: %s must contain %d to %d sorted entries", name, minimum, maximum)
	}
	for index, value := range values {
		if err := validateID(name, value); err != nil {
			return err
		}
		if index > 0 && values[index-1] == value {
			return fmt.Errorf("squad: %s contains a duplicate", name)
		}
	}
	return nil
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}
