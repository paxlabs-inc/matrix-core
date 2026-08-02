package executive

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"slices"
	"time"

	"matrix/workforce/internal/companylifecycle"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/mission"
	"matrix/workforce/internal/policy"
)

// CompileInput contains the already owner-controlled roots and the exact seat
// records from which executable Executive authority is derived.
type CompileInput struct {
	Authority           mission.ActivationAuthority
	Delegation          DelegationPolicy
	Seats               []contracts.Seat
	Mandates            []contracts.Mandate
	FounderKeyID        string
	FounderPublicKey    ed25519.PublicKey
	ControllerPublicKey ed25519.PublicKey
	At                  time.Time
}

// Compile verifies every founder signature and deterministically compiles
// bounded Executive clauses. The result contains no new authority and cannot
// outlive any signed source record.
func Compile(input CompileInput) (CompiledAuthority, error) {
	if validateToken("founder key id", input.FounderKeyID) != nil ||
		len(input.FounderPublicKey) != ed25519.PublicKeySize ||
		len(input.ControllerPublicKey) != ed25519.PublicKeySize || !validUTC(input.At) {
		return CompiledAuthority{}, fmt.Errorf("executive: compile authority or time is invalid")
	}
	if err := mission.VerifyActivationAuthority(
		input.Authority, input.FounderKeyID, input.FounderPublicKey,
	); err != nil {
		return CompiledAuthority{}, fmt.Errorf("executive: activation authority: %w", err)
	}
	if err := mission.VerifyCompanyIssuerPolicy(
		input.Authority.IssuerPolicy, input.FounderKeyID, input.FounderPublicKey, input.At,
	); err != nil {
		return CompiledAuthority{}, fmt.Errorf("executive: company issuer authority: %w", err)
	}
	issuerKey, err := base64.RawURLEncoding.DecodeString(input.Authority.IssuerPolicy.IssuerPublicKey)
	if err != nil || !bytes.Equal(issuerKey, input.ControllerPublicKey) {
		return CompiledAuthority{}, fmt.Errorf("executive: controller key does not match current company issuer authority")
	}
	if err := VerifyDelegationPolicy(input.Delegation, input.FounderKeyID, input.FounderPublicKey); err != nil {
		return CompiledAuthority{}, fmt.Errorf("executive: delegation policy: %w", err)
	}
	if err := validateRootBindings(input); err != nil {
		return CompiledAuthority{}, err
	}
	if input.Authority.Constitution.Autonomy != mission.AutonomyBoundedAuto {
		return CompiledAuthority{}, fmt.Errorf("executive: Constitution does not grant bounded autonomous decisions")
	}
	if input.At.Before(input.Delegation.EffectiveAt) || !input.At.Before(input.Delegation.ExpiresAt) ||
		input.Delegation.EffectiveAt.Before(input.Authority.Mission.EffectiveAt) ||
		input.Delegation.ExpiresAt.After(input.Authority.IssuerPolicy.ExpiresAt) {
		return CompiledAuthority{}, fmt.Errorf("executive: delegation policy is not current inside its source authority")
	}
	if err := validateCompiledLimits(input.Authority, input.Delegation); err != nil {
		return CompiledAuthority{}, err
	}
	if err := verifyPolicySeats(input); err != nil {
		return CompiledAuthority{}, err
	}

	policyHash, err := contracts.HashCanonical(&input.Delegation)
	if err != nil {
		return CompiledAuthority{}, fmt.Errorf("executive: hash delegation policy: %w", err)
	}
	missionHash, err := contracts.HashCanonical(&input.Authority.Mission)
	if err != nil {
		return CompiledAuthority{}, fmt.Errorf("executive: hash Mission: %w", err)
	}
	constitutionHash, err := contracts.HashCanonical(&input.Authority.Constitution)
	if err != nil {
		return CompiledAuthority{}, fmt.Errorf("executive: hash Constitution: %w", err)
	}
	capitalHash, err := contracts.HashCanonical(&input.Authority.Capital)
	if err != nil {
		return CompiledAuthority{}, fmt.Errorf("executive: hash capital envelope: %w", err)
	}
	issuerHash, err := contracts.HashCanonical(&input.Authority.IssuerPolicy)
	if err != nil {
		return CompiledAuthority{}, fmt.Errorf("executive: hash issuer policy: %w", err)
	}

	compiled := CompiledAuthority{
		SchemaVersion:                CompiledAuthoritySchemaVersion,
		ID:                           compiledAuthorityID(input.Delegation.OrganizationID, input.Delegation.Version),
		OrganizationID:               input.Delegation.OrganizationID,
		PolicyID:                     input.Delegation.ID,
		PolicyVersion:                input.Delegation.Version,
		PolicyHash:                   policyHash,
		MissionVersion:               input.Authority.Mission.Version,
		MissionHash:                  missionHash,
		ConstitutionVersion:          input.Authority.Constitution.Version,
		ConstitutionHash:             constitutionHash,
		CapitalEnvelopeVersion:       input.Authority.Capital.Version,
		CapitalEnvelopeHash:          capitalHash,
		IssuerPolicyVersion:          input.Authority.IssuerPolicy.Version,
		IssuerPolicyHash:             issuerHash,
		DecisionMakers:               cloneSeatBindings(input.Delegation.DecisionMakers),
		Reviewers:                    cloneSeatBindings(input.Delegation.Reviewers),
		Clauses:                      cloneClauses(input.Delegation.Clauses),
		MaxRollingCapitalMicrounits:  input.Delegation.MaxRollingCapitalMicrounits,
		MaxRollingExposureMicrounits: input.Delegation.MaxRollingExposureMicrounits,
		AggregationWindowSeconds:     input.Delegation.AggregationWindowSeconds,
		EffectiveAt:                  input.Delegation.EffectiveAt,
		ExpiresAt:                    input.Delegation.ExpiresAt,
		CompiledAt:                   input.At,
	}
	if err := compiled.Validate(); err != nil {
		return CompiledAuthority{}, err
	}
	return compiled, nil
}

