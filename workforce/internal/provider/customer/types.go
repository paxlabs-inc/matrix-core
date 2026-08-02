package customer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/provider/external"
	"matrix/workforce/internal/skills"
)

const SchemaVersion = contracts.SchemaVersionV1

var (
	ErrDenied          = errors.New("customer adapter: operation denied")
	ErrConflict        = errors.New("customer adapter: durable identity conflict")
	ErrAmbiguous       = errors.New("customer adapter: external outcome is ambiguous")
	ErrUnavailable     = errors.New("customer adapter: provider unavailable")
	ErrIntegrity       = errors.New("customer adapter: sealed record integrity failure")
	ErrCapacity        = errors.New("customer adapter: scoped capacity exhausted")
	ErrCircuitOpen     = errors.New("customer adapter: operation circuit is open")
	ErrConsent         = errors.New("customer adapter: current consent is required")
	ErrUnsubscribed    = errors.New("customer adapter: recipient has unsubscribed")
	ErrFrequencyLimit  = errors.New("customer adapter: communication frequency limit reached")
	ErrPromptInjection = errors.New("customer adapter: untrusted payload attempted authority injection")
)

type Family string

const (
	FamilyEmail                Family = "email"
	FamilyConsentedOutbound    Family = "consented_outbound"
	FamilySocialDistribution   Family = "social_distribution"
	FamilyCRM                  Family = "crm"
	FamilySalesPipeline        Family = "sales_pipeline"
	FamilyContractTransmission Family = "contract_transmission"
	FamilyCustomerOnboarding   Family = "customer_onboarding"
	FamilySupport              Family = "customer_support"
	FamilyCustomerObservation  Family = "customer_observation"
)

func (value Family) Valid() bool {
	switch value {
	case FamilyEmail, FamilyConsentedOutbound, FamilySocialDistribution,
		FamilyCRM, FamilySalesPipeline, FamilyContractTransmission,
		FamilyCustomerOnboarding, FamilySupport, FamilyCustomerObservation:
		return true
	default:
		return false
	}
}

func (value Family) customerFacing() bool {
	switch value {
	case FamilyEmail, FamilyConsentedOutbound, FamilySocialDistribution,
		FamilyContractTransmission, FamilyCustomerOnboarding, FamilySupport:
		return true
	default:
		return false
	}
}

type Action string

const (
	ActionSend       Action = "send"
	ActionPublish    Action = "publish"
	ActionUpsert     Action = "upsert"
	ActionAdvance    Action = "advance"
	ActionTransmit   Action = "transmit"
	ActionEnroll     Action = "enroll"
	ActionRespond    Action = "respond"
	ActionObserve    Action = "observe"
	ActionCompensate Action = "compensate"
)

func (value Action) Valid() bool {
	switch value {
	case ActionSend, ActionPublish, ActionUpsert, ActionAdvance,
		ActionTransmit, ActionEnroll, ActionRespond, ActionObserve,
		ActionCompensate:
		return true
	default:
		return false
	}
}

func (value Action) Mutates() bool { return value.Valid() && value != ActionObserve }

type ConsentBasis string

const (
	ConsentExplicitOptIn ConsentBasis = "explicit_opt_in"
	ConsentContractual   ConsentBasis = "contractual"
	ConsentService       ConsentBasis = "service_communication"
	ConsentLegitimate    ConsentBasis = "documented_legitimate_interest"
)

func (value ConsentBasis) Valid() bool {
	return value == ConsentExplicitOptIn || value == ConsentContractual ||
		value == ConsentService || value == ConsentLegitimate
}

type CustomerState string

const (
	CustomerActive  CustomerState = "active"
	CustomerBlocked CustomerState = "blocked"
	CustomerDeleted CustomerState = "deleted"
)

func (value CustomerState) Valid() bool {
	return value == CustomerActive || value == CustomerBlocked || value == CustomerDeleted
}

type ConsentState string

const (
	ConsentGranted   ConsentState = "granted"
	ConsentWithdrawn ConsentState = "withdrawn"
)

func (value ConsentState) Valid() bool {
	return value == ConsentGranted || value == ConsentWithdrawn
}

type AuthorityBinding struct {
	MissionVersion       uint64                `json:"mission_version"`
	ConstitutionVersion  uint64                `json:"constitution_version"`
	OrganizationVersion  uint64                `json:"organization_version"`
	OperatingScopeID     string                `json:"operating_scope_id"`
	OperatingScopeHash   contracts.ContentHash `json:"operating_scope_hash"`
	PolicyRefs           []contracts.PolicyRef `json:"policy_refs"`
	CustomerIssuerKeyIDs []string              `json:"customer_issuer_key_ids"`
	ConsentIssuerKeyIDs  []string              `json:"consent_issuer_key_ids"`
}

