package companystate

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

const (
	RecordSchemaVersion            = "workforce.company-state.record.v1"
	StoreSchemaVersion             = "workforce.company-state.store.v1"
	LegacyStoreSchemaVersion       = "workforce.v1"
	MigrationManifestSchemaVersion = "workforce.company-state-migration-manifest.v1"
)

type RecordKind string

const (
	RecordMarket              RecordKind = "market"
	RecordCustomerSegment     RecordKind = "customer_segment"
	RecordCustomer            RecordKind = "customer"
	RecordLead                RecordKind = "lead"
	RecordOpportunity         RecordKind = "opportunity"
	RecordDemandSignal        RecordKind = "demand_signal"
	RecordCompetitor          RecordKind = "competitor"
	RecordProduct             RecordKind = "product"
	RecordProductVersion      RecordKind = "product_version"
	RecordValueProposition    RecordKind = "value_proposition"
	RecordBusinessModel       RecordKind = "business_model"
	RecordPricePackage        RecordKind = "price_package"
	RecordHypothesis          RecordKind = "hypothesis"
	RecordExperiment          RecordKind = "experiment"
	RecordInitiative          RecordKind = "initiative"
	RecordPortfolioDecision   RecordKind = "portfolio_decision"
	RecordCampaign            RecordKind = "campaign"
	RecordSalesPipeline       RecordKind = "sales_pipeline"
	RecordContract            RecordKind = "contract"
	RecordSubscription        RecordKind = "subscription"
	RecordPurchase            RecordKind = "purchase"
	RecordRevenue             RecordKind = "revenue"
	RecordExpense             RecordKind = "expense"
	RecordAsset               RecordKind = "asset"
	RecordLiability           RecordKind = "liability"
	RecordCashPosition        RecordKind = "cash_position"
	RecordRunway              RecordKind = "runway"
	RecordConversionMetric    RecordKind = "conversion_metric"
	RecordRetentionMetric     RecordKind = "retention_metric"
	RecordSupportIssue        RecordKind = "support_issue"
	RecordOperationalIncident RecordKind = "operational_incident"
	RecordStrategicReview     RecordKind = "strategic_review"
)

func AllRecordKinds() []RecordKind {
	return []RecordKind{
		RecordMarket, RecordCustomerSegment, RecordCustomer, RecordLead,
		RecordOpportunity, RecordDemandSignal, RecordCompetitor, RecordProduct,
		RecordProductVersion, RecordValueProposition, RecordBusinessModel,
		RecordPricePackage, RecordHypothesis, RecordExperiment, RecordInitiative,
		RecordPortfolioDecision, RecordCampaign, RecordSalesPipeline,
		RecordContract, RecordSubscription, RecordPurchase, RecordRevenue,
		RecordExpense, RecordAsset, RecordLiability, RecordCashPosition,
		RecordRunway, RecordConversionMetric, RecordRetentionMetric,
		RecordSupportIssue, RecordOperationalIncident, RecordStrategicReview,
	}
}

func (value RecordKind) Valid() bool {
	return slices.Contains(AllRecordKinds(), value)
}

type Domain string

const (
	DomainMarket     Domain = "market"
	DomainCustomer   Domain = "customer"
	DomainProduct    Domain = "product"
	DomainPortfolio  Domain = "portfolio"
	DomainCommercial Domain = "commercial"
	DomainFinancial  Domain = "financial"
	DomainOperations Domain = "operations"
	DomainLearning   Domain = "learning"
)

func (value Domain) Valid() bool {
	switch value {
	case DomainMarket, DomainCustomer, DomainProduct, DomainPortfolio,
		DomainCommercial, DomainFinancial, DomainOperations, DomainLearning:
		return true
	default:
		return false
	}
}