func validateRootBindings(input CompileInput) error {
	authority := input.Authority
	delegation := input.Delegation
	if delegation.OrganizationID != authority.Mission.OrganizationID ||
		delegation.MissionVersion != authority.Mission.Version ||
		delegation.ConstitutionVersion != authority.Constitution.Version ||
		delegation.CapitalEnvelopeVersion != authority.Capital.Version ||
		delegation.IssuerPolicyVersion != authority.IssuerPolicy.Version {
		return fmt.Errorf("executive: delegation policy does not bind the current company authority")
	}
	return nil
}

func validateCompiledLimits(authority mission.ActivationAuthority, delegation DelegationPolicy) error {
	capital := authority.Capital
	if delegation.MaxRollingCapitalMicrounits > capital.SpendCeilingMicrounits ||
		delegation.MaxRollingExposureMicrounits > capital.ExposureCeilingMicrounits {
		return fmt.Errorf("executive: policy aggregate limits exceed the founder capital envelope")
	}
	jurisdictions := makeSet(authority.Constitution.PermittedJurisdictions)
	counterparties := makeSet(authority.Constitution.PermittedCounterparties)
	for index := range delegation.Clauses {
		clause := delegation.Clauses[index]
		if !statesPermittedForAction(clause.Action, clause.AllowedLifecycleStates) {
			return fmt.Errorf("executive: clause %q permits an incompatible lifecycle state", clause.ClauseID)
		}
		if clause.MaxRequestCapitalMicrounits > authority.IssuerPolicy.MaxWorkOrderMicrounits ||
			clause.MaxRollingCapitalMicrounits > delegation.MaxRollingCapitalMicrounits ||
			clause.MaxRollingExposureMicrounits > delegation.MaxRollingExposureMicrounits {
			return fmt.Errorf("executive: clause %q exceeds company issuer or aggregate authority", clause.ClauseID)
		}
		for _, jurisdiction := range clause.PermittedJurisdictions {
			if !jurisdictions[jurisdiction] {
				return fmt.Errorf("executive: clause %q includes a non-permitted jurisdiction", clause.ClauseID)
			}
		}
		for _, counterparty := range clause.PermittedCounterparties {
			if !counterparties[counterparty] {
				return fmt.Errorf("executive: clause %q includes a non-permitted counterparty", clause.ClauseID)
			}
		}
	}
	return nil
}

