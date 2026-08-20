package organization

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/policy"
)

type LegacyProjectionInput struct {
	TemplateID            TemplateID
	TemplateVersion       uint64
	Name                  string
	Organization          contracts.Organization
	Mandates              []contracts.Mandate
	SkillVersions         []contracts.SkillRef
	ModelBindings         []ModelBindingRef
	SeatResourceLimits    []SeatResourceLimit
	ReceiptSchemaVersions []string
	EffectiveAt           time.Time
	LegacyOwnerKeyID      string
	LegacyOwnerPublicKey  ed25519.PublicKey
}

type legacyOrganizationSigningPayload struct {
	SchemaVersion string                   `json:"schema_version"`
	ID            contracts.OrganizationID `json:"organization_id"`
	OwnerID       contracts.OwnerID        `json:"owner_id"`
	Version       uint64                   `json:"version"`
	Name          string                   `json:"name"`
	Departments   []contracts.Department   `json:"departments"`
	EffectiveAt   time.Time                `json:"effective_at"`
}

func (legacyOrganizationSigningPayload) Validate() error { return nil }

type SeatResourceLimit struct {
	SeatID contracts.SeatID `json:"seat_id"`
	Limit  ResourceVector   `json:"limit"`
}

func (value SeatResourceLimit) Validate() error {
	if err := validateID("seat_id", string(value.SeatID)); err != nil {
		return err
	}
	return value.Limit.Validate()
}