func (value AuthorityBinding) Validate() error {
	if value.MissionVersion == 0 || value.ConstitutionVersion == 0 ||
		value.OrganizationVersion == 0 || token("operating scope id", value.OperatingScopeID) != nil ||
		value.OperatingScopeHash.Validate() != nil || len(value.PolicyRefs) == 0 ||
		len(value.PolicyRefs) > 64 || !distinctTokens(value.CustomerIssuerKeyIDs, 32) ||
		!distinctTokens(value.ConsentIssuerKeyIDs, 32) ||
		len(value.CustomerIssuerKeyIDs) == 0 || len(value.ConsentIssuerKeyIDs) == 0 {
		return fmt.Errorf("customer adapter: authority binding is incomplete")
	}
	seen := make(map[contracts.PolicyID]bool, len(value.PolicyRefs))
	for _, reference := range value.PolicyRefs {
		if reference.Validate() != nil || seen[reference.ID] {
			return fmt.Errorf("customer adapter: authority policy binding is invalid")
		}
		seen[reference.ID] = true
	}
	return nil
}

type ResourceLimits struct {
	FrequencyWindow       time.Duration `json:"frequency_window"`
	MaxPerRecipientWindow uint16        `json:"max_per_recipient_window"`
	MaxPerRecipientDay    uint16        `json:"max_per_recipient_day"`
	MaxPerConnectionDay   uint32        `json:"max_per_connection_day"`
	MaxConcurrent         uint16        `json:"max_concurrent"`
	MaxAttempts           uint16        `json:"max_attempts"`
	RetryWindow           time.Duration `json:"retry_window"`
	DriftBlindMutations   uint16        `json:"drift_blind_mutations"`
	FailureThreshold      uint16        `json:"failure_threshold"`
	SuccessThreshold      uint16        `json:"success_threshold"`
	CircuitWindow         time.Duration `json:"circuit_window"`
	CircuitOpenDuration   time.Duration `json:"circuit_open_duration"`
	OutputBytes           uint64        `json:"output_bytes"`
}

func (value ResourceLimits) Validate() error {
	if value.FrequencyWindow <= 0 || value.FrequencyWindow > 30*24*time.Hour ||
		value.MaxPerRecipientWindow == 0 || value.MaxPerRecipientWindow > 128 ||
		value.MaxPerRecipientDay == 0 || value.MaxPerRecipientDay > 256 ||
		value.MaxPerConnectionDay == 0 || value.MaxPerConnectionDay > 100000 ||
		value.MaxConcurrent == 0 || value.MaxConcurrent > 32 ||
		value.MaxAttempts == 0 || value.MaxAttempts > 16 ||
		value.RetryWindow <= 0 || value.RetryWindow > 24*time.Hour ||
		value.DriftBlindMutations > 8 || value.FailureThreshold == 0 ||
		value.FailureThreshold > 100 || value.SuccessThreshold == 0 ||
		value.SuccessThreshold > 100 || value.CircuitWindow <= 0 ||
		value.CircuitWindow > 24*time.Hour || value.CircuitOpenDuration <= 0 ||
		value.CircuitOpenDuration > 24*time.Hour || value.OutputBytes == 0 ||
		value.OutputBytes > 1<<20 {
		return fmt.Errorf("customer adapter: resource limits are outside hard ceilings")
	}
	return nil
}

type OperationPolicy struct {
	Name                  string               `json:"name"`
	ExternalOperation     string               `json:"external_operation"`
	Family                Family               `json:"family"`
	Action                Action               `json:"action"`
	ExternalAction        external.ActionClass `json:"external_action"`
	EffectClass           skills.EffectClass   `json:"effect_class"`
	RequiresConsent       bool                 `json:"requires_consent"`
	ConsentBases          []ConsentBasis       `json:"consent_bases"`
	CountsFrequency       bool                 `json:"counts_frequency"`
	AuthoritativeOutcome  bool                 `json:"authoritative_outcome"`
	CompensationOperation string               `json:"compensation_operation"`
	CompensatesOperation  string               `json:"compensates_operation"`
}

func (value OperationPolicy) Validate() error {
	if token("operation", value.Name) != nil || token("external operation", value.ExternalOperation) != nil ||
		!value.Family.Valid() || !value.Action.Valid() || !value.ExternalAction.Valid() ||
		!value.EffectClass.Valid() || value.Action.Mutates() != value.ExternalAction.Mutates() ||
		value.Action.Mutates() == (value.EffectClass == skills.EffectRead) {
		return fmt.Errorf("customer adapter: operation identity or effect class is invalid")
	}
	if value.Action == ActionObserve && value.CountsFrequency {
		return fmt.Errorf("customer adapter: observations cannot consume communication frequency")
	}
	if value.Family.customerFacing() && value.Action.Mutates() && value.Action != ActionCompensate &&
		(!value.RequiresConsent || !value.CountsFrequency) {
		return fmt.Errorf("customer adapter: customer-facing mutations require consent and frequency control")
	}
	if value.RequiresConsent {
		if len(value.ConsentBases) == 0 || len(value.ConsentBases) > 4 {
			return fmt.Errorf("customer adapter: consent bases are incomplete")
		}
		seen := map[ConsentBasis]bool{}
		for _, basis := range value.ConsentBases {
			if !basis.Valid() || seen[basis] {
				return fmt.Errorf("customer adapter: consent bases are invalid")
			}
			seen[basis] = true
		}
	} else if len(value.ConsentBases) != 0 {
		return fmt.Errorf("customer adapter: consent-free operation cannot declare consent bases")
	}
	if value.Action.Mutates() && !value.AuthoritativeOutcome {
		return fmt.Errorf("customer adapter: mutations require authoritative provider outcomes")
	}
	if value.EffectClass == skills.EffectReversible && value.CompensatesOperation == "" &&
		token("compensation operation", value.CompensationOperation) != nil {
		return fmt.Errorf("customer adapter: reversible operation requires compensation")
	}
	if value.EffectClass != skills.EffectReversible && value.CompensationOperation != "" {
		return fmt.Errorf("customer adapter: only reversible operations declare compensation")
	}
	if value.CompensatesOperation != "" && value.Action != ActionCompensate {
		return fmt.Errorf("customer adapter: compensation action is invalid")
	}
	if value.CompensatesOperation != "" && value.CompensationOperation != "" {
		return fmt.Errorf("customer adapter: compensation operation cannot itself declare compensation")
	}
	return nil
}