func verifyPolicySeats(input CompileInput) error {
	seats := make(map[contracts.SeatID]contracts.Seat, len(input.Seats))
	for index := range input.Seats {
		seat := input.Seats[index]
		if err := seat.Validate(); err != nil {
			return fmt.Errorf("executive: seat %d: %w", index, err)
		}
		if seat.OrganizationID != input.Delegation.OrganizationID ||
			seat.EffectiveAt.After(input.At) {
			return fmt.Errorf("executive: seat %q is not current for this organization", seat.ID)
		}
		if err := policy.VerifySeatAuthority(seat, input.FounderKeyID, input.FounderPublicKey); err != nil {
			return fmt.Errorf("executive: seat %q authority: %w", seat.ID, err)
		}
		if _, duplicate := seats[seat.ID]; duplicate {
			return fmt.Errorf("executive: duplicate seat %q", seat.ID)
		}
		seats[seat.ID] = seat
	}
	mandates := make(map[contracts.MandateID]contracts.Mandate, len(input.Mandates))
	for index := range input.Mandates {
		mandate := input.Mandates[index]
		if err := mandate.Validate(); err != nil {
			return fmt.Errorf("executive: mandate %d: %w", index, err)
		}
		if mandate.OrganizationID != input.Delegation.OrganizationID ||
			mandate.EffectiveAt.After(input.At) ||
			mandate.ExpiresAt != nil && !input.At.Before(*mandate.ExpiresAt) {
			return fmt.Errorf("executive: mandate %q is not current for this organization", mandate.ID)
		}
		if err := policy.VerifyMandateAuthority(mandate, input.FounderKeyID, input.FounderPublicKey); err != nil {
			return fmt.Errorf("executive: mandate %q authority: %w", mandate.ID, err)
		}
		if _, duplicate := mandates[mandate.ID]; duplicate {
			return fmt.Errorf("executive: duplicate mandate %q", mandate.ID)
		}
		mandates[mandate.ID] = mandate
	}
	bindings := append(cloneSeatBindings(input.Delegation.DecisionMakers), input.Delegation.Reviewers...)
	seenSeats := make(map[contracts.SeatID]struct{}, len(bindings))
	seenSigningKeys := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if _, duplicate := seenSeats[binding.SeatID]; duplicate {
			return fmt.Errorf("executive: seat %q appears in more than one authority role", binding.SeatID)
		}
		seenSeats[binding.SeatID] = struct{}{}
		bindingPublicKey, err := decodePublicKey(binding.SigningPublicKey)
		if err != nil || bytes.Equal(bindingPublicKey, input.FounderPublicKey) ||
			bytes.Equal(bindingPublicKey, input.ControllerPublicKey) {
			return fmt.Errorf("executive: seat %q signing key collides with root or controller authority", binding.SeatID)
		}
		if _, duplicate := seenSigningKeys[binding.SigningPublicKey]; duplicate {
			return fmt.Errorf("executive: policy reuses one signing key across independent seats")
		}
		seenSigningKeys[binding.SigningPublicKey] = struct{}{}
		seat, exists := seats[binding.SeatID]
		if !exists {
			return fmt.Errorf("executive: policy seat %q is absent", binding.SeatID)
		}
		mandate, exists := mandates[binding.MandateID]
		if !exists {
			return fmt.Errorf("executive: policy mandate %q is absent", binding.MandateID)
		}
		seatHash, err := contracts.HashCanonical(&seat)
		if err != nil {
			return err
		}
		mandateHash, err := contracts.HashCanonical(&mandate)
		if err != nil {
			return err
		}
		if binding.SeatVersion != seat.Version || binding.SeatHash != seatHash ||
			binding.DepartmentKind != mandate.DepartmentKind || binding.Role != seat.Role ||
			binding.Role != mandate.SeatRole || binding.MandateID != seat.MandateID ||
			binding.MandateVersion != seat.MandateVersion ||
			binding.MandateVersion != mandate.Version || binding.MandateHash != mandateHash {
			return fmt.Errorf("executive: seat %q policy binding is inconsistent", binding.SeatID)
		}
	}
	return nil
}

