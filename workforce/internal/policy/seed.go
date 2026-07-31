package policy

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

var departmentNames = map[contracts.DepartmentKind]string{
	contracts.DepartmentDeveloper:  "Developer",
	contracts.DepartmentExecutive:  "Executive",
	contracts.DepartmentResearch:   "Research and Development",
	contracts.DepartmentMarketing:  "Marketing and Social",
	contracts.DepartmentLegal:      "Legal",
	contracts.DepartmentAccounting: "Accounting",
	contracts.DepartmentBackOffice: "Back Office",
}

var departmentSkills = map[contracts.DepartmentKind][]contracts.SkillID{
	contracts.DepartmentDeveloper: {
		"developer.implement", "developer.plan", "developer.project_brain_update",
		"developer.review_handoff", "developer.verify",
	},
	contracts.DepartmentExecutive: {
		"evidence-review", "portfolio-analysis", "portfolio-planning", "typed-handoff",
	},
	contracts.DepartmentResearch: {
		"evidence-review", "experiment-design", "research-analysis", "typed-handoff",
	},
	contracts.DepartmentMarketing: {
		"campaign-operations", "campaign-research", "channel-evidence", "content-operations", "publication-gates",
	},
	contracts.DepartmentLegal: {
		"compliance-workflow", "contract-analysis", "evidence-review", "issue-spotting", "jurisdiction-check",
	},
	contracts.DepartmentAccounting: {
		"bookkeeping", "close-workflow", "payment-proposal", "reconciliation", "reporting",
	},
	contracts.DepartmentBackOffice: {
		"administrative-workflow", "records", "scheduling", "sla-tracking", "vendor-coordination",
	},
}

func BuildSeed(
	organizationID contracts.OrganizationID,
	ownerID contracts.OwnerID,
	name string,
	effectiveAt time.Time,
	keyID string,
	privateKey ed25519.PrivateKey,
) (Seed, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return Seed{}, fmt.Errorf("seed owner key is invalid")
	}
	publicKey := privateKey.Public()
	runtimePublicKey, ok := publicKey.(ed25519.PublicKey)
	if !ok {
		return Seed{}, fmt.Errorf("seed owner key is invalid")
	}
	seed, err := BuildSeedDraft(
		organizationID, ownerID, name, effectiveAt, keyID,
		keyID, runtimePublicKey,
	)
	if err != nil {
		return Seed{}, err
	}
	for departmentIndex := range seed.Organization.Departments {
		for seatIndex := range seed.Organization.Departments[departmentIndex].Seats {
			seat := &seed.Organization.Departments[departmentIndex].Seats[seatIndex]
			if err := SignSeat(seat, keyID, privateKey); err != nil {
				return Seed{}, fmt.Errorf("seed seat %s: %w", seat.ID, err)
			}
		}
	}
	for index := range seed.Mandates {
		if err := SignMandate(&seed.Mandates[index], keyID, privateKey); err != nil {
			return Seed{}, fmt.Errorf("seed mandate %s: %w", seed.Mandates[index].ID, err)
		}
	}
	if err := SignRuntimeAuthority(
		&seed.RuntimeAuthority, keyID, privateKey,
	); err != nil {
		return Seed{}, fmt.Errorf("seed runtime authority: %w", err)
	}
	for index := range seed.Policies {
		if err := SignPolicy(&seed.Policies[index], keyID, privateKey); err != nil {
			return Seed{}, fmt.Errorf("seed policy %s: %w", seed.Policies[index].ID, err)
		}
	}
	if err := SignOrganization(&seed.Organization, keyID, privateKey); err != nil {
		return Seed{}, fmt.Errorf("seed organization: %w", err)
	}
	if err := seed.Validate(); err != nil {
		return Seed{}, err
	}
	return seed, nil
}