func DomainFor(kind RecordKind) (Domain, error) {
	switch kind {
	case RecordMarket, RecordCustomerSegment, RecordDemandSignal, RecordCompetitor:
		return DomainMarket, nil
	case RecordCustomer, RecordLead, RecordSupportIssue:
		return DomainCustomer, nil
	case RecordProduct, RecordProductVersion, RecordValueProposition, RecordPricePackage:
		return DomainProduct, nil
	case RecordInitiative, RecordPortfolioDecision, RecordOpportunity:
		return DomainPortfolio, nil
	case RecordCampaign, RecordSalesPipeline, RecordContract, RecordSubscription,
		RecordPurchase, RecordBusinessModel:
		return DomainCommercial, nil
	case RecordRevenue, RecordExpense, RecordAsset, RecordLiability,
		RecordCashPosition, RecordRunway:
		return DomainFinancial, nil
	case RecordOperationalIncident:
		return DomainOperations, nil
	case RecordHypothesis, RecordExperiment, RecordConversionMetric,
		RecordRetentionMetric, RecordStrategicReview:
		return DomainLearning, nil
	default:
		return "", fmt.Errorf("company state: unsupported record kind %q", kind)
	}
}

type ObservationKind string

const (
	ObservationProviderReported    ObservationKind = "provider_reported"
	ObservationCustomerReported    ObservationKind = "customer_reported"
	ObservationReconciledFinancial ObservationKind = "reconciled_financial"
	ObservationAnalyticallyDerived ObservationKind = "analytically_derived"
	ObservationModelProposed       ObservationKind = "model_proposed"
)

func (value ObservationKind) Valid() bool {
	switch value {
	case ObservationProviderReported, ObservationCustomerReported,
		ObservationReconciledFinancial, ObservationAnalyticallyDerived,
		ObservationModelProposed:
		return true
	default:
		return false
	}
}

type TruthStatus string

const (
	TruthObserved TruthStatus = "observed"
	TruthVerified TruthStatus = "verified"
	TruthProposal TruthStatus = "proposal"
)

func (value TruthStatus) Valid() bool {
	return value == TruthObserved || value == TruthVerified || value == TruthProposal
}

type SourceKind string

const (
	SourceProviderRecord      SourceKind = "provider_record"
	SourceCustomerAttestation SourceKind = "customer_attestation"
	SourceReconciledLedger    SourceKind = "reconciled_ledger"
	SourceAnalyticalResult    SourceKind = "analytical_result"
	SourceModelOutput         SourceKind = "model_output"
	SourceHumanAttestation    SourceKind = "human_attestation"
	SourceCompanyStateRecord  SourceKind = "company_state_record"
)

func (value SourceKind) Valid() bool {
	switch value {
	case SourceProviderRecord, SourceCustomerAttestation, SourceReconciledLedger,
		SourceAnalyticalResult, SourceModelOutput, SourceHumanAttestation,
		SourceCompanyStateRecord:
		return true
	default:
		return false
	}
}

type ScopeKind string

const (
	ScopeOrganization ScopeKind = "organization"
	ScopeInitiative   ScopeKind = "initiative"
)

type InitiativeScope struct {
	Kind         ScopeKind `json:"kind"`
	InitiativeID *string   `json:"initiative_id"`
}