type GovernancePolicy struct {
	Channels               []string                      `json:"channels"`
	Audiences              []string                      `json:"audiences"`
	Purposes               []string                      `json:"purposes"`
	Jurisdictions          []string                      `json:"jurisdictions"`
	ClaimRefs              []string                      `json:"claim_refs"`
	BrandPolicyRefs        []string                      `json:"brand_policy_refs"`
	PrivacyPolicyRefs      []string                      `json:"privacy_policy_refs"`
	LegalPolicyRefs        []string                      `json:"legal_policy_refs"`
	SecurityPolicyRefs     []string                      `json:"security_policy_refs"`
	CompensationPolicyRefs []string                      `json:"compensation_policy_refs"`
	ContractTemplateRefs   []string                      `json:"contract_template_refs"`
	SupportQueues          []string                      `json:"support_queues"`
	DataClassifications    []external.DataClassification `json:"data_classifications"`
}

func (value GovernancePolicy) Validate() error {
	for _, set := range [][]string{
		value.Channels, value.Audiences, value.Purposes, value.Jurisdictions,
		value.ClaimRefs, value.BrandPolicyRefs, value.PrivacyPolicyRefs,
		value.LegalPolicyRefs, value.SecurityPolicyRefs,
		value.CompensationPolicyRefs,
		value.ContractTemplateRefs, value.SupportQueues,
	} {
		if !distinctValues(set, 128, 1024) {
			return fmt.Errorf("customer adapter: governance policy contains invalid values")
		}
	}
	if len(value.Channels) == 0 || len(value.Purposes) == 0 ||
		len(value.Jurisdictions) == 0 || len(value.PrivacyPolicyRefs) == 0 ||
		len(value.LegalPolicyRefs) == 0 || len(value.SecurityPolicyRefs) == 0 ||
		len(value.DataClassifications) == 0 || len(value.DataClassifications) > 6 {
		return fmt.Errorf("customer adapter: governance policy is incomplete")
	}
	seen := map[external.DataClassification]bool{}
	for _, classification := range value.DataClassifications {
		if !classification.Valid() || seen[classification] {
			return fmt.Errorf("customer adapter: data classifications are invalid")
		}
		seen[classification] = true
	}
	return nil
}

type Connection struct {
	SchemaVersion             string                   `json:"schema_version"`
	ID                        string                   `json:"id"`
	Version                   uint64                   `json:"version"`
	OrganizationID            contracts.OrganizationID `json:"organization_id"`
	AdapterName               string                   `json:"adapter_name"`
	ExternalAdapterName       string                   `json:"external_adapter_name"`
	ExternalConnectionID      string                   `json:"external_connection_id"`
	ExternalConnectionVersion uint64                   `json:"external_connection_version"`
	ExternalConnectionHash    contracts.ContentHash    `json:"external_connection_hash"`
	Family                    Family                   `json:"family"`
	AccountID                 string                   `json:"account_id"`
	IdentityID                string                   `json:"identity_id"`
	Authority                 AuthorityBinding         `json:"authority"`
	Governance                GovernancePolicy         `json:"governance"`
	Limits                    ResourceLimits           `json:"limits"`
	Operations                []OperationPolicy        `json:"operations"`
	EffectiveAt               time.Time                `json:"effective_at"`
	ExpiresAt                 time.Time                `json:"expires_at"`
	Signature                 contracts.Signature      `json:"signature"`
}

