package customer

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/provider/external"
	"matrix/workforce/internal/skills"
)

type EmailCommand struct {
	TemplateRef string                `json:"template_ref"`
	Subject     string                `json:"subject"`
	TextBody    string                `json:"text_body"`
	HTMLHash    contracts.ContentHash `json:"html_hash"`
	ReplyToRef  string                `json:"reply_to_ref"`
}

func (value EmailCommand) Validate() error {
	if bounded("email template ref", value.TemplateRef) != nil ||
		boundedText("email subject", value.Subject, 998) != nil ||
		boundedText("email text body", value.TextBody, 96<<10) != nil ||
		value.HTMLHash.Validate() != nil {
		return fmt.Errorf("customer adapter: email command is invalid")
	}
	if value.ReplyToRef != "" && bounded("reply-to ref", value.ReplyToRef) != nil {
		return fmt.Errorf("customer adapter: email reply-to reference is invalid")
	}
	return nil
}

type OutboundMessageCommand struct {
	TemplateRef     string          `json:"template_ref"`
	CampaignRef     string          `json:"campaign_ref"`
	ConversationRef string          `json:"conversation_ref"`
	Content         json.RawMessage `json:"content"`
}

func (value OutboundMessageCommand) Validate() error {
	if bounded("outbound template ref", value.TemplateRef) != nil ||
		bounded("outbound campaign ref", value.CampaignRef) != nil ||
		bounded("outbound conversation ref", value.ConversationRef) != nil {
		return fmt.Errorf("customer adapter: outbound message identity is invalid")
	}
	return validatePayloadFragment(value.Content, 96<<10)
}

type SocialPublicationCommand struct {
	PublicationRef string                  `json:"publication_ref"`
	Content        string                  `json:"content"`
	MediaHashes    []contracts.ContentHash `json:"media_hashes"`
	ScheduledAt    *time.Time              `json:"scheduled_at"`
}

func (value SocialPublicationCommand) Validate() error {
	if bounded("publication ref", value.PublicationRef) != nil ||
		boundedText("social content", value.Content, 32<<10) != nil || len(value.MediaHashes) > 32 {
		return fmt.Errorf("customer adapter: social publication is invalid")
	}
	for _, hash := range value.MediaHashes {
		if hash.Validate() != nil {
			return fmt.Errorf("customer adapter: social media hash is invalid")
		}
	}
	if value.ScheduledAt != nil && !validUTC(*value.ScheduledAt) {
		return fmt.Errorf("customer adapter: social schedule must be UTC")
	}
	return nil
}

type CRMCommand struct {
	RecordRef       string          `json:"record_ref"`
	RecordKind      string          `json:"record_kind"`
	ExpectedVersion string          `json:"expected_version"`
	Fields          json.RawMessage `json:"fields"`
}

func (value CRMCommand) Validate() error {
	if bounded("CRM record ref", value.RecordRef) != nil ||
		bounded("CRM record kind", value.RecordKind) != nil ||
		(value.ExpectedVersion != "" && bounded("CRM expected version", value.ExpectedVersion) != nil) {
		return fmt.Errorf("customer adapter: CRM command identity is invalid")
	}
	return validatePayloadFragment(value.Fields, 96<<10)
}

type SalesPipelineCommand struct {
	OpportunityRef  string                `json:"opportunity_ref"`
	FromStage       string                `json:"from_stage"`
	ToStage         string                `json:"to_stage"`
	ConversationRef string                `json:"conversation_ref"`
	ProposalHash    contracts.ContentHash `json:"proposal_hash"`
}

func (value SalesPipelineCommand) Validate() error {
	if bounded("sales opportunity ref", value.OpportunityRef) != nil ||
		bounded("sales from stage", value.FromStage) != nil ||
		bounded("sales to stage", value.ToStage) != nil || value.FromStage == value.ToStage ||
		bounded("sales conversation ref", value.ConversationRef) != nil ||
		value.ProposalHash.Validate() != nil {
		return fmt.Errorf("customer adapter: sales pipeline transition is invalid")
	}
	return nil
}