func (value InitiativeScope) Validate() error {
	switch value.Kind {
	case ScopeOrganization:
		if value.InitiativeID != nil {
			return fmt.Errorf("company state: organization scope cannot name an initiative")
		}
	case ScopeInitiative:
		if value.InitiativeID == nil {
			return fmt.Errorf("company state: initiative scope requires initiative_id")
		}
		if err := validateID("initiative_id", *value.InitiativeID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("company state: invalid scope kind %q", value.Kind)
	}
	return nil
}

type RecordReference struct {
	ID          string                `json:"record_id"`
	Version     uint64                `json:"version"`
	ContentHash contracts.ContentHash `json:"content_hash"`
}

func (value RecordReference) Validate() error {
	if err := validateID("record reference id", value.ID); err != nil {
		return err
	}
	if value.Version == 0 {
		return fmt.Errorf("company state: record reference version must be positive")
	}
	return value.ContentHash.Validate()
}

type EvidenceReference struct {
	ID          contracts.EvidenceID  `json:"evidence_id"`
	Kind        SourceKind            `json:"source_kind"`
	Reference   string                `json:"reference"`
	ContentHash contracts.ContentHash `json:"content_hash"`
	ObservedAt  time.Time             `json:"observed_at"`
}

func (value EvidenceReference) Validate() error {
	if err := validateID("evidence_id", string(value.ID)); err != nil {
		return err
	}
	if !value.Kind.Valid() {
		return fmt.Errorf("company state: invalid evidence source %q", value.Kind)
	}
	if err := validateReference(value.Reference); err != nil {
		return err
	}
	if err := value.ContentHash.Validate(); err != nil {
		return fmt.Errorf("company state: evidence content hash: %w", err)
	}
	if !validUTC(value.ObservedAt) {
		return fmt.Errorf("company state: evidence observed_at must be non-zero UTC")
	}
	return nil
}

type ProvenanceEdge struct {
	Source     RecordReference      `json:"source"`
	Relation   string               `json:"relation"`
	EvidenceID contracts.EvidenceID `json:"evidence_id"`
}

func (value ProvenanceEdge) Validate() error {
	if err := value.Source.Validate(); err != nil {
		return err
	}
	if err := validateID("provenance relation", value.Relation); err != nil {
		return err
	}
	return validateID("provenance evidence_id", string(value.EvidenceID))
}

type PrincipalKind string

const (
	PrincipalOrganization     PrincipalKind = "organization"
	PrincipalInitiative       PrincipalKind = "initiative"
	PrincipalDepartment       PrincipalKind = "department"
	PrincipalSeat             PrincipalKind = "seat"
	PrincipalProject          PrincipalKind = "project"
	PrincipalCustomer         PrincipalKind = "customer"
	PrincipalFinancialAccount PrincipalKind = "financial_account"
	PrincipalCapability       PrincipalKind = "capability"
)

func (value PrincipalKind) Valid() bool {
	switch value {
	case PrincipalOrganization, PrincipalInitiative, PrincipalDepartment,
		PrincipalSeat, PrincipalProject, PrincipalCustomer,
		PrincipalFinancialAccount, PrincipalCapability:
		return true
	default:
		return false
	}
}

type AccessGrant struct {
	PrincipalKind  PrincipalKind            `json:"principal_kind"`
	PrincipalID    string                   `json:"principal_id"`
	Classification contracts.Classification `json:"classification"`
	Purpose        string                   `json:"purpose"`
	ConsentRef     *string                  `json:"consent_ref"`
	Jurisdictions  []string                 `json:"jurisdictions"`
	ExpiresAt      *time.Time               `json:"expires_at"`
}

func (value AccessGrant) Validate(effectiveAt time.Time) error {
	if !value.PrincipalKind.Valid() {
		return fmt.Errorf("company state: invalid access principal %q", value.PrincipalKind)
	}
	if err := validateID("access principal_id", value.PrincipalID); err != nil {
		return err
	}
	if !value.Classification.Valid() {
		return fmt.Errorf("company state: invalid access classification %q", value.Classification)
	}
	if err := validateText("access purpose", value.Purpose, 512); err != nil {
		return err
	}
	if value.ConsentRef != nil {
		if err := validateID("access consent_ref", *value.ConsentRef); err != nil {
			return err
		}
	}
	if err := validateSortedTokens("access jurisdictions", value.Jurisdictions, 0, 32); err != nil {
		return err
	}
	if value.ExpiresAt != nil && (!validUTC(*value.ExpiresAt) || !value.ExpiresAt.After(effectiveAt)) {
		return fmt.Errorf("company state: access expiry must be UTC and after effective_at")
	}
	return nil
}

type ValueKind string

const (
	ValueText            ValueKind = "text"
	ValueIdentifier      ValueKind = "identifier"
	ValueInteger         ValueKind = "integer"
	ValueBoolean         ValueKind = "boolean"
	ValueTimestamp       ValueKind = "timestamp"
	ValueMoneyMinor      ValueKind = "money_minor"
	ValueBasisPoints     ValueKind = "basis_points"
	ValueMicros          ValueKind = "micros"
	ValueTextSet         ValueKind = "text_set"
	ValueRecordReference ValueKind = "record_reference"
)

type Attribute struct {
	Name      string           `json:"name"`
	Kind      ValueKind        `json:"kind"`
	Text      string           `json:"text"`
	Integer   int64            `json:"integer"`
	Boolean   bool             `json:"boolean"`
	Timestamp *time.Time       `json:"timestamp"`
	Currency  string           `json:"currency"`
	Scale     uint32           `json:"scale"`
	Values    []string         `json:"values"`
	Reference *RecordReference `json:"reference"`
}

func (value Attribute) Validate() error {
	if err := validateID("attribute name", value.Name); err != nil {
		return err
	}
	zeroCommon := func() bool {
		return value.Text == "" && value.Integer == 0 && !value.Boolean &&
			value.Timestamp == nil && value.Currency == "" && value.Scale == 0 &&
			len(value.Values) == 0 && value.Reference == nil
	}
	switch value.Kind {
	case ValueText:
		if err := validateText("text attribute", value.Text, 16<<10); err != nil {
			return err
		}
		if value.Integer != 0 || value.Boolean || value.Timestamp != nil || value.Currency != "" || value.Scale != 0 || len(value.Values) != 0 || value.Reference != nil {
			return fmt.Errorf("company state: text attribute contains fields from another value kind")
		}
	case ValueIdentifier:
		if err := validateID("identifier attribute", value.Text); err != nil {
			return err
		}
		if value.Integer != 0 || value.Boolean || value.Timestamp != nil || value.Currency != "" || value.Scale != 0 || len(value.Values) != 0 || value.Reference != nil {
			return fmt.Errorf("company state: identifier attribute contains fields from another value kind")
		}
	case ValueInteger:
		if value.Text != "" || value.Boolean || value.Timestamp != nil || value.Currency != "" || value.Scale != 0 || len(value.Values) != 0 || value.Reference != nil {
			return fmt.Errorf("company state: integer attribute contains fields from another value kind")
		}
	case ValueBoolean:
		if value.Text != "" || value.Integer != 0 || value.Timestamp != nil || value.Currency != "" || value.Scale != 0 || len(value.Values) != 0 || value.Reference != nil {
			return fmt.Errorf("company state: boolean attribute contains fields from another value kind")
		}
	case ValueTimestamp:
		if value.Timestamp == nil || !validUTC(*value.Timestamp) || value.Text != "" || value.Integer != 0 || value.Boolean || value.Currency != "" || value.Scale != 0 || len(value.Values) != 0 || value.Reference != nil {
			return fmt.Errorf("company state: timestamp attribute is invalid")
		}
	case ValueMoneyMinor:
		if err := validateID("money currency", value.Currency); err != nil {
			return err
		}
		if value.Text != "" || value.Boolean || value.Timestamp != nil || value.Scale != 0 || len(value.Values) != 0 || value.Reference != nil {
			return fmt.Errorf("company state: money attribute contains fields from another value kind")
		}
	case ValueBasisPoints:
		if value.Integer < 0 || value.Integer > 10_000 || value.Text != "" || value.Boolean || value.Timestamp != nil || value.Currency != "" || value.Scale != 0 || len(value.Values) != 0 || value.Reference != nil {
			return fmt.Errorf("company state: basis-points attribute must be between 0 and 10000")
		}
	case ValueMicros:
		if value.Scale != 6 || value.Text != "" || value.Boolean || value.Timestamp != nil || value.Currency != "" || len(value.Values) != 0 || value.Reference != nil {
			return fmt.Errorf("company state: micros attribute must declare scale 6")
		}
	case ValueTextSet:
		if err := validateSortedText("text-set attribute", value.Values, 1, 256); err != nil {
			return err
		}
		if value.Text != "" || value.Integer != 0 || value.Boolean || value.Timestamp != nil || value.Currency != "" || value.Scale != 0 || value.Reference != nil {
			return fmt.Errorf("company state: text-set attribute contains fields from another value kind")
		}
	case ValueRecordReference:
		if value.Reference == nil {
			return fmt.Errorf("company state: record-reference attribute requires reference")
		}
		if err := value.Reference.Validate(); err != nil {
			return err
		}
		if value.Text != "" || value.Integer != 0 || value.Boolean || value.Timestamp != nil || value.Currency != "" || value.Scale != 0 || len(value.Values) != 0 {
			return fmt.Errorf("company state: record-reference attribute contains fields from another value kind")
		}
	default:
		if zeroCommon() {
			return fmt.Errorf("company state: attribute value kind %q is invalid", value.Kind)
		}
		return fmt.Errorf("company state: attribute value kind %q is invalid", value.Kind)
	}
	return nil
}

type RevisionState string

const (
	RevisionAssert    RevisionState = "assert"
	RevisionSupersede RevisionState = "supersede"
	RevisionRetract   RevisionState = "retract"
)

type Revision struct {
	State  RevisionState    `json:"state"`
	Prior  *RecordReference `json:"prior"`
	Reason *string          `json:"reason"`
}

func (value Revision) Validate(recordID string, version uint64) error {
	switch value.State {
	case RevisionAssert:
		if version != 1 || value.Prior != nil || value.Reason != nil {
			return fmt.Errorf("company state: initial assertion must be version one without a prior record")
		}
	case RevisionSupersede, RevisionRetract:
		if version <= 1 || value.Prior == nil || value.Reason == nil {
			return fmt.Errorf("company state: revision requires a prior record and reason")
		}
		if err := value.Prior.Validate(); err != nil {
			return err
		}
		if value.Prior.ID != recordID || value.Prior.Version+1 != version {
			return fmt.Errorf("company state: revision must advance the same canonical identity by one version")
		}
		if err := validateText("revision reason", *value.Reason, 2048); err != nil {
			return err
		}
	default:
		return fmt.Errorf("company state: revision state %q is invalid", value.State)
	}
	return nil
}

type RecordBody struct {
	SchemaVersion         string                   `json:"schema_version"`
	ID                    string                   `json:"record_id"`
	Version               uint64                   `json:"version"`
	Kind                  RecordKind               `json:"kind"`
	Domain                Domain                   `json:"domain"`
	OrganizationID        contracts.OrganizationID `json:"organization_id"`
	Scope                 InitiativeScope          `json:"scope"`
	AuthorSeatID          contracts.SeatID         `json:"author_seat_id"`
	Observation           ObservationKind          `json:"observation"`
	TruthStatus           TruthStatus              `json:"truth_status"`
	ConfidenceBasisPoints *uint16                  `json:"confidence_basis_points"`
	Evidence              []EvidenceReference      `json:"evidence"`
	Provenance            []ProvenanceEdge         `json:"provenance"`
	ObservedAt            time.Time                `json:"observed_at"`
	EffectiveAt           time.Time                `json:"effective_at"`
	ExpiresAt             *time.Time               `json:"expires_at"`
	Validity              contracts.Validity       `json:"validity"`
	Classification        contracts.Classification `json:"classification"`
	Access                []AccessGrant            `json:"access"`
	Revision              Revision                 `json:"revision"`
	Attributes            []Attribute              `json:"attributes"`
}

func (value RecordBody) Validate() error {
	if value.SchemaVersion != RecordSchemaVersion {
		return fmt.Errorf("company state: unsupported schema version %q", value.SchemaVersion)
	}
	if err := validateID("record_id", value.ID); err != nil {
		return err
	}
	if value.Version == 0 || !value.Kind.Valid() {
		return fmt.Errorf("company state: record version and kind are invalid")
	}
	domain, err := DomainFor(value.Kind)
	if err != nil || value.Domain != domain {
		return fmt.Errorf("company state: record kind %q does not belong to domain %q", value.Kind, value.Domain)
	}
	if err := validateID("organization_id", string(value.OrganizationID)); err != nil {
		return err
	}
	if err := value.Scope.Validate(); err != nil {
		return err
	}
	if err := validateID("author_seat_id", string(value.AuthorSeatID)); err != nil {
		return err
	}
	if !value.Observation.Valid() || !value.TruthStatus.Valid() {
		return fmt.Errorf("company state: observation taxonomy is invalid")
	}
	if value.Observation == ObservationModelProposed && value.TruthStatus != TruthProposal {
		return fmt.Errorf("company state: model proposals cannot become observed business truth")
	}
	if value.Observation != ObservationModelProposed && value.TruthStatus == TruthProposal {
		return fmt.Errorf("company state: proposal truth status is reserved for model proposals")
	}
	if value.Observation == ObservationModelProposed || value.Observation == ObservationAnalyticallyDerived {
		if value.ConfidenceBasisPoints == nil {
			return fmt.Errorf("company state: derived and model-proposed claims require confidence")
		}
	}
	if value.ConfidenceBasisPoints != nil && *value.ConfidenceBasisPoints > 10_000 {
		return fmt.Errorf("company state: confidence exceeds 10000 basis points")
	}
	if len(value.Evidence) == 0 || len(value.Evidence) > 256 {
		return fmt.Errorf("company state: every record requires 1 to 256 evidence references")
	}
	for index := range value.Evidence {
		if err := value.Evidence[index].Validate(); err != nil {
			return fmt.Errorf("company state: evidence %d: %w", index, err)
		}
		if index > 0 && string(value.Evidence[index-1].ID) >= string(value.Evidence[index].ID) {
			return fmt.Errorf("company state: evidence must be sorted and unique by evidence_id")
		}
	}
	if !evidenceMatchesObservation(value.Observation, value.Evidence) {
		return fmt.Errorf("company state: evidence taxonomy does not support observation %q", value.Observation)
	}
	if len(value.Provenance) > 1024 {
		return fmt.Errorf("company state: provenance exceeds 1024 edges")
	}
	for index := range value.Provenance {
		if err := value.Provenance[index].Validate(); err != nil {
			return fmt.Errorf("company state: provenance %d: %w", index, err)
		}
		if value.Provenance[index].Source.ID == value.ID && value.Provenance[index].Source.Version == value.Version {
			return fmt.Errorf("company state: record cannot derive from itself")
		}
		if index > 0 && provenanceKey(value.Provenance[index-1]) >= provenanceKey(value.Provenance[index]) {
			return fmt.Errorf("company state: provenance must be sorted and unique")
		}
	}
	if !validUTC(value.ObservedAt) || !validUTC(value.EffectiveAt) || value.EffectiveAt.Before(value.ObservedAt) {
		return fmt.Errorf("company state: observed and effective times are invalid")
	}
	if value.ExpiresAt != nil && (!validUTC(*value.ExpiresAt) || !value.ExpiresAt.After(value.EffectiveAt)) {
		return fmt.Errorf("company state: expiry must be UTC and after effective_at")
	}
	if value.Validity != contracts.ValidityActive && value.Validity != contracts.ValidityContested && value.Validity != contracts.ValidityRetracted {
		return fmt.Errorf("company state: immutable assertion validity %q is invalid", value.Validity)
	}
	if value.Revision.State == RevisionRetract && value.Validity != contracts.ValidityRetracted {
		return fmt.Errorf("company state: retraction revision requires retracted validity")
	}
	if value.Revision.State != RevisionRetract && value.Validity == contracts.ValidityRetracted {
		return fmt.Errorf("company state: retracted validity requires a retraction revision")
	}
	if !value.Classification.Valid() {
		return fmt.Errorf("company state: classification %q is invalid", value.Classification)
	}
	if len(value.Access) == 0 || len(value.Access) > 256 {
		return fmt.Errorf("company state: every record requires 1 to 256 access grants")
	}
	for index := range value.Access {
		if err := value.Access[index].Validate(value.EffectiveAt); err != nil {
			return fmt.Errorf("company state: access grant %d: %w", index, err)
		}
		if !classificationAtLeast(value.Access[index].Classification, value.Classification) {
			return fmt.Errorf("company state: access grant %d is weaker than record classification", index)
		}
		if index > 0 && accessKey(value.Access[index-1]) >= accessKey(value.Access[index]) {
			return fmt.Errorf("company state: access grants must be sorted and unique")
		}
	}
	if err := value.Revision.Validate(value.ID, value.Version); err != nil {
		return err
	}
	return validateAttributes(value.Kind, value.Attributes)
}

type Record struct {
	Body        RecordBody            `json:"body"`
	ContentHash contracts.ContentHash `json:"content_hash"`
	Signature   contracts.Signature   `json:"signature"`
}

func (value Record) Validate() error {
	if err := value.Body.Validate(); err != nil {
		return err
	}
	if err := value.ContentHash.Validate(); err != nil {
		return fmt.Errorf("company state: content hash: %w", err)
	}
	if err := value.Signature.Validate(); err != nil {
		return fmt.Errorf("company state: signature: %w", err)
	}
	return nil
}

func evidenceMatchesObservation(observation ObservationKind, evidence []EvidenceReference) bool {
	wanted := SourceKind("")
	switch observation {
	case ObservationProviderReported:
		wanted = SourceProviderRecord
	case ObservationCustomerReported:
		wanted = SourceCustomerAttestation
	case ObservationReconciledFinancial:
		wanted = SourceReconciledLedger
	case ObservationAnalyticallyDerived:
		wanted = SourceAnalyticalResult
	case ObservationModelProposed:
		wanted = SourceModelOutput
	}
	for index := range evidence {
		if evidence[index].Kind == wanted {
			return true
		}
	}
	return false
}

func provenanceKey(value ProvenanceEdge) string {
	return fmt.Sprintf("%s/%020d/%s/%s", value.Source.ID, value.Source.Version, value.Relation, value.EvidenceID)
}

func accessKey(value AccessGrant) string {
	return strings.Join([]string{string(value.PrincipalKind), value.PrincipalID, value.Purpose}, "/")
}

func classificationAtLeast(candidate, required contracts.Classification) bool {
	ranks := func(value contracts.Classification) int {
		switch value {
		case contracts.ClassificationOrganization:
			return 1
		case contracts.ClassificationDepartment:
			return 2
		case contracts.ClassificationSeat:
			return 3
		case contracts.ClassificationProject:
			return 4
		case contracts.ClassificationRestricted:
			return 5
		default:
			return 0
		}
	}
	return ranks(candidate) >= ranks(required)
}

func validateID(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 128 {
		return fmt.Errorf("company state: %s must contain 1 to 128 canonical bytes", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-_.:", character) {
			continue
		}
		return fmt.Errorf("company state: %s contains an invalid character", name)
	}
	return nil
}

func validateReference(value string) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 2048 || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("company state: evidence reference must contain 1 to 2048 safe canonical bytes")
	}
	return nil
}

func validateText(name, value string, maximum int) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > maximum || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("company state: %s must contain 1 to %d canonical bytes", name, maximum)
	}
	return nil
}

func validateSortedTokens(name string, values []string, minimum, maximum int) error {
	if len(values) < minimum || len(values) > maximum {
		return fmt.Errorf("company state: %s must contain %d to %d values", name, minimum, maximum)
	}
	for index := range values {
		if err := validateID(name, values[index]); err != nil {
			return err
		}
		if index > 0 && values[index-1] >= values[index] {
			return fmt.Errorf("company state: %s must be sorted and unique", name)
		}
	}
	return nil
}

func validateSortedText(name string, values []string, minimum, maximum int) error {
	if len(values) < minimum || len(values) > maximum {
		return fmt.Errorf("company state: %s must contain %d to %d values", name, minimum, maximum)
	}
	for index := range values {
		if err := validateText(name, values[index], 2048); err != nil {
			return err
		}
		if index > 0 && values[index-1] >= values[index] {
			return fmt.Errorf("company state: %s must be sorted and unique", name)
		}
	}
	return nil
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}