func (value Connection) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("connection id", value.ID) != nil ||
		value.Version == 0 || token("organization id", string(value.OrganizationID)) != nil ||
		token("adapter name", value.AdapterName) != nil ||
		token("external adapter name", value.ExternalAdapterName) != nil ||
		token("external connection id", value.ExternalConnectionID) != nil ||
		value.ExternalConnectionVersion == 0 || value.ExternalConnectionHash.Validate() != nil ||
		!value.Family.Valid() || bounded("account id", value.AccountID) != nil ||
		bounded("identity id", value.IdentityID) != nil || value.Authority.Validate() != nil ||
		value.Governance.Validate() != nil || value.Limits.Validate() != nil ||
		len(value.Operations) == 0 || len(value.Operations) > 64 ||
		!validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) ||
		value.ExpiresAt.Sub(value.EffectiveAt) > 366*24*time.Hour ||
		value.Signature.Validate() != nil {
		return fmt.Errorf("customer adapter: connection is invalid")
	}
	seen := make(map[string]OperationPolicy, len(value.Operations))
	for _, operation := range value.Operations {
		if operation.Validate() != nil || operation.Family != value.Family {
			return fmt.Errorf("customer adapter: operation is invalid for connection family")
		}
		if _, exists := seen[operation.Name]; exists {
			return fmt.Errorf("customer adapter: operation is duplicated")
		}
		seen[operation.Name] = operation
	}
	for _, operation := range value.Operations {
		if operation.CompensationOperation != "" {
			compensation, ok := seen[operation.CompensationOperation]
			if !ok || compensation.CompensatesOperation != operation.Name {
				return fmt.Errorf("customer adapter: compensation operation is not reciprocal")
			}
		}
		if operation.Action.Mutates() && !operation.AuthoritativeOutcome &&
			value.Limits.DriftBlindMutations == 0 {
			return fmt.Errorf("customer adapter: drift-blind mutation lacks a ceiling")
		}
		if operation.EffectClass == skills.EffectReversible &&
			len(value.Governance.CompensationPolicyRefs) == 0 {
			return fmt.Errorf("customer adapter: reversible operation lacks compensation policy")
		}
	}
	if value.Family.customerFacing() &&
		(len(value.Governance.Audiences) == 0 || len(value.Governance.BrandPolicyRefs) == 0) {
		return fmt.Errorf("customer adapter: customer-facing governance lacks audience or brand policy")
	}
	if value.Family == FamilyContractTransmission && len(value.Governance.ContractTemplateRefs) == 0 {
		return fmt.Errorf("customer adapter: contract transmission lacks template authority")
	}
	if value.Family == FamilySupport && len(value.Governance.SupportQueues) == 0 {
		return fmt.Errorf("customer adapter: support lacks queue authority")
	}
	return nil
}

func (value Connection) Operation(name string) (OperationPolicy, bool) {
	for _, operation := range value.Operations {
		if operation.Name == name {
			return operation, true
		}
	}
	return OperationPolicy{}, false
}

type ConnectionRevocation struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	ConnectionID   string                   `json:"connection_id"`
	Version        uint64                   `json:"version"`
	ReasonCode     string                   `json:"reason_code"`
	RevokedAt      time.Time                `json:"revoked_at"`
	Signature      contracts.Signature      `json:"signature"`
}

func (value ConnectionRevocation) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("revocation id", value.ID) != nil ||
		token("organization id", string(value.OrganizationID)) != nil ||
		token("connection id", value.ConnectionID) != nil || value.Version == 0 ||
		token("reason code", value.ReasonCode) != nil || !validUTC(value.RevokedAt) ||
		value.Signature.Validate() != nil {
		return fmt.Errorf("customer adapter: connection revocation is invalid")
	}
	return nil
}

type CustomerScope struct {
	SchemaVersion       string                        `json:"schema_version"`
	ID                  string                        `json:"id"`
	Version             uint64                        `json:"version"`
	OrganizationID      contracts.OrganizationID      `json:"organization_id"`
	ConnectionID        string                        `json:"connection_id"`
	ConnectionVersion   uint64                        `json:"connection_version"`
	RecipientRef        string                        `json:"recipient_ref"`
	DestinationHash     contracts.ContentHash         `json:"destination_hash"`
	Channels            []string                      `json:"channels"`
	Audiences           []string                      `json:"audiences"`
	Purposes            []string                      `json:"purposes"`
	Jurisdictions       []string                      `json:"jurisdictions"`
	DataClassifications []external.DataClassification `json:"data_classifications"`
	ContractRefs        []string                      `json:"contract_refs"`
	SupportQueues       []string                      `json:"support_queues"`
	State               CustomerState                 `json:"state"`
	EffectiveAt         time.Time                     `json:"effective_at"`
	ExpiresAt           time.Time                     `json:"expires_at"`
	Signature           contracts.Signature           `json:"signature"`
}