type ContractTransmissionCommand struct {
	ContractRef  string                `json:"contract_ref"`
	ContractHash contracts.ContentHash `json:"contract_hash"`
	DeliveryMode string                `json:"delivery_mode"`
	SignerRole   string                `json:"signer_role"`
	ExpiresAt    time.Time             `json:"expires_at"`
}

func (value ContractTransmissionCommand) Validate() error {
	if bounded("contract ref", value.ContractRef) != nil || value.ContractHash.Validate() != nil ||
		bounded("contract delivery mode", value.DeliveryMode) != nil ||
		bounded("contract signer role", value.SignerRole) != nil || !validUTC(value.ExpiresAt) {
		return fmt.Errorf("customer adapter: contract transmission is invalid")
	}
	return nil
}

type OnboardingCommand struct {
	PlanRef      string    `json:"plan_ref"`
	StepRef      string    `json:"step_ref"`
	State        string    `json:"state"`
	DueAt        time.Time `json:"due_at"`
	EvidenceRefs []string  `json:"evidence_refs"`
}

func (value OnboardingCommand) Validate() error {
	if bounded("onboarding plan ref", value.PlanRef) != nil ||
		bounded("onboarding step ref", value.StepRef) != nil ||
		bounded("onboarding state", value.State) != nil || !validUTC(value.DueAt) ||
		!distinctValues(value.EvidenceRefs, 64, 1024) {
		return fmt.Errorf("customer adapter: onboarding command is invalid")
	}
	return nil
}

type SupportCommand struct {
	TicketRef      string                `json:"ticket_ref"`
	ThreadRef      string                `json:"thread_ref"`
	Kind           string                `json:"kind"`
	Severity       string                `json:"severity"`
	SLAExpiresAt   time.Time             `json:"sla_expires_at"`
	Message        string                `json:"message"`
	ResolutionHash contracts.ContentHash `json:"resolution_hash"`
}

func (value SupportCommand) Validate() error {
	if bounded("support ticket ref", value.TicketRef) != nil ||
		bounded("support thread ref", value.ThreadRef) != nil ||
		bounded("support kind", value.Kind) != nil || bounded("support severity", value.Severity) != nil ||
		!validUTC(value.SLAExpiresAt) || boundedText("support message", value.Message, 96<<10) != nil ||
		value.ResolutionHash.Validate() != nil {
		return fmt.Errorf("customer adapter: support command is invalid")
	}
	return nil
}

type CustomerObservationQuery struct {
	ObservationRef string        `json:"observation_ref"`
	SourceRef      string        `json:"source_ref"`
	MetricRef      string        `json:"metric_ref"`
	WindowStart    time.Time     `json:"window_start"`
	WindowEnd      time.Time     `json:"window_end"`
	FreshnessLimit time.Duration `json:"freshness_limit"`
}

func (value CustomerObservationQuery) Validate() error {
	if bounded("customer observation ref", value.ObservationRef) != nil ||
		bounded("customer observation source", value.SourceRef) != nil ||
		bounded("customer observation metric", value.MetricRef) != nil ||
		!validUTC(value.WindowStart) || !validUTC(value.WindowEnd) ||
		!value.WindowEnd.After(value.WindowStart) ||
		value.WindowEnd.Sub(value.WindowStart) > 366*24*time.Hour ||
		value.FreshnessLimit <= 0 || value.FreshnessLimit > 30*24*time.Hour {
		return fmt.Errorf("customer adapter: customer observation query is invalid")
	}
	return nil
}

func EncodePayload[T interface{ Validate() error }](value T) (json.RawMessage, error) {
	if err := value.Validate(); err != nil {
		return nil, err
	}
	encoded, err := contracts.EncodeCanonical(value)
	if err != nil || len(encoded) == 0 || len(encoded) > 128<<10 {
		return nil, fmt.Errorf("customer adapter: typed payload exceeds hard limits")
	}
	var decoded any
	if err := decodeStrict(encoded, &decoded); err != nil {
		return nil, err
	}
	if err := rejectAuthorityFields(decoded, 0); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), encoded...), nil
}