// BuildSeedDraft returns the canonical seven-department topology with
// non-authorizing zero signatures. The owner signs every seat and mandate,
// then the containing organization, before PublishSeed accepts the draft.
func BuildSeedDraft(
	organizationID contracts.OrganizationID,
	ownerID contracts.OwnerID,
	name string,
	effectiveAt time.Time,
	keyID string,
	runtimeKeyID string,
	runtimePublicKey ed25519.PublicKey,
) (Seed, error) {
	placeholder := contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value: base64.RawURLEncoding.EncodeToString(
			make([]byte, ed25519.SignatureSize),
		),
	}
	organization := contracts.Organization{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            organizationID, OwnerID: ownerID, Version: 1,
		Name: name, EffectiveAt: effectiveAt,
		Signature:   placeholder,
		Departments: make([]contracts.Department, 0, len(contracts.AllDepartmentKinds())),
	}
	if strings.TrimSpace(runtimeKeyID) == "" ||
		len(runtimePublicKey) != ed25519.PublicKeySize {
		return Seed{}, fmt.Errorf("seed runtime signing authority is invalid")
	}
	mandates := make([]contracts.Mandate, 0, 21)
	for _, kind := range contracts.AllDepartmentKinds() {
		departmentID := contracts.DepartmentID("department-" + string(kind))
		department := contracts.Department{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            departmentID, OrganizationID: organizationID,
			Kind: kind, Enabled: true,
			Seats: make([]contracts.Seat, 0, len(contracts.AllSeatRoles())),
		}
		for _, role := range contracts.AllSeatRoles() {
			prefix := string(kind) + "-" + string(role)
			mandate := seedMandate(
				organizationID,
				kind,
				role,
				contracts.MandateID("mandate-"+prefix),
				effectiveAt,
			)
			mandate.Signature = placeholder
			seat := contracts.Seat{
				SchemaVersion: contracts.SchemaVersionV1,
				ID:            contracts.SeatID("seat-" + prefix), Version: 1,
				DID:            contracts.SeatDID("did:matrix:workforce:" + prefix),
				OrganizationID: organizationID, DepartmentID: departmentID,
				Role: role, MandateID: mandate.ID, MandateVersion: mandate.Version,
				BindingID:      contracts.SeatBindingID("binding-" + prefix),
				BindingVersion: 1, EffectiveAt: effectiveAt,
				Signature: placeholder,
			}
			department.Seats = append(department.Seats, seat)
			mandates = append(mandates, mandate)
		}
		organization.Departments = append(organization.Departments, department)
	}
	if err := organization.Validate(); err != nil {
		return Seed{}, fmt.Errorf("seed organization draft: %w", err)
	}
	for index := range mandates {
		if err := mandates[index].Validate(); err != nil {
			return Seed{}, fmt.Errorf("seed mandate draft %d: %w", index, err)
		}
	}
	runtimeAuthority := RuntimeAuthority{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             RuntimeAuthorityID(runtimeKeyID),
		Version:        1,
		OrganizationID: organizationID,
		KeyID:          runtimeKeyID,
		PublicKey: base64.RawURLEncoding.EncodeToString(
			append([]byte(nil), runtimePublicKey...),
		),
		Purposes:    []string{WakeLeaseSigningPurpose},
		EffectiveAt: effectiveAt,
		Signature:   placeholder,
	}
	baseline := contracts.Policy{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             contracts.PolicyID("policy:workforce:baseline"),
		Version:        1,
		OrganizationID: organizationID,
		Kind:           "workforce-baseline",
		EffectiveAt:    effectiveAt,
		Rules: []contracts.PolicyRule{
			{
				ClauseID: "deny-seat-credentials",
				Outcome:  "deny",
				Scope:    "seat access to owner, runtime, provider, or effect credentials",
			},
			{
				ClauseID: "review-irreversible-effects",
				Outcome:  "require_review",
				Scope:    "irreversible external effects",
			},
			{
				ClauseID: "escalate-uncertain-authority",
				Outcome:  "escalate",
				Scope:    "uncertain authority, evidence, safety, policy, or effect state",
			},
		},
		Signature: placeholder,
	}
	return Seed{
		Organization: organization, Mandates: mandates,
		RuntimeAuthority: runtimeAuthority,
		Policies:         []contracts.Policy{baseline},
	}, nil
}

func seedMandate(
	organizationID contracts.OrganizationID,
	kind contracts.DepartmentKind,
	role contracts.SeatRole,
	id contracts.MandateID,
	effectiveAt time.Time,
) contracts.Mandate {
	prohibition := "cannot-self-approve"
	description := "The seat cannot approve or attest its own work."
	if kind == contracts.DepartmentExecutive {
		prohibition = "no-approval-or-control-plane-authority"
		description = "Executive seats cannot approve, sign, use effect credentials, or acquire control-plane authority."
	}
	purpose := strings.ReplaceAll(departmentNames[kind], " ", "-")
	return contracts.Mandate{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            id, Version: 1, OrganizationID: organizationID,
		DepartmentKind: kind, SeatRole: role,
		AllowedSkills: append([]contracts.SkillID(nil), departmentSkills[kind]...),
		DataScopes: []contracts.DataScope{{
			Name:           "department-work",
			Classification: contracts.ClassificationDepartment,
			Purpose:        strings.ToLower(purpose) + "-mandate-work",
		}},
		EscalationRules: []contracts.EscalationRule{{
			Condition: "authority, evidence, safety, or policy is uncertain",
			Action:    "stop and escalate to the human owner",
		}},
		Prohibitions: []contracts.Prohibition{{
			ClauseID: prohibition, Description: description,
		}},
		EffectiveAt: effectiveAt,
	}
}