func (value CustomerScope) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("customer id", value.ID) != nil ||
		value.Version == 0 || token("organization id", string(value.OrganizationID)) != nil ||
		token("connection id", value.ConnectionID) != nil || value.ConnectionVersion == 0 ||
		bounded("recipient ref", value.RecipientRef) != nil || value.DestinationHash.Validate() != nil ||
		!distinctValues(value.Channels, 32, 256) || len(value.Channels) == 0 ||
		!distinctValues(value.Audiences, 32, 256) ||
		!distinctValues(value.Purposes, 32, 256) || len(value.Purposes) == 0 ||
		!distinctValues(value.Jurisdictions, 32, 256) || len(value.Jurisdictions) == 0 ||
		!distinctValues(value.ContractRefs, 64, 1024) ||
		!distinctValues(value.SupportQueues, 64, 1024) ||
		len(value.DataClassifications) == 0 || len(value.DataClassifications) > 6 ||
		!value.State.Valid() || !validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) ||
		value.ExpiresAt.Sub(value.EffectiveAt) > 10*366*24*time.Hour ||
		value.Signature.Validate() != nil {
		return fmt.Errorf("customer adapter: customer scope is invalid")
	}
	seen := map[external.DataClassification]bool{}
	for _, classification := range value.DataClassifications {
		if !classification.Valid() || seen[classification] {
			return fmt.Errorf("customer adapter: customer data scope is invalid")
		}
		seen[classification] = true
	}
	return nil
}

type ConsentRecord struct {
	SchemaVersion     string                   `json:"schema_version"`
	ID                string                   `json:"id"`
	Version           uint64                   `json:"version"`
	OrganizationID    contracts.OrganizationID `json:"organization_id"`
	ConnectionID      string                   `json:"connection_id"`
	ConnectionVersion uint64                   `json:"connection_version"`
	CustomerID        string                   `json:"customer_id"`
	CustomerVersion   uint64                   `json:"customer_version"`
	RecipientRef      string                   `json:"recipient_ref"`
	DestinationHash   contracts.ContentHash    `json:"destination_hash"`
	Channel           string                   `json:"channel"`
	Purpose           string                   `json:"purpose"`
	Jurisdiction      string                   `json:"jurisdiction"`
	Basis             ConsentBasis             `json:"basis"`
	State             ConsentState             `json:"state"`
	SourceRef         string                   `json:"source_ref"`
	PrivacyPolicyRef  string                   `json:"privacy_policy_ref"`
	UnsubscribeRef    string                   `json:"unsubscribe_ref"`
	FrequencyWindow   time.Duration            `json:"frequency_window"`
	FrequencyMaximum  uint16                   `json:"frequency_maximum"`
	CapturedAt        time.Time                `json:"captured_at"`
	EffectiveAt       time.Time                `json:"effective_at"`
	ExpiresAt         time.Time                `json:"expires_at"`
	Signature         contracts.Signature      `json:"signature"`
}

func (value ConsentRecord) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("consent id", value.ID) != nil ||
		value.Version == 0 || token("organization id", string(value.OrganizationID)) != nil ||
		token("connection id", value.ConnectionID) != nil || value.ConnectionVersion == 0 ||
		token("customer id", value.CustomerID) != nil || value.CustomerVersion == 0 ||
		bounded("recipient ref", value.RecipientRef) != nil || value.DestinationHash.Validate() != nil ||
		bounded("channel", value.Channel) != nil || bounded("purpose", value.Purpose) != nil ||
		bounded("jurisdiction", value.Jurisdiction) != nil || !value.Basis.Valid() ||
		!value.State.Valid() || bounded("source ref", value.SourceRef) != nil ||
		bounded("privacy policy ref", value.PrivacyPolicyRef) != nil ||
		value.FrequencyWindow <= 0 || value.FrequencyWindow > 30*24*time.Hour ||
		value.FrequencyMaximum == 0 || value.FrequencyMaximum > 128 ||
		!validUTC(value.CapturedAt) || !validUTC(value.EffectiveAt) ||
		!validUTC(value.ExpiresAt) || value.CapturedAt.After(value.EffectiveAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) ||
		value.ExpiresAt.Sub(value.EffectiveAt) > 10*366*24*time.Hour ||
		value.Signature.Validate() != nil {
		return fmt.Errorf("customer adapter: consent record is invalid")
	}
	if value.State == ConsentWithdrawn {
		if bounded("unsubscribe ref", value.UnsubscribeRef) != nil {
			return fmt.Errorf("customer adapter: withdrawal requires an unsubscribe reference")
		}
	} else if value.UnsubscribeRef != "" && bounded("unsubscribe ref", value.UnsubscribeRef) != nil {
		return fmt.Errorf("customer adapter: unsubscribe reference is invalid")
	}
	return nil
}

