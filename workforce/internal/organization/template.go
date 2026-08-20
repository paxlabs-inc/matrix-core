package organization

import (
	"crypto/ed25519"
	"fmt"
	"slices"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
)

const (
	SeatMandateSchemaVersion        = "workforce.organization-seat-mandate.v2"
	TemplateSchemaVersion           = "workforce.organization-template.v2"
	TemplateActivationSchemaVersion = "workforce.organization-template-activation.v1"
	MinimumDepartments              = 1
	MaximumDepartments              = 16
	MaximumDurableSeats             = 48
)

type TemplateID string

type TemplateMode string

const (
	TemplateLegacyProjection TemplateMode = "legacy_v1_projection"
	TemplateFullCompany      TemplateMode = "full_company"
)

func (value TemplateMode) Valid() bool {
	return value == TemplateLegacyProjection || value == TemplateFullCompany
}

type MandateOrigin string

const (
	MandateLegacyProjection MandateOrigin = "legacy_v1_projection"
	MandateOwnerNative      MandateOrigin = "owner_native_v2"
)

func (value MandateOrigin) Valid() bool {
	return value == MandateLegacyProjection || value == MandateOwnerNative
}

type ModelBindingRef struct {
	ID      contracts.SeatBindingID `json:"binding_id"`
	Version uint64                  `json:"version"`
	Digest  contracts.ContentHash   `json:"digest"`
}

func (value ModelBindingRef) Validate() error {
	if err := validateID("binding_id", string(value.ID)); err != nil {
		return err
	}
	if value.Version == 0 {
		return fmt.Errorf("organization: model binding version must be positive")
	}
	return value.Digest.Validate()
}

type LegacyAuthorityRef struct {
	OrganizationVersion uint64                `json:"organization_version"`
	SeatID              contracts.SeatID      `json:"seat_id"`
	SeatVersion         uint64                `json:"seat_version"`
	SeatDigest          contracts.ContentHash `json:"seat_digest"`
	MandateID           contracts.MandateID   `json:"mandate_id"`
	MandateVersion      uint64                `json:"mandate_version"`
	MandateDigest       contracts.ContentHash `json:"mandate_digest"`
}

func (value LegacyAuthorityRef) Validate() error {
	if value.OrganizationVersion == 0 || value.SeatVersion == 0 || value.MandateVersion == 0 {
		return fmt.Errorf("organization: legacy authority versions must be positive")
	}
	if err := validateID("legacy seat_id", string(value.SeatID)); err != nil {
		return err
	}
	if err := validateID("legacy mandate_id", string(value.MandateID)); err != nil {
		return err
	}
	if err := value.SeatDigest.Validate(); err != nil {
		return err
	}
	return value.MandateDigest.Validate()
}

type SeatMandate struct {
	SchemaVersion         string                     `json:"schema_version"`
	ID                    contracts.MandateID        `json:"mandate_id"`
	Version               uint64                     `json:"version"`
	OrganizationID        contracts.OrganizationID   `json:"organization_id"`
	DepartmentID          contracts.DepartmentID     `json:"department_id"`
	SeatID                contracts.SeatID           `json:"seat_id"`
	SeatDID               contracts.SeatDID          `json:"seat_did"`
	Role                  contracts.SeatRole         `json:"role"`
	Origin                MandateOrigin              `json:"origin"`
	LegacyAuthority       *LegacyAuthorityRef        `json:"legacy_authority"`
	AllowedCapabilities   []CapabilityRef            `json:"allowed_capabilities"`
	AllowedSkills         []contracts.SkillRef       `json:"allowed_skills"`
	DataScopes            []contracts.DataScope      `json:"data_scopes"`
	EscalationRules       []contracts.EscalationRule `json:"escalation_rules"`
	ModelBinding          ModelBindingRef            `json:"model_binding"`
	IndependenceDomain    string                     `json:"independence_domain"`
	ConflictDomains       []string                   `json:"conflict_domains"`
	Prohibitions          []contracts.Prohibition    `json:"prohibitions"`
	ReceiptSchemaVersions []string                   `json:"receipt_schema_versions"`
	ResourceLimit         ResourceVector             `json:"resource_limit"`
	EffectiveAt           time.Time                  `json:"effective_at"`
	ExpiresAt             *time.Time                 `json:"expires_at"`
	Signature             contracts.Signature        `json:"signature"`
}