func DefaultOperationPolicies(family Family) ([]OperationPolicy, error) {
	var policies []OperationPolicy
	switch family {
	case FamilyEmail:
		policies = []OperationPolicy{irreversiblePolicy("email.send", family, ActionSend, external.ActionSubmit, true, true)}
	case FamilyConsentedOutbound:
		policies = []OperationPolicy{irreversiblePolicy("outbound.send", family, ActionSend, external.ActionSubmit, true, true)}
	case FamilySocialDistribution:
		policies = reversiblePolicies("social.publish", "social.unpublish", family,
			ActionPublish, external.ActionPublish, external.ActionUnpublish, true, true)
	case FamilyCRM:
		policies = reversiblePolicies("crm.upsert", "crm.restore", family,
			ActionUpsert, external.ActionConfigure, external.ActionConfigure, false, false)
	case FamilySalesPipeline:
		policies = reversiblePolicies("sales.advance", "sales.revert", family,
			ActionAdvance, external.ActionConfigure, external.ActionConfigure, false, false)
	case FamilyContractTransmission:
		policies = []OperationPolicy{irreversiblePolicy("contract.transmit", family, ActionTransmit, external.ActionSubmit, true, true)}
	case FamilyCustomerOnboarding:
		policies = reversiblePolicies("onboarding.enroll", "onboarding.unenroll", family,
			ActionEnroll, external.ActionConfigure, external.ActionConfigure, true, true)
	case FamilySupport:
		policies = []OperationPolicy{irreversiblePolicy("support.respond", family, ActionRespond, external.ActionSubmit, true, true)}
	case FamilyCustomerObservation:
		policies = []OperationPolicy{{
			Name: "customer.observe", ExternalOperation: "customer.observe",
			Family: family, Action: ActionObserve, ExternalAction: external.ActionObserve,
			EffectClass: skills.EffectRead, AuthoritativeOutcome: true,
		}}
	default:
		return nil, fmt.Errorf("customer adapter: family is invalid")
	}
	for _, policy := range policies {
		if err := policy.Validate(); err != nil {
			return nil, err
		}
	}
	return policies, nil
}

func irreversiblePolicy(
	name string,
	family Family,
	action Action,
	externalAction external.ActionClass,
	requiresConsent bool,
	countsFrequency bool,
) OperationPolicy {
	return OperationPolicy{
		Name: name, ExternalOperation: name, Family: family, Action: action,
		ExternalAction: externalAction, EffectClass: skills.EffectIrreversible,
		RequiresConsent: requiresConsent,
		ConsentBases: []ConsentBasis{
			ConsentExplicitOptIn, ConsentContractual, ConsentService, ConsentLegitimate,
		},
		CountsFrequency: countsFrequency, AuthoritativeOutcome: true,
	}
}

func reversiblePolicies(
	name, compensation string,
	family Family,
	action Action,
	externalAction, compensationAction external.ActionClass,
	requiresConsent bool,
	countsFrequency bool,
) []OperationPolicy {
	bases := []ConsentBasis(nil)
	if requiresConsent {
		bases = []ConsentBasis{
			ConsentExplicitOptIn, ConsentContractual, ConsentService, ConsentLegitimate,
		}
	}
	return []OperationPolicy{
		{
			Name: name, ExternalOperation: name, Family: family, Action: action,
			ExternalAction: externalAction, EffectClass: skills.EffectReversible,
			RequiresConsent: requiresConsent, ConsentBases: bases,
			CountsFrequency: countsFrequency, AuthoritativeOutcome: true,
			CompensationOperation: compensation,
		},
		{
			Name: compensation, ExternalOperation: compensation, Family: family,
			Action: ActionCompensate, ExternalAction: compensationAction,
			EffectClass: skills.EffectReversible, AuthoritativeOutcome: true,
			CompensatesOperation: name,
		},
	}
}

func validatePayloadFragment(value json.RawMessage, maximum int) error {
	if len(value) == 0 || len(value) > maximum || !json.Valid(value) {
		return fmt.Errorf("customer adapter: payload fragment is invalid or unbounded")
	}
	var decoded any
	if err := decodeStrict(value, &decoded); err != nil {
		return err
	}
	canonical, err := json.Marshal(decoded)
	if err != nil || string(canonical) != string(value) {
		return fmt.Errorf("customer adapter: payload fragment is not canonical")
	}
	return rejectAuthorityFields(decoded, 0)
}

func boundedText(name, value string, maximum int) error {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) ||
		strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}
