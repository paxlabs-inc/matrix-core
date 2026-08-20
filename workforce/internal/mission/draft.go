package mission

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"slices"
	"time"

	"centra/workforce/internal/contracts"
)

// ActivationDraft contains founder-entered values before exact local signing.
type ActivationDraft struct {
	Purpose                   string           `json:"purpose"`
	PermittedBusinessDomains  []string         `json:"permitted_business_domains"`
	StrategicPrinciples       []string         `json:"strategic_principles"`
	TargetOutcomes            []string         `json:"target_outcomes"`
	SuccessConditions         []string         `json:"success_conditions"`
	FailureConditions         []string         `json:"failure_conditions"`
	LegalProhibitions         []string         `json:"legal_prohibitions"`
	EthicalProhibitions       []string         `json:"ethical_prohibitions"`
	PermittedJurisdictions    []string         `json:"permitted_jurisdictions"`
	DataBoundaries            []string         `json:"data_boundaries"`
	PermittedCounterparties   []string         `json:"permitted_counterparties"`
	OperatingScopes           []OperatingScope `json:"operating_scopes"`
	RiskTolerance             RiskTolerance    `json:"risk_tolerance"`
	Autonomy                  AutonomyLevel    `json:"autonomy"`
	EscalationConditions      []string         `json:"escalation_conditions"`
	PauseConditions           []string         `json:"pause_conditions"`
	ShutdownConditions        []string         `json:"shutdown_conditions"`
	Currency                  string           `json:"currency"`
	StartingMicrounits        uint64           `json:"starting_microunits"`
	SpendCeilingMicrounits    uint64           `json:"spend_ceiling_microunits"`
	ExposureCeilingMicrounits uint64           `json:"exposure_ceiling_microunits"`
	MinimumRunwayDays         uint32           `json:"minimum_runway_days"`
	MaxWorkOrderMicrounits    uint64           `json:"max_work_order_microunits"`
}

// BuildActivationDraft produces the exact closed authority bundle the browser
// previews and signs; all caller-provided sets are normalized deterministically.
func BuildActivationDraft(
	organizationID contracts.OrganizationID,
	ownerID contracts.OwnerID,
	effectiveAt time.Time,
	founderKeyID string,
	issuerKeyID string,
	issuerPublicKey ed25519.PublicKey,
	draft ActivationDraft,
) (ActivationAuthority, error) {
	return BuildAuthorityDraft(
		organizationID, ownerID, effectiveAt, founderKeyID,
		issuerKeyID, issuerPublicKey, 1, draft,
	)
}

// BuildAuthorityDraft produces one exact later authority version for a
// founder-reviewed material change.
func BuildAuthorityDraft(
	organizationID contracts.OrganizationID,
	ownerID contracts.OwnerID,
	effectiveAt time.Time,
	founderKeyID string,
	issuerKeyID string,
	issuerPublicKey ed25519.PublicKey,
	version uint64,
	draft ActivationDraft,
) (ActivationAuthority, error) {
	if version == 0 {
		return ActivationAuthority{}, fmt.Errorf("mission: authority version must be positive")
	}
	sortDraft(&draft)
	placeholder := signaturePlaceholder(founderKeyID)
	reserved := defaultReservedDecisions()
	value := ActivationAuthority{
		Mission: FounderMission{
			SchemaVersion: MissionSchemaVersion,
			ID:            "mission:" + string(organizationID), Version: version,
			OrganizationID: organizationID, OwnerID: ownerID,
			Purpose:                  draft.Purpose,
			PermittedBusinessDomains: draft.PermittedBusinessDomains,
			StrategicPrinciples:      draft.StrategicPrinciples,
			TargetOutcomes:           draft.TargetOutcomes,
			SuccessConditions:        draft.SuccessConditions,
			FailureConditions:        draft.FailureConditions,
			EffectiveAt:              effectiveAt, Signature: placeholder,
		},
		Constitution: CompanyConstitution{
			SchemaVersion: ConstitutionSchemaVersion,
			ID:            "constitution:" + string(organizationID), Version: version,
			OrganizationID: organizationID, OwnerID: ownerID,
			LegalProhibitions:       draft.LegalProhibitions,
			EthicalProhibitions:     draft.EthicalProhibitions,
			PermittedJurisdictions:  draft.PermittedJurisdictions,
			DataBoundaries:          draft.DataBoundaries,
			PermittedCounterparties: draft.PermittedCounterparties,
			OperatingScopes:         draft.OperatingScopes,
			RiskTolerance:           draft.RiskTolerance, Autonomy: draft.Autonomy,
			ReservedDecisions:    reserved,
			EscalationConditions: draft.EscalationConditions,
			PauseConditions:      draft.PauseConditions,
			ShutdownConditions:   draft.ShutdownConditions,
			EffectiveAt:          effectiveAt, Signature: placeholder,
		},
		Capital: CapitalEnvelope{
			SchemaVersion: CapitalSchemaVersion,
			ID:            "capital:" + string(organizationID), Version: version,
			OrganizationID: organizationID, Currency: draft.Currency,
			StartingMicrounits:        draft.StartingMicrounits,
			SpendCeilingMicrounits:    draft.SpendCeilingMicrounits,
			ExposureCeilingMicrounits: draft.ExposureCeilingMicrounits,
			MinimumRunwayDays:         draft.MinimumRunwayDays,
			EffectiveAt:               effectiveAt, Signature: placeholder,
		},
		IssuerPolicy: CompanyIssuerPolicy{
			SchemaVersion: IssuerPolicySchemaVersion,
			ID:            "company-issuer-policy:" + string(organizationID), Version: version,
			OrganizationID:  organizationID,
			IssuerKeyID:     issuerKeyID,
			IssuerPublicKey: base64.RawURLEncoding.EncodeToString(issuerPublicKey),
			MissionVersion:  version, ConstitutionVersion: version, CapitalEnvelopeVersion: version,
			AllowedWorkOrderClasses: []string{"initiative"},
			MaxWorkOrderMicrounits:  draft.MaxWorkOrderMicrounits,
			EffectiveAt:             effectiveAt, ExpiresAt: effectiveAt.Add(365 * 24 * time.Hour),
			Signature: placeholder,
		},
		Organization: OrganizationV2{
			SchemaVersion: OrganizationV2SchemaVersion,
			ID:            "organization-v2:" + string(organizationID), Version: version,
			OrganizationID: organizationID, OwnerID: ownerID,
			TemplateID: "organization-template:default-v1", TemplateVersion: 1,
			MissionVersion: version, ConstitutionVersion: version,
			CapitalEnvelopeVersion: version, IssuerPolicyVersion: version,
			EffectiveAt: effectiveAt, Signature: placeholder,
		},
	}
	if len(issuerPublicKey) != ed25519.PublicKeySize {
		return ActivationAuthority{}, fmt.Errorf("mission: company issuer public key is invalid")
	}
	if err := value.Validate(); err != nil {
		return ActivationAuthority{}, err
	}
	return value, nil
}