func ProjectLegacyTemplate(
	input LegacyProjectionInput,
	registry *Registry,
	ownerKeyID string,
	ownerPrivateKey ed25519.PrivateKey,
) (OrganizationTemplate, error) {
	if registry == nil || len(ownerPrivateKey) != ed25519.PrivateKeySize || ownerKeyID == "" {
		return OrganizationTemplate{}, fmt.Errorf("organization: projection registry and owner authority are required")
	}
	if err := input.Organization.Validate(); err != nil {
		return OrganizationTemplate{}, err
	}
	ownerPublicKey := ownerPrivateKey.Public().(ed25519.PublicKey)
	legacyKeyID := input.LegacyOwnerKeyID
	legacyPublicKey := input.LegacyOwnerPublicKey
	if legacyKeyID == "" && len(legacyPublicKey) == 0 {
		legacyKeyID = ownerKeyID
		legacyPublicKey = ownerPublicKey
	}
	if len(legacyPublicKey) != ed25519.PublicKeySize ||
		input.Organization.Signature.KeyID != legacyKeyID ||
		input.Organization.ID != registry.OrganizationID() ||
		verifyLegacyOrganization(input.Organization, legacyKeyID, legacyPublicKey) != nil {
		return OrganizationTemplate{}, fmt.Errorf("organization: legacy organization authority does not match")
	}
	if input.TemplateVersion == 0 || input.TemplateID == "" ||
		!validUTC(input.EffectiveAt) || input.EffectiveAt.Before(input.Organization.EffectiveAt) {
		return OrganizationTemplate{}, fmt.Errorf("organization: projection identity or time is invalid")
	}
	if err := validateSortedUnique(
		"receipt schema versions", input.ReceiptSchemaVersions, 1, 16,
	); err != nil {
		return OrganizationTemplate{}, err
	}
	mandates := make(map[contracts.MandateID]contracts.Mandate, len(input.Mandates))
	for _, mandate := range input.Mandates {
		if mandate.OrganizationID != input.Organization.ID ||
			policy.VerifyMandateAuthority(mandate, legacyKeyID, legacyPublicKey) != nil {
			return OrganizationTemplate{}, fmt.Errorf("organization: legacy mandate authority is invalid")
		}
		if _, duplicate := mandates[mandate.ID]; duplicate {
			return OrganizationTemplate{}, fmt.Errorf("organization: duplicate legacy mandate %q", mandate.ID)
		}
		mandates[mandate.ID] = mandate
	}
	skillVersions := make(map[contracts.SkillID]contracts.SkillRef, len(input.SkillVersions))
	for _, skill := range input.SkillVersions {
		if err := skill.Validate(); err != nil {
			return OrganizationTemplate{}, err
		}
		if _, duplicate := skillVersions[skill.ID]; duplicate {
			return OrganizationTemplate{}, fmt.Errorf("organization: duplicate skill version %q", skill.ID)
		}
		skillVersions[skill.ID] = skill
	}
	resourceLimits := make(map[contracts.SeatID]ResourceVector, len(input.SeatResourceLimits))
	for _, item := range input.SeatResourceLimits {
		if err := item.Validate(); err != nil {
			return OrganizationTemplate{}, err
		}
		if _, duplicate := resourceLimits[item.SeatID]; duplicate {
			return OrganizationTemplate{}, fmt.Errorf("organization: duplicate seat resource limit %q", item.SeatID)
		}
		resourceLimits[item.SeatID] = item.Limit
	}
	modelBindings := make(map[string]ModelBindingRef, len(input.ModelBindings))
	for _, binding := range input.ModelBindings {
		if err := binding.Validate(); err != nil {
			return OrganizationTemplate{}, err
		}
		identity := modelBindingIdentity(binding.ID, binding.Version)
		if _, duplicate := modelBindings[identity]; duplicate {
			return OrganizationTemplate{}, fmt.Errorf("organization: duplicate model binding %q", identity)
		}
		modelBindings[identity] = binding
	}
	template := OrganizationTemplate{
		SchemaVersion: TemplateSchemaVersion,
		ID:            input.TemplateID, Version: input.TemplateVersion,
		OrganizationID: input.Organization.ID, OwnerID: input.Organization.OwnerID,
		Name: input.Name, Mode: TemplateLegacyProjection,
		Departments:               make([]DepartmentTemplate, 0, len(input.Organization.Departments)),
		CapabilityRegistryDigest:  registry.Digest(),
		LegacyOrganizationVersion: input.Organization.Version,
		ReceiptSchemaVersions:     append([]string(nil), input.ReceiptSchemaVersions...),
		EffectiveAt:               input.EffectiveAt,
		Signature:                 signaturePreimage(ownerKeyID),
	}
	for _, legacyDepartment := range input.Organization.Departments {
		department := DepartmentTemplate{
			ID: legacyDepartment.ID, Key: string(legacyDepartment.Kind),
			Name:     legacyDepartmentName(legacyDepartment.Kind),
			Mandates: make([]SeatMandate, 0, 3),
		}
		for _, legacySeat := range legacyDepartment.Seats {
			if policy.VerifySeatAuthority(legacySeat, legacyKeyID, legacyPublicKey) != nil {
				return OrganizationTemplate{}, fmt.Errorf("organization: legacy seat authority is invalid")
			}
			legacyMandate, exists := mandates[legacySeat.MandateID]
			if !exists || legacyMandate.Version != legacySeat.MandateVersion ||
				legacyMandate.DepartmentKind != legacyDepartment.Kind || legacyMandate.SeatRole != legacySeat.Role {
				return OrganizationTemplate{}, fmt.Errorf("organization: legacy seat mandate binding is invalid")
			}
			if input.EffectiveAt.Before(legacySeat.EffectiveAt) ||
				input.EffectiveAt.Before(legacyMandate.EffectiveAt) ||
				legacyMandate.ExpiresAt != nil && !legacyMandate.ExpiresAt.After(input.EffectiveAt) {
				return OrganizationTemplate{}, fmt.Errorf("organization: legacy seat or mandate is not effective at projection")
			}
			seatDigest, err := contracts.HashCanonical(&legacySeat)
			if err != nil {
				return OrganizationTemplate{}, err
			}
			mandateDigest, err := contracts.HashCanonical(&legacyMandate)
			if err != nil {
				return OrganizationTemplate{}, err
			}
			allowedSkills := make([]contracts.SkillRef, 0, len(legacyMandate.AllowedSkills))
			for _, skillID := range legacyMandate.AllowedSkills {
				skill, exists := skillVersions[skillID]
				if !exists {
					return OrganizationTemplate{}, fmt.Errorf("organization: legacy skill %q lacks a versioned contract", skillID)
				}
				allowedSkills = append(allowedSkills, skill)
			}
			slices.SortFunc(allowedSkills, func(left, right contracts.SkillRef) int {
				return strings.Compare(string(left.ID), string(right.ID))
			})
			allowedCapabilities, err := projectedCapabilities(
				registry, legacySeat.Role, legacyMandate, allowedSkills,
			)
			if err != nil {
				return OrganizationTemplate{}, err
			}
			limit, exists := resourceLimits[legacySeat.ID]
			if !exists {
				return OrganizationTemplate{}, fmt.Errorf("organization: seat %q lacks an explicit resource limit", legacySeat.ID)
			}
			binding, exists := modelBindings[modelBindingIdentity(legacySeat.BindingID, legacySeat.BindingVersion)]
			if !exists {
				return OrganizationTemplate{}, fmt.Errorf("organization: seat %q lacks an exact model binding digest", legacySeat.ID)
			}
			prohibitions := projectedProhibitions(legacyMandate.Prohibitions)
			mandate := SeatMandate{
				SchemaVersion: SeatMandateSchemaVersion,
				ID:            legacyMandate.ID, Version: legacyMandate.Version,
				OrganizationID: input.Organization.ID, DepartmentID: legacyDepartment.ID,
				SeatID: legacySeat.ID, SeatDID: legacySeat.DID, Role: legacySeat.Role,
				Origin: MandateLegacyProjection,
				LegacyAuthority: &LegacyAuthorityRef{
					OrganizationVersion: input.Organization.Version,
					SeatID:              legacySeat.ID, SeatVersion: legacySeat.Version, SeatDigest: seatDigest,
					MandateID: legacyMandate.ID, MandateVersion: legacyMandate.Version,
					MandateDigest: mandateDigest,
				},
				AllowedCapabilities: allowedCapabilities,
				AllowedSkills:       allowedSkills,
				DataScopes:          append([]contracts.DataScope(nil), legacyMandate.DataScopes...),
				EscalationRules:     append([]contracts.EscalationRule(nil), legacyMandate.EscalationRules...),
				ModelBinding:        binding,
				IndependenceDomain:  independenceDomain(legacyDepartment.ID, legacySeat.Role),
				ConflictDomains:     []string{}, Prohibitions: prohibitions,
				ReceiptSchemaVersions: append([]string(nil), input.ReceiptSchemaVersions...),
				ResourceLimit:         limit, EffectiveAt: input.EffectiveAt,
				ExpiresAt: copyTimePointer(legacyMandate.ExpiresAt),
				Signature: signaturePreimage(ownerKeyID),
			}
			slices.SortFunc(mandate.DataScopes, func(left, right contracts.DataScope) int {
				return strings.Compare(left.Name, right.Name)
			})
			slices.SortFunc(mandate.EscalationRules, func(left, right contracts.EscalationRule) int {
				return strings.Compare(left.Condition, right.Condition)
			})
			if err := SignSeatMandate(&mandate, ownerKeyID, ownerPrivateKey); err != nil {
				return OrganizationTemplate{}, err
			}
			department.Mandates = append(department.Mandates, mandate)
		}
		slices.SortFunc(department.Mandates, func(left, right SeatMandate) int {
			return strings.Compare(string(left.Role), string(right.Role))
		})
		template.Departments = append(template.Departments, department)
	}
	slices.SortFunc(template.Departments, func(left, right DepartmentTemplate) int {
		return strings.Compare(string(left.ID), string(right.ID))
	})
	if err := SignOrganizationTemplate(&template, ownerKeyID, ownerPrivateKey); err != nil {
		return OrganizationTemplate{}, err
	}
	if err := ValidateTemplateAgainstRegistry(
		template, registry, ownerKeyID, ownerPublicKey, input.EffectiveAt, false,
	); err != nil {
		return OrganizationTemplate{}, err
	}
	return template, nil
}