func statesPermittedForAction(action Action, states []companylifecycle.State) bool {
	allowed := make(map[companylifecycle.State]bool)
	add := func(values ...companylifecycle.State) {
		for _, value := range values {
			allowed[value] = true
		}
	}
	switch action {
	case ActionRejectOpportunity:
		add(companylifecycle.StateDiscover, companylifecycle.StateScreen,
			companylifecycle.StateValidate, companylifecycle.StateDecide)
	case ActionAuthorizeExperiment:
		add(companylifecycle.StateScreen, companylifecycle.StateValidate)
	case ActionPrioritizeInitiative:
		add(companylifecycle.StateDiscover, companylifecycle.StateScreen,
			companylifecycle.StateValidate, companylifecycle.StateDecide,
			companylifecycle.StateFund, companylifecycle.StateDesign,
			companylifecycle.StateBuild, companylifecycle.StateVerify)
	case ActionAllocateDelegatedCapital:
		add(companylifecycle.StateDecide, companylifecycle.StateFund)
	case ActionSelectProduct:
		add(companylifecycle.StateFund, companylifecycle.StateDesign)
	case ActionAuthorizePricingTest:
		add(companylifecycle.StateValidate, companylifecycle.StateLaunch,
			companylifecycle.StateMonetize, companylifecycle.StateOperate,
			companylifecycle.StateMeasure)
	case ActionSequenceLaunch:
		add(companylifecycle.StateVerify, companylifecycle.StateLaunch)
	case ActionReallocateResources:
		add(companylifecycle.StateFund, companylifecycle.StateDesign,
			companylifecycle.StateBuild, companylifecycle.StateVerify,
			companylifecycle.StateLaunch, companylifecycle.StateAcquire,
			companylifecycle.StateMonetize, companylifecycle.StateOperate,
			companylifecycle.StateMeasure)
	case ActionScale, ActionPivot, ActionMaintain:
		add(companylifecycle.StateMeasure)
	case ActionPause, ActionEmergencyPause:
		for _, state := range companylifecycle.AllStates() {
			if state != companylifecycle.StateTerminate {
				allowed[state] = true
			}
		}
	case ActionTerminate:
		add(companylifecycle.StateDecide, companylifecycle.StateMeasure,
			companylifecycle.StateScale, companylifecycle.StatePivot,
			companylifecycle.StateMaintain, companylifecycle.StatePaused)
	default:
		return false
	}
	for _, state := range states {
		if !allowed[state] {
			return false
		}
	}
	return true
}

func cloneSeatBindings(values []SeatAuthorityBinding) []SeatAuthorityBinding {
	result := make([]SeatAuthorityBinding, len(values))
	for index := range values {
		result[index] = values[index]
		result[index].ReviewKinds = slices.Clone(values[index].ReviewKinds)
	}
	return result
}

func cloneClauses(values []DecisionClause) []DecisionClause {
	result := make([]DecisionClause, len(values))
	for index := range values {
		result[index] = values[index]
		result[index].AllowedLifecycleStates = slices.Clone(values[index].AllowedLifecycleStates)
		result[index].PermittedJurisdictions = slices.Clone(values[index].PermittedJurisdictions)
		result[index].PermittedCounterparties = slices.Clone(values[index].PermittedCounterparties)
		result[index].RequiredReviews = slices.Clone(values[index].RequiredReviews)
	}
	return result
}

func makeSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}