type Request struct {
	Operation               string                      `json:"operation"`
	Family                  Family                      `json:"family"`
	Action                  Action                      `json:"action"`
	AccountID               string                      `json:"account_id"`
	IdentityID              string                      `json:"identity_id"`
	CustomerID              string                      `json:"customer_id"`
	CustomerVersion         uint64                      `json:"customer_version"`
	CustomerHash            contracts.ContentHash       `json:"customer_hash"`
	RecipientRef            string                      `json:"recipient_ref"`
	Destination             string                      `json:"destination"`
	DestinationHash         contracts.ContentHash       `json:"destination_hash"`
	Channel                 string                      `json:"channel"`
	Audience                string                      `json:"audience"`
	Purpose                 string                      `json:"purpose"`
	Jurisdiction            string                      `json:"jurisdiction"`
	DataClassification      external.DataClassification `json:"data_classification"`
	ConsentID               string                      `json:"consent_id"`
	ConsentVersion          uint64                      `json:"consent_version"`
	ConsentHash             contracts.ContentHash       `json:"consent_hash"`
	ClaimRefs               []string                    `json:"claim_refs"`
	BrandPolicyRef          string                      `json:"brand_policy_ref"`
	PrivacyPolicyRef        string                      `json:"privacy_policy_ref"`
	LegalPolicyRef          string                      `json:"legal_policy_ref"`
	SecurityPolicyRef       string                      `json:"security_policy_ref"`
	CompensationPolicyRef   string                      `json:"compensation_policy_ref"`
	ContractRef             string                      `json:"contract_ref"`
	ContractHash            contracts.ContentHash       `json:"contract_hash"`
	SupportQueue            string                      `json:"support_queue"`
	ExpectedResourceVersion string                      `json:"expected_resource_version"`
	CompensatesKey          string                      `json:"compensates_key"`
	TargetURL               string                      `json:"target_url"`
	Payload                 json.RawMessage             `json:"payload"`
	Upload                  *external.Transfer          `json:"upload"`
	Download                bool                        `json:"download"`
	OutputBytes             uint64                      `json:"output_bytes"`
	IssuedAt                time.Time                   `json:"issued_at"`
	ExpiresAt               time.Time                   `json:"expires_at"`
}

func (value Request) Validate() error {
	if token("operation", value.Operation) != nil || !value.Family.Valid() || !value.Action.Valid() ||
		bounded("account id", value.AccountID) != nil || bounded("identity id", value.IdentityID) != nil ||
		token("customer id", value.CustomerID) != nil || value.CustomerVersion == 0 ||
		value.CustomerHash.Validate() != nil || bounded("recipient ref", value.RecipientRef) != nil ||
		bounded("destination", value.Destination) != nil || value.DestinationHash.Validate() != nil ||
		bounded("channel", value.Channel) != nil || bounded("purpose", value.Purpose) != nil ||
		bounded("jurisdiction", value.Jurisdiction) != nil || !value.DataClassification.Valid() ||
		bounded("privacy policy ref", value.PrivacyPolicyRef) != nil ||
		bounded("legal policy ref", value.LegalPolicyRef) != nil ||
		bounded("security policy ref", value.SecurityPolicyRef) != nil ||
		!distinctValues(value.ClaimRefs, 128, 1024) || value.OutputBytes == 0 ||
		value.OutputBytes > 1<<20 || !validUTC(value.IssuedAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.IssuedAt) || value.ExpiresAt.Sub(value.IssuedAt) > 10*time.Minute ||
		len(value.TargetURL) == 0 || len(value.TargetURL) > 4096 {
		return fmt.Errorf("customer adapter: request identity, policy, or time is invalid")
	}
	for name, field := range map[string]string{
		"audience": value.Audience, "brand policy ref": value.BrandPolicyRef,
		"compensation policy ref": value.CompensationPolicyRef,
		"contract ref":            value.ContractRef, "support queue": value.SupportQueue,
		"expected resource version": value.ExpectedResourceVersion,
		"compensates key":           value.CompensatesKey,
	} {
		if field != "" && bounded(name, field) != nil {
			return fmt.Errorf("customer adapter: request contains an invalid %s", name)
		}
	}
	if DestinationHash(value.Destination) != value.DestinationHash {
		return fmt.Errorf("customer adapter: destination hash mismatch")
	}
	if len(value.Payload) == 0 || len(value.Payload) > 128<<10 || !json.Valid(value.Payload) {
		return fmt.Errorf("customer adapter: payload is invalid or unbounded")
	}
	var payload any
	if err := decodeStrict(value.Payload, &payload); err != nil {
		return fmt.Errorf("customer adapter: payload is invalid: %w", err)
	}
	canonical, err := json.Marshal(payload)
	if err != nil || !bytes.Equal(canonical, value.Payload) {
		return fmt.Errorf("customer adapter: payload is not canonical")
	}
	if err := rejectAuthorityFields(payload, 0); err != nil {
		return err
	}
	if value.ContractRef == "" {
		if value.ContractHash.Algorithm != "" || value.ContractHash.Digest != "" {
			return fmt.Errorf("customer adapter: contract hash lacks a contract reference")
		}
	} else if value.ContractHash.Validate() != nil {
		return fmt.Errorf("customer adapter: contract reference lacks a valid hash")
	}
	if value.ConsentID == "" {
		if value.ConsentVersion != 0 || value.ConsentHash.Algorithm != "" || value.ConsentHash.Digest != "" {
			return fmt.Errorf("customer adapter: consent binding is invalid")
		}
	} else if token("consent id", value.ConsentID) != nil || value.ConsentVersion == 0 ||
		value.ConsentHash.Validate() != nil {
		return fmt.Errorf("customer adapter: consent binding is invalid")
	}
	return nil
}