func (value SeatMandate) Validate() error {
	if value.SchemaVersion != SeatMandateSchemaVersion || value.Version == 0 ||
		value.OrganizationID == "" {
		return fmt.Errorf("organization: seat mandate identity is incomplete")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"mandate_id", string(value.ID)},
		{"department_id", string(value.DepartmentID)},
		{"seat_id", string(value.SeatID)},
		{"seat_did", string(value.SeatDID)},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if !value.Role.Valid() || !value.Origin.Valid() {
		return fmt.Errorf("organization: seat mandate role or origin is invalid")
	}
	if value.Origin == MandateLegacyProjection && value.LegacyAuthority == nil ||
		value.Origin == MandateOwnerNative && value.LegacyAuthority != nil {
		return fmt.Errorf("organization: seat mandate legacy authority does not match its origin")
	}
	if value.LegacyAuthority != nil {
		if err := value.LegacyAuthority.Validate(); err != nil {
			return err
		}
		if value.LegacyAuthority.SeatID != value.SeatID ||
			value.LegacyAuthority.MandateID != value.ID ||
			value.LegacyAuthority.MandateVersion != value.Version {
			return fmt.Errorf("organization: projected seat mandate changes legacy identity")
		}
	}
	if len(value.AllowedCapabilities) == 0 || len(value.AllowedCapabilities) > 64 ||
		!capabilityRefsSorted(value.AllowedCapabilities) {
		return fmt.Errorf("organization: allowed capabilities must be sorted and contain 1 to 64 entries")
	}
	for index, reference := range value.AllowedCapabilities {
		if err := reference.Validate(); err != nil {
			return err
		}
		if index > 0 && reference.ID == value.AllowedCapabilities[index-1].ID {
			return fmt.Errorf("organization: allowed capability %q is duplicated", reference.ID)
		}
	}
	if len(value.AllowedSkills) == 0 || len(value.AllowedSkills) > 64 {
		return fmt.Errorf("organization: allowed skills must contain 1 to 64 entries")
	}
	previousSkill := ""
	for _, skill := range value.AllowedSkills {
		if err := skill.Validate(); err != nil {
			return err
		}
		if string(skill.ID) <= previousSkill {
			return fmt.Errorf("organization: allowed skills must be sorted and unique")
		}
		previousSkill = string(skill.ID)
	}
	if len(value.DataScopes) == 0 || len(value.DataScopes) > 32 {
		return fmt.Errorf("organization: seat mandate data scopes must contain 1 to 32 entries")
	}
	previousScope := ""
	for _, scope := range value.DataScopes {
		if err := scope.Validate(); err != nil {
			return err
		}
		if scope.Name <= previousScope {
			return fmt.Errorf("organization: seat mandate data scopes must be sorted and unique")
		}
		previousScope = scope.Name
	}
	if len(value.EscalationRules) == 0 || len(value.EscalationRules) > 32 {
		return fmt.Errorf("organization: escalation rules must contain 1 to 32 entries")
	}
	previousEscalation := ""
	for _, rule := range value.EscalationRules {
		if err := rule.Validate(); err != nil {
			return err
		}
		if rule.Condition <= previousEscalation {
			return fmt.Errorf("organization: escalation rules must be sorted and unique")
		}
		previousEscalation = rule.Condition
	}
	if err := value.ModelBinding.Validate(); err != nil {
		return err
	}
	if err := validateID("independence domain", value.IndependenceDomain); err != nil {
		return err
	}
	if value.ConflictDomains == nil {
		return fmt.Errorf("organization: conflict domains must be an explicit array")
	}
	if err := validateSortedUnique("conflict domains", value.ConflictDomains, 0, 32); err != nil {
		return err
	}
	if len(value.Prohibitions) < 4 || len(value.Prohibitions) > 32 {
		return fmt.Errorf("organization: seat mandate must contain 4 to 32 non-delegable prohibitions")
	}
	previousProhibition := ""
	for _, prohibition := range value.Prohibitions {
		if err := prohibition.Validate(); err != nil {
			return err
		}
		if prohibition.ClauseID <= previousProhibition {
			return fmt.Errorf("organization: prohibitions must be sorted and unique")
		}
		previousProhibition = prohibition.ClauseID
	}
	for _, required := range []string{
		"deny-credential-possession", "deny-owner-signing", "deny-policy-mutation", "deny-self-approval",
	} {
		found := false
		for _, prohibition := range value.Prohibitions {
			if prohibition.ClauseID == required {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("organization: seat mandate omits prohibition %q", required)
		}
	}
	if err := validateSortedUnique(
		"receipt schema versions", value.ReceiptSchemaVersions, 1, 16,
	); err != nil {
		return err
	}
	if err := value.ResourceLimit.Validate(); err != nil {
		return err
	}
	if !value.ResourceLimit.NonZero() {
		return fmt.Errorf("organization: seat resource limit must be non-zero")
	}
	if err := validateOptionalExpiry(value.EffectiveAt, value.ExpiresAt); err != nil {
		return err
	}
	return value.Signature.Validate()
}

type DepartmentTemplate struct {
	ID       contracts.DepartmentID `json:"department_id"`
	Key      string                 `json:"department_key"`
	Name     string                 `json:"name"`
	Mandates []SeatMandate          `json:"seat_mandates"`
}

func (value DepartmentTemplate) Validate(organizationID contracts.OrganizationID) error {
	if err := validateID("department_id", string(value.ID)); err != nil {
		return err
	}
	if err := validateID("department key", value.Key); err != nil {
		return err
	}
	if err := validateText("department name", value.Name, 160); err != nil {
		return err
	}
	if len(value.Mandates) != 3 {
		return fmt.Errorf("organization: department %q must contain exactly three seat mandates", value.ID)
	}
	seenRoles := make(map[contracts.SeatRole]bool, 3)
	seenSeats := make(map[contracts.SeatID]bool, 3)
	independenceDomains := make(map[string]contracts.SeatRole, 3)
	previousRole := ""
	for _, mandate := range value.Mandates {
		if err := mandate.Validate(); err != nil {
			return err
		}
		if mandate.OrganizationID != organizationID || mandate.DepartmentID != value.ID {
			return fmt.Errorf("organization: department mandate crosses topology scope")
		}
		if seenRoles[mandate.Role] || seenSeats[mandate.SeatID] {
			return fmt.Errorf("organization: department contains a duplicate role or seat")
		}
		if string(mandate.Role) <= previousRole {
			return fmt.Errorf("organization: department mandates must be sorted by role")
		}
		if existing, duplicate := independenceDomains[mandate.IndependenceDomain]; duplicate &&
			(existing == contracts.SeatAuditor || mandate.Role == contracts.SeatAuditor) {
			return fmt.Errorf("organization: department Auditor shares an independence domain")
		}
		seenRoles[mandate.Role] = true
		seenSeats[mandate.SeatID] = true
		independenceDomains[mandate.IndependenceDomain] = mandate.Role
		previousRole = string(mandate.Role)
	}
	for _, role := range contracts.AllSeatRoles() {
		if !seenRoles[role] {
			return fmt.Errorf("organization: department %q omits %s", value.ID, role)
		}
	}
	return nil
}

type OrganizationTemplate struct {
	SchemaVersion             string                   `json:"schema_version"`
	ID                        TemplateID               `json:"template_id"`
	Version                   uint64                   `json:"version"`
	OrganizationID            contracts.OrganizationID `json:"organization_id"`
	OwnerID                   contracts.OwnerID        `json:"owner_id"`
	Name                      string                   `json:"name"`
	Mode                      TemplateMode             `json:"mode"`
	Departments               []DepartmentTemplate     `json:"departments"`
	CapabilityRegistryDigest  contracts.ContentHash    `json:"capability_registry_digest"`
	LegacyOrganizationVersion uint64                   `json:"legacy_organization_version"`
	ReceiptSchemaVersions     []string                 `json:"receipt_schema_versions"`
	EffectiveAt               time.Time                `json:"effective_at"`
	ExpiresAt                 *time.Time               `json:"expires_at"`
	Signature                 contracts.Signature      `json:"signature"`
}

type ResolvedSeatBinding struct {
	OrganizationID           contracts.OrganizationID `json:"organization_id"`
	TemplateID               TemplateID               `json:"template_id"`
	TemplateVersion          uint64                   `json:"template_version"`
	TemplateDigest           contracts.ContentHash    `json:"template_digest"`
	CapabilityRegistryDigest contracts.ContentHash    `json:"capability_registry_digest"`
	DepartmentID             contracts.DepartmentID   `json:"department_id"`
	SeatID                   contracts.SeatID         `json:"seat_id"`
	Mandate                  SeatMandate              `json:"seat_mandate"`
	MandateDigest            contracts.ContentHash    `json:"mandate_digest"`
}

func (value ResolvedSeatBinding) Validate() error {
	if value.OrganizationID == "" || value.TemplateVersion == 0 {
		return fmt.Errorf("organization: resolved seat binding identity is incomplete")
	}
	for _, field := range []struct{ name, value string }{
		{"template_id", string(value.TemplateID)},
		{"department_id", string(value.DepartmentID)},
		{"seat_id", string(value.SeatID)},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := value.TemplateDigest.Validate(); err != nil {
		return err
	}
	if err := value.CapabilityRegistryDigest.Validate(); err != nil {
		return err
	}
	if err := value.Mandate.Validate(); err != nil {
		return err
	}
	if value.Mandate.OrganizationID != value.OrganizationID ||
		value.Mandate.DepartmentID != value.DepartmentID || value.Mandate.SeatID != value.SeatID {
		return fmt.Errorf("organization: resolved seat binding crosses topology scope")
	}
	digest, err := SeatMandateDigest(value.Mandate)
	if err != nil {
		return err
	}
	if digest != value.MandateDigest {
		return fmt.Errorf("organization: resolved seat mandate digest does not match")
	}
	return nil
}

type TemplateActivation struct {
	SchemaVersion             string                   `json:"schema_version"`
	ID                        string                   `json:"activation_id"`
	OrganizationID            contracts.OrganizationID `json:"organization_id"`
	OwnerID                   contracts.OwnerID        `json:"owner_id"`
	FromTemplateID            TemplateID               `json:"from_template_id"`
	FromTemplateVersion       uint64                   `json:"from_template_version"`
	FromTemplateDigest        contracts.ContentHash    `json:"from_template_digest"`
	ToTemplateID              TemplateID               `json:"to_template_id"`
	ToTemplateVersion         uint64                   `json:"to_template_version"`
	ToTemplateDigest          contracts.ContentHash    `json:"to_template_digest"`
	ExpectedProjectionVersion uint64                   `json:"expected_projection_version"`
	EffectiveAt               time.Time                `json:"effective_at"`
	ExpiresAt                 time.Time                `json:"expires_at"`
	Signature                 contracts.Signature      `json:"signature"`
}

func (value TemplateActivation) Validate() error {
	if value.SchemaVersion != TemplateActivationSchemaVersion || value.OrganizationID == "" ||
		value.OwnerID == "" || value.FromTemplateVersion == 0 || value.ToTemplateVersion == 0 ||
		value.ExpectedProjectionVersion == 0 || value.FromTemplateID == value.ToTemplateID &&
		value.FromTemplateVersion == value.ToTemplateVersion {
		return fmt.Errorf("organization: template activation identity is incomplete")
	}
	for _, field := range []struct{ name, value string }{
		{"activation_id", value.ID},
		{"from_template_id", string(value.FromTemplateID)},
		{"to_template_id", string(value.ToTemplateID)},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := value.FromTemplateDigest.Validate(); err != nil {
		return err
	}
	if err := value.ToTemplateDigest.Validate(); err != nil {
		return err
	}
	if !validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) {
		return fmt.Errorf("organization: template activation times are invalid")
	}
	return value.Signature.Validate()
}

func (value OrganizationTemplate) Validate() error {
	if value.SchemaVersion != TemplateSchemaVersion || value.Version == 0 ||
		value.OrganizationID == "" || value.OwnerID == "" || !value.Mode.Valid() {
		return fmt.Errorf("organization: template identity is incomplete")
	}
	if err := validateID("template_id", string(value.ID)); err != nil {
		return err
	}
	if err := validateText("template name", value.Name, 160); err != nil {
		return err
	}
	if len(value.Departments) < MinimumDepartments || len(value.Departments) > MaximumDepartments {
		return fmt.Errorf("organization: template must contain 1 to 16 departments")
	}
	seenDepartments := make(map[contracts.DepartmentID]bool, len(value.Departments))
	seenKeys := make(map[string]bool, len(value.Departments))
	seenSeats := make(map[contracts.SeatID]bool, len(value.Departments)*3)
	seenMandates := make(map[contracts.MandateID]bool, len(value.Departments)*3)
	previousDepartment := ""
	for _, department := range value.Departments {
		if string(department.ID) <= previousDepartment {
			return fmt.Errorf("organization: template departments must be sorted and unique")
		}
		if seenDepartments[department.ID] || seenKeys[department.Key] {
			return fmt.Errorf("organization: template contains a duplicate department")
		}
		if err := department.Validate(value.OrganizationID); err != nil {
			return err
		}
		for _, mandate := range department.Mandates {
			if mandate.Signature.KeyID != value.Signature.KeyID {
				return fmt.Errorf("organization: template mandates must use the template owner key")
			}
			if seenSeats[mandate.SeatID] {
				return fmt.Errorf("organization: seat %q appears in multiple departments", mandate.SeatID)
			}
			if seenMandates[mandate.ID] {
				return fmt.Errorf("organization: mandate %q appears more than once", mandate.ID)
			}
			seenSeats[mandate.SeatID] = true
			seenMandates[mandate.ID] = true
		}
		seenDepartments[department.ID] = true
		seenKeys[department.Key] = true
		previousDepartment = string(department.ID)
	}
	if len(seenSeats) != len(value.Departments)*3 || len(seenSeats) > MaximumDurableSeats {
		return fmt.Errorf("organization: template durable seat count is invalid")
	}
	if err := value.CapabilityRegistryDigest.Validate(); err != nil {
		return err
	}
	if value.Mode == TemplateLegacyProjection {
		if value.LegacyOrganizationVersion == 0 || len(value.Departments) != 7 || len(seenSeats) != 21 {
			return fmt.Errorf("organization: legacy projection must preserve the seven-department topology")
		}
	} else if value.LegacyOrganizationVersion != 0 {
		return fmt.Errorf("organization: owner-native template cannot claim a legacy organization version")
	}
	if err := validateSortedUnique(
		"receipt schema versions", value.ReceiptSchemaVersions, 1, 16,
	); err != nil {
		return err
	}
	if value.Mode == TemplateLegacyProjection &&
		!containsString(value.ReceiptSchemaVersions, contracts.SchemaVersionV1) {
		return fmt.Errorf("organization: legacy projection must preserve Workforce v1 receipts")
	}
	if err := validateOptionalExpiry(value.EffectiveAt, value.ExpiresAt); err != nil {
		return err
	}
	return value.Signature.Validate()
}

func ValidateTemplateAgainstRegistry(
	value OrganizationTemplate,
	registry *Registry,
	ownerKeyID string,
	ownerPublicKey ed25519.PublicKey,
	at time.Time,
	requireStartupCoverage bool,
) error {
	if registry == nil || value.OrganizationID != registry.OrganizationID() ||
		value.CapabilityRegistryDigest != registry.Digest() {
		return fmt.Errorf("organization: template capability registry does not match")
	}
	if err := VerifyOrganizationTemplate(value, ownerKeyID, ownerPublicKey); err != nil {
		return err
	}
	if !validUTC(at) || value.EffectiveAt.After(at) || value.ExpiresAt != nil && !value.ExpiresAt.After(at) {
		return fmt.Errorf("organization: template is not current")
	}
	coverage := make(map[CapabilityID]bool)
	for _, department := range value.Departments {
		for _, mandate := range department.Mandates {
			if err := VerifySeatMandate(mandate, ownerKeyID, ownerPublicKey); err != nil {
				return err
			}
			if mandate.EffectiveAt.After(at) || mandate.ExpiresAt != nil && !mandate.ExpiresAt.After(at) {
				return fmt.Errorf("organization: template contains a non-current mandate")
			}
			for _, reference := range mandate.AllowedCapabilities {
				definition, err := registry.Resolve(reference)
				if err != nil {
					return err
				}
				if !slices.Contains(definition.AllowedRoles, mandate.Role) {
					return fmt.Errorf("organization: capability %q does not permit seat role %q", reference.ID, mandate.Role)
				}
				if !mandateContainsSkills(mandate, definition.RequiredSkills) ||
					!mandateContainsDataScopes(mandate, definition.RequiredDataScopes) ||
					!definition.ResourceEstimate.Fits(mandate.ResourceLimit) ||
					!stringsIntersect(mandate.ReceiptSchemaVersions, definition.ReceiptSchemaVersions) {
					return fmt.Errorf("organization: capability %q exceeds mandate %q", reference.ID, mandate.ID)
				}
				coverage[reference.ID] = true
			}
		}
	}
	if requireStartupCoverage || value.Mode == TemplateFullCompany {
		missing := make([]string, 0)
		for _, capabilityID := range StartupCapabilities() {
			if !coverage[capabilityID] {
				missing = append(missing, string(capabilityID))
			}
		}
		if len(missing) != 0 {
			return fmt.Errorf("organization: startup capability coverage is incomplete: %s", strings.Join(missing, ","))
		}
	}
	return nil
}

func mandateContainsSkills(mandate SeatMandate, required []contracts.SkillID) bool {
	for _, skillID := range required {
		found := false
		for _, skill := range mandate.AllowedSkills {
			if skill.ID == skillID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func mandateContainsDataScopes(mandate SeatMandate, required []contracts.DataScope) bool {
	for _, wanted := range required {
		found := false
		for _, available := range mandate.DataScopes {
			if available.Name == wanted.Name && available.Classification == wanted.Classification &&
				available.Purpose == wanted.Purpose {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func stringsIntersect(left, right []string) bool {
	for _, value := range left {
		if slices.Contains(right, value) {
			return true
		}
	}
	return false
}

func copyTemplate(value OrganizationTemplate) OrganizationTemplate {
	value.Departments = append([]DepartmentTemplate(nil), value.Departments...)
	for departmentIndex := range value.Departments {
		department := &value.Departments[departmentIndex]
		department.Mandates = append([]SeatMandate(nil), department.Mandates...)
		for mandateIndex := range department.Mandates {
			mandate := &department.Mandates[mandateIndex]
			mandate.AllowedCapabilities = append([]CapabilityRef(nil), mandate.AllowedCapabilities...)
			mandate.AllowedSkills = append([]contracts.SkillRef(nil), mandate.AllowedSkills...)
			mandate.DataScopes = append([]contracts.DataScope(nil), mandate.DataScopes...)
			mandate.EscalationRules = append([]contracts.EscalationRule(nil), mandate.EscalationRules...)
			mandate.ConflictDomains = append([]string(nil), mandate.ConflictDomains...)
			mandate.Prohibitions = append([]contracts.Prohibition(nil), mandate.Prohibitions...)
			mandate.ReceiptSchemaVersions = append([]string(nil), mandate.ReceiptSchemaVersions...)
			if mandate.LegacyAuthority != nil {
				legacy := *mandate.LegacyAuthority
				mandate.LegacyAuthority = &legacy
			}
			if mandate.ExpiresAt != nil {
				expiresAt := *mandate.ExpiresAt
				mandate.ExpiresAt = &expiresAt
			}
		}
	}
	value.ReceiptSchemaVersions = append([]string(nil), value.ReceiptSchemaVersions...)
	if value.ExpiresAt != nil {
		expiresAt := *value.ExpiresAt
		value.ExpiresAt = &expiresAt
	}
	return value
}