func verifyLegacyOrganization(
	value contracts.Organization,
	keyID string,
	publicKey ed25519.PublicKey,
) error {
	if value.Signature.Algorithm != "ed25519" || value.Signature.KeyID != keyID ||
		len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("organization: legacy organization signature authority is invalid")
	}
	payload, err := contracts.EncodeCanonical(&legacyOrganizationSigningPayload{
		SchemaVersion: value.SchemaVersion,
		ID:            value.ID,
		OwnerID:       value.OwnerID,
		Version:       value.Version,
		Name:          value.Name,
		Departments:   value.Departments,
		EffectiveAt:   value.EffectiveAt,
	})
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(value.Signature.Value)
	if err != nil || !ed25519.Verify(publicKey, payload, signature) {
		return fmt.Errorf("organization: legacy organization signature verification failed")
	}
	return nil
}

func modelBindingIdentity(id contracts.SeatBindingID, version uint64) string {
	return fmt.Sprintf("%s@%020d", id, version)
}

func copyTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func projectedCapabilities(
	registry *Registry,
	role contracts.SeatRole,
	legacyMandate contracts.Mandate,
	allowedSkills []contracts.SkillRef,
) ([]CapabilityRef, error) {
	result := make([]CapabilityRef, 0)
	for _, reference := range registry.CurrentReferences() {
		definition, err := registry.Resolve(reference)
		if err != nil {
			return nil, err
		}
		candidate := SeatMandate{
			AllowedSkills: allowedSkills,
			DataScopes:    legacyMandate.DataScopes,
		}
		if slices.Contains(definition.AllowedRoles, role) &&
			mandateContainsSkills(candidate, definition.RequiredSkills) &&
			mandateContainsDataScopes(candidate, definition.RequiredDataScopes) {
			result = append(result, reference)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("organization: legacy mandate %q maps to no capability", legacyMandate.ID)
	}
	slices.SortFunc(result, func(left, right CapabilityRef) int {
		return strings.Compare(string(left.ID), string(right.ID))
	})
	return result, nil
}

func projectedProhibitions(existing []contracts.Prohibition) []contracts.Prohibition {
	byID := make(map[string]contracts.Prohibition, len(existing)+4)
	for _, prohibition := range existing {
		byID[prohibition.ClauseID] = prohibition
	}
	for _, prohibition := range []contracts.Prohibition{
		{ClauseID: "deny-credential-possession", Description: "Seat processes cannot possess provider, browser, deployment, financial, Vault, or database credentials."},
		{ClauseID: "deny-owner-signing", Description: "The seat cannot sign as or impersonate the founder authority."},
		{ClauseID: "deny-policy-mutation", Description: "The seat cannot mutate policy, mandate, capability, lease, or control-plane authority."},
		{ClauseID: "deny-self-approval", Description: "The seat cannot approve or independently verify its own work or financial result."},
	} {
		byID[prohibition.ClauseID] = prohibition
	}
	result := make([]contracts.Prohibition, 0, len(byID))
	for _, prohibition := range byID {
		result = append(result, prohibition)
	}
	slices.SortFunc(result, func(left, right contracts.Prohibition) int {
		return strings.Compare(left.ClauseID, right.ClauseID)
	})
	return result
}

func independenceDomain(departmentID contracts.DepartmentID, role contracts.SeatRole) string {
	if role == contracts.SeatAuditor {
		return "audit:" + string(departmentID)
	}
	return "production:" + string(departmentID)
}

func legacyDepartmentName(kind contracts.DepartmentKind) string {
	switch kind {
	case contracts.DepartmentDeveloper:
		return "Developer"
	case contracts.DepartmentExecutive:
		return "Executive"
	case contracts.DepartmentResearch:
		return "Research and Development"
	case contracts.DepartmentMarketing:
		return "Marketing and Social"
	case contracts.DepartmentLegal:
		return "Legal"
	case contracts.DepartmentAccounting:
		return "Accounting"
	case contracts.DepartmentBackOffice:
		return "Back Office"
	default:
		return string(kind)
	}
}