type Envelope struct {
	SchemaVersion     string                `json:"schema_version"`
	ConnectionID      string                `json:"connection_id"`
	ConnectionVersion uint64                `json:"connection_version"`
	ConnectionHash    contracts.ContentHash `json:"connection_hash"`
	Grant             lease.Grant           `json:"grant"`
	Request           Request               `json:"request"`
	External          external.Envelope     `json:"external"`
}

func (value Envelope) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("connection id", value.ConnectionID) != nil ||
		value.ConnectionVersion == 0 || value.ConnectionHash.Validate() != nil ||
		value.Grant.Request.Validate() != nil || value.Grant.Fence.Validate() != nil ||
		value.Grant.State != lease.StateActive || value.Request.Validate() != nil ||
		value.External.Validate() != nil || value.Request.ExpiresAt.After(value.Grant.ExpiresAt) {
		return fmt.Errorf("customer adapter: envelope authority or request is invalid")
	}
	left, err := json.Marshal(&value.Grant)
	if err != nil {
		return err
	}
	right, err := json.Marshal(&value.External.Grant)
	if err != nil || !bytes.Equal(left, right) {
		return fmt.Errorf("customer adapter: external grant does not match customer grant")
	}
	return nil
}

type ProviderCommand struct {
	SchemaVersion      string                      `json:"schema_version"`
	Family             Family                      `json:"family"`
	Action             Action                      `json:"action"`
	AccountID          string                      `json:"account_id"`
	IdentityID         string                      `json:"identity_id"`
	CustomerID         string                      `json:"customer_id"`
	RecipientRef       string                      `json:"recipient_ref"`
	Destination        string                      `json:"destination"`
	DestinationHash    contracts.ContentHash       `json:"destination_hash"`
	Channel            string                      `json:"channel"`
	Audience           string                      `json:"audience"`
	Purpose            string                      `json:"purpose"`
	Jurisdiction       string                      `json:"jurisdiction"`
	DataClassification external.DataClassification `json:"data_classification"`
	ConsentID          string                      `json:"consent_id"`
	ClaimRefs          []string                    `json:"claim_refs"`
	BrandPolicyRef     string                      `json:"brand_policy_ref"`
	PrivacyPolicyRef   string                      `json:"privacy_policy_ref"`
	LegalPolicyRef     string                      `json:"legal_policy_ref"`
	ContractRef        string                      `json:"contract_ref"`
	ContractHash       contracts.ContentHash       `json:"contract_hash"`
	SupportQueue       string                      `json:"support_queue"`
	Payload            json.RawMessage             `json:"payload"`
}

type ProviderOutcome struct {
	SchemaVersion   string                 `json:"schema_version"`
	CustomerID      string                 `json:"customer_id"`
	RecipientRef    string                 `json:"recipient_ref"`
	DestinationHash contracts.ContentHash  `json:"destination_hash"`
	AccountID       string                 `json:"account_id"`
	IdentityID      string                 `json:"identity_id"`
	Channel         string                 `json:"channel"`
	Purpose         string                 `json:"purpose"`
	ExternalID      string                 `json:"external_id"`
	State           external.ExternalState `json:"state"`
	ResourceVersion string                 `json:"resource_version"`
	ObservedAt      time.Time              `json:"observed_at"`
	UntrustedData   bool                   `json:"untrusted_data"`
	Details         json.RawMessage        `json:"details"`
}

func (value ProviderOutcome) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("customer id", value.CustomerID) != nil ||
		bounded("recipient ref", value.RecipientRef) != nil || value.DestinationHash.Validate() != nil ||
		bounded("account id", value.AccountID) != nil || bounded("identity id", value.IdentityID) != nil ||
		bounded("channel", value.Channel) != nil || bounded("purpose", value.Purpose) != nil ||
		bounded("external id", value.ExternalID) != nil || !value.State.Valid() ||
		!validUTC(value.ObservedAt) || !value.UntrustedData || len(value.Details) == 0 ||
		len(value.Details) > 128<<10 || !json.Valid(value.Details) {
		return fmt.Errorf("customer adapter: provider outcome is invalid")
	}
	var details any
	if err := decodeStrict(value.Details, &details); err != nil {
		return err
	}
	canonical, err := json.Marshal(details)
	if err != nil || !bytes.Equal(canonical, value.Details) {
		return fmt.Errorf("customer adapter: provider outcome details are not canonical")
	}
	return nil
}