func sortDraft(value *ActivationDraft) {
	sets := []*[]string{
		&value.PermittedBusinessDomains, &value.StrategicPrinciples,
		&value.TargetOutcomes, &value.SuccessConditions, &value.FailureConditions,
		&value.LegalProhibitions, &value.EthicalProhibitions,
		&value.PermittedJurisdictions, &value.DataBoundaries,
		&value.PermittedCounterparties, &value.EscalationConditions,
		&value.PauseConditions, &value.ShutdownConditions,
	}
	for _, set := range sets {
		slices.Sort(*set)
	}
	for index := range value.OperatingScopes {
		slices.Sort(value.OperatingScopes[index].AllowedActions)
		slices.Sort(value.OperatingScopes[index].DataClassifications)
		slices.Sort(value.OperatingScopes[index].Jurisdictions)
	}
	slices.SortFunc(value.OperatingScopes, func(left, right OperatingScope) int {
		if result := stringsCompare(string(left.Kind), string(right.Kind)); result != 0 {
			return result
		}
		return stringsCompare(left.ScopeID, right.ScopeID)
	})
}

func defaultReservedDecisions() []ReservedDecision {
	values := []ReservedDecision{
		{ClauseID: "reserved.aggregate-capital", Kind: ReservedCapitalIncrease, Description: "Increase aggregate company capital", Escalation: "Require an exact founder signature"},
		{ClauseID: "reserved.constitution", Kind: ReservedConstitutionChange, Description: "Change the Company Constitution", Escalation: "Require an exact founder signature"},
		{ClauseID: "reserved.controls", Kind: ReservedControlRelaxation, Description: "Relax legal, security, privacy, or financial controls", Escalation: "Require an exact founder signature and independent review"},
		{ClauseID: "reserved.corporate-action", Kind: ReservedCorporateAction, Description: "Perform an irreversible corporate action", Escalation: "Require an exact founder signature and legal review"},
		{ClauseID: "reserved.custody", Kind: ReservedCustodyOrWithdrawal, Description: "Acquire custody or withdrawal authority", Escalation: "Require an exact founder signature and financial review"},
		{ClauseID: "reserved.debt", Kind: ReservedDebtOrLeverage, Description: "Create debt or leverage", Escalation: "Require an exact founder signature and financial review"},
		{ClauseID: "reserved.jurisdiction", Kind: ReservedRestrictedRegion, Description: "Enter a restricted jurisdiction", Escalation: "Require an exact founder signature and legal review"},
		{ClauseID: "reserved.mission", Kind: ReservedMissionChange, Description: "Change the Founder Mission", Escalation: "Require an exact founder signature"},
		{ClauseID: "reserved.transfer", Kind: ReservedMaterialTransfer, Description: "Make a material or unrestricted transfer", Escalation: "Require an exact founder signature and financial review"},
	}
	slices.SortFunc(values, func(left, right ReservedDecision) int {
		return stringsCompare(string(left.Kind), string(right.Kind))
	})
	return values
}

func stringsCompare(left, right string) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}