type Observation struct {
	SchemaVersion      string                        `json:"schema_version"`
	OrganizationID     contracts.OrganizationID      `json:"organization_id"`
	ConnectionID       string                        `json:"connection_id"`
	ConnectionVersion  uint64                        `json:"connection_version"`
	Family             Family                        `json:"family"`
	Operation          string                        `json:"operation"`
	Action             Action                        `json:"action"`
	CustomerID         string                        `json:"customer_id"`
	CustomerVersion    uint64                        `json:"customer_version"`
	RecipientHash      contracts.ContentHash         `json:"recipient_hash"`
	ConsentID          string                        `json:"consent_id"`
	ConsentVersion     uint64                        `json:"consent_version"`
	Channel            string                        `json:"channel"`
	Purpose            string                        `json:"purpose"`
	Jurisdiction       string                        `json:"jurisdiction"`
	DataClassification external.DataClassification   `json:"data_classification"`
	ExternalID         string                        `json:"external_id"`
	IdempotencyKey     string                        `json:"idempotency_key"`
	State              external.ExternalState        `json:"state"`
	Authority          external.ObservationAuthority `json:"authority"`
	UntrustedData      bool                          `json:"untrusted_data"`
	ProviderObservedAt time.Time                     `json:"provider_observed_at"`
	CapturedAt         time.Time                     `json:"captured_at"`
	ExternalHash       contracts.ContentHash         `json:"external_hash"`
	OutcomeHash        contracts.ContentHash         `json:"outcome_hash"`
	Outcome            ProviderOutcome               `json:"outcome"`
}

func (value Observation) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("organization id", string(value.OrganizationID)) != nil ||
		token("connection id", value.ConnectionID) != nil || value.ConnectionVersion == 0 ||
		!value.Family.Valid() || token("operation", value.Operation) != nil || !value.Action.Valid() ||
		token("customer id", value.CustomerID) != nil || value.CustomerVersion == 0 ||
		value.RecipientHash.Validate() != nil || bounded("channel", value.Channel) != nil ||
		bounded("purpose", value.Purpose) != nil || bounded("jurisdiction", value.Jurisdiction) != nil ||
		!value.DataClassification.Valid() || bounded("external id", value.ExternalID) != nil ||
		token("idempotency key", value.IdempotencyKey) != nil || !value.State.Valid() ||
		!value.Authority.Valid() || !value.UntrustedData || !validUTC(value.ProviderObservedAt) ||
		!validUTC(value.CapturedAt) || value.ProviderObservedAt.After(value.CapturedAt.Add(5*time.Minute)) ||
		value.ExternalHash.Validate() != nil || value.OutcomeHash.Validate() != nil ||
		value.Outcome.Validate() != nil {
		return fmt.Errorf("customer adapter: observation is invalid")
	}
	if value.ConsentID == "" {
		if value.ConsentVersion != 0 {
			return fmt.Errorf("customer adapter: observation consent binding is invalid")
		}
	} else if token("consent id", value.ConsentID) != nil || value.ConsentVersion == 0 {
		return fmt.Errorf("customer adapter: observation consent binding is invalid")
	}
	return nil
}

type ConnectionView struct {
	Connection Connection            `json:"connection"`
	Hash       contracts.ContentHash `json:"hash"`
}

type CustomerView struct {
	Customer CustomerScope         `json:"customer"`
	Hash     contracts.ContentHash `json:"hash"`
}

type ConsentView struct {
	Consent ConsentRecord         `json:"consent"`
	Hash    contracts.ContentHash `json:"hash"`
}

func DestinationHash(destination string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(destination))
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func CanonicalHash[T interface{ Validate() error }](value T) (contracts.ContentHash, error) {
	if err := value.Validate(); err != nil {
		return contracts.ContentHash{}, err
	}
	encoded, err := contracts.EncodeCanonical(value)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(encoded)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func token(name, value string) error {
	if strings.TrimSpace(value) != value || value == "" || len(value) > 128 {
		return fmt.Errorf("%s must contain 1 to 128 bytes", name)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' ||
			char == '.' || char == ':' || char == '/' {
			continue
		}
		return fmt.Errorf("%s contains an invalid character", name)
	}
	return nil
}

func bounded(name, value string) error {
	if strings.TrimSpace(value) != value || value == "" || len(value) > 1024 ||
		strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func distinctTokens(values []string, maximum int) bool {
	if len(values) == 0 || len(values) > maximum {
		return false
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if token("value", value) != nil || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func distinctValues(values []string, maximum, maximumBytes int) bool {
	if len(values) > maximum {
		return false
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != value || value == "" || len(value) > maximumBytes ||
			strings.ContainsAny(value, "\r\n\x00") || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsClassification(values []external.DataClassification, target external.DataClassification) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsBasis(values []ConsentBasis, target ConsentBasis) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validUTC(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values are forbidden")
		}
		return err
	}
	return nil
}

func rejectAuthorityFields(value any, depth int) error {
	if depth > 32 {
		return fmt.Errorf("%w: payload nesting exceeds policy", ErrDenied)
	}
	blocked := map[string]bool{
		"account_id": true, "identity_id": true, "recipient": true,
		"recipient_ref": true, "destination": true, "mission": true,
		"constitution": true, "authority": true, "mandate": true,
		"policy": true, "system_instruction": true, "system_prompt": true,
		"tool": true, "tools": true, "capability": true, "capabilities": true,
		"consent_id": true, "jurisdiction": true, "brand_policy_ref": true,
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if blocked[strings.ToLower(strings.TrimSpace(key))] {
				return fmt.Errorf("%w: payload attempts to supply an authority-owned field", ErrPromptInjection)
			}
			if err := rejectAuthorityFields(child, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := rejectAuthorityFields(child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
