// Package businessoutcome owns authoritative metric identity, normalized
// observations, derived business outcomes, and the gates that consume them.
package businessoutcome

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

const (
	SchemaVersion           = "workforce.business-outcome.v1"
	MetricEnvelopeVersion   = "workforce.business-metric-envelope.v1"
	GateSchemaVersion       = "workforce.business-outcome-gate.v1"
	CorrectionSchemaVersion = "workforce.business-outcome-correction.v1"
)

type MetricID string
type ObservationID string
type OutcomeID string
type OutcomeChainID string
type GateID string
type CorrectionID string
type LineageEdgeID string

type OutcomeKind string

const (
	OutcomeActivity          OutcomeKind = "activity"
	OutcomeOutput            OutcomeKind = "output"
	OutcomeCustomer          OutcomeKind = "customer_outcome"
	OutcomeCommercial        OutcomeKind = "commercial_outcome"
	OutcomeEconomic          OutcomeKind = "economic_outcome"
	OutcomeRisk              OutcomeKind = "risk_outcome"
	OutcomeStrategicLearning OutcomeKind = "strategic_learning"
)

func (value OutcomeKind) Valid() bool {
	switch value {
	case OutcomeActivity, OutcomeOutput, OutcomeCustomer, OutcomeCommercial,
		OutcomeEconomic, OutcomeRisk, OutcomeStrategicLearning:
		return true
	default:
		return false
	}
}

func (value OutcomeKind) BusinessOutcome() bool {
	switch value {
	case OutcomeCustomer, OutcomeCommercial, OutcomeEconomic, OutcomeRisk,
		OutcomeStrategicLearning:
		return true
	default:
		return false
	}
}

type Aggregation string

const (
	AggregationLatest  Aggregation = "latest"
	AggregationSum     Aggregation = "sum"
	AggregationRate    Aggregation = "rate"
	AggregationMinimum Aggregation = "minimum"
	AggregationMaximum Aggregation = "maximum"
)

func (value Aggregation) Valid() bool {
	switch value {
	case AggregationLatest, AggregationSum, AggregationRate,
		AggregationMinimum, AggregationMaximum:
		return true
	default:
		return false
	}
}

type SourceFamily string

const (
	SourceProduct          SourceFamily = "product"
	SourceProductTelemetry SourceFamily = "product_telemetry"
	SourceDeployment       SourceFamily = "deployment"
	SourceExternalProvider SourceFamily = "external_provider"
	SourceCustomer         SourceFamily = "customer"
	SourceCRM              SourceFamily = "crm"
	SourceChannel          SourceFamily = "channel"
	SourceSupport          SourceFamily = "support"
	SourceCommercial       SourceFamily = "commercial"
	SourceBilling          SourceFamily = "billing"
	SourceAccounting       SourceFamily = "accounting"
	SourcePaxeer           SourceFamily = "paxeer"
	SourceLayerX           SourceFamily = "layerx"
	SourceOperational      SourceFamily = "operational"
	SourceLegal            SourceFamily = "legal"
	SourceAnalytical       SourceFamily = "analytical"
)

func (value SourceFamily) Valid() bool {
	switch value {
	case SourceProduct, SourceProductTelemetry, SourceDeployment,
		SourceExternalProvider, SourceCustomer, SourceCRM, SourceChannel,
		SourceSupport, SourceCommercial, SourceBilling, SourceAccounting,
		SourcePaxeer, SourceLayerX, SourceOperational, SourceLegal,
		SourceAnalytical:
		return true
	default:
		return false
	}
}

func (value SourceFamily) Financial() bool {
	switch value {
	case SourceBilling, SourceAccounting, SourcePaxeer, SourceLayerX:
		return true
	default:
		return false
	}
}

type ObservationAuthority string

const (
	AuthorityProviderReported    ObservationAuthority = "provider_reported"
	AuthorityCustomerReported    ObservationAuthority = "customer_reported"
	AuthorityReconciledFinancial ObservationAuthority = "reconciled_financial"
	AuthorityAnalyticallyDerived ObservationAuthority = "analytically_derived"
	AuthorityInternalVerified    ObservationAuthority = "internal_verified"
	AuthorityModelProposed       ObservationAuthority = "model_proposed"
)

func (value ObservationAuthority) Valid() bool {
	switch value {
	case AuthorityProviderReported, AuthorityCustomerReported,
		AuthorityReconciledFinancial, AuthorityAnalyticallyDerived,
		AuthorityInternalVerified, AuthorityModelProposed:
		return true
	default:
		return false
	}
}

type SourceState string

const (
	SourceCompleted  SourceState = "completed"
	SourceReconciled SourceState = "reconciled"
	SourcePending    SourceState = "pending"
	SourceProposed   SourceState = "proposed"
	SourceContested  SourceState = "contested"
	SourceReversed   SourceState = "reversed"
	SourceRetracted  SourceState = "retracted"
)

func (value SourceState) Valid() bool {
	switch value {
	case SourceCompleted, SourceReconciled, SourcePending, SourceProposed,
		SourceContested, SourceReversed, SourceRetracted:
		return true
	default:
		return false
	}
}

type MeasurementStatus string

const (
	MeasurementProposed   MeasurementStatus = "proposed"
	MeasurementPending    MeasurementStatus = "pending"
	MeasurementObserved   MeasurementStatus = "observed"
	MeasurementReconciled MeasurementStatus = "reconciled"
	MeasurementContested  MeasurementStatus = "contested"
	MeasurementRetracted  MeasurementStatus = "retracted"
)

func (value MeasurementStatus) Valid() bool {
	switch value {
	case MeasurementProposed, MeasurementPending, MeasurementObserved,
		MeasurementReconciled, MeasurementContested, MeasurementRetracted:
		return true
	default:
		return false
	}
}

type AttributionMode string

const (
	AttributionNone       AttributionMode = "none"
	AttributionDirect     AttributionMode = "direct"
	AttributionFirstTouch AttributionMode = "first_touch"
	AttributionLastTouch  AttributionMode = "last_touch"
	AttributionMultiTouch AttributionMode = "multi_touch"
	AttributionExperiment AttributionMode = "experiment"
)

func (value AttributionMode) Valid() bool {
	switch value {
	case AttributionNone, AttributionDirect, AttributionFirstTouch,
		AttributionLastTouch, AttributionMultiTouch, AttributionExperiment:
		return true
	default:
		return false
	}
}

type Comparator string

const (
	ComparatorGreaterOrEqual Comparator = "gte"
	ComparatorGreater        Comparator = "gt"
	ComparatorLessOrEqual    Comparator = "lte"
	ComparatorLess           Comparator = "lt"
	ComparatorEqual          Comparator = "eq"
)

func (value Comparator) Valid() bool {
	switch value {
	case ComparatorGreaterOrEqual, ComparatorGreater, ComparatorLessOrEqual,
		ComparatorLess, ComparatorEqual:
		return true
	default:
		return false
	}
}

func (value Comparator) Match(actual, expected int64) bool {
	switch value {
	case ComparatorGreaterOrEqual:
		return actual >= expected
	case ComparatorGreater:
		return actual > expected
	case ComparatorLessOrEqual:
		return actual <= expected
	case ComparatorLess:
		return actual < expected
	case ComparatorEqual:
		return actual == expected
	default:
		return false
	}
}

type MetricReference struct {
	ID             MetricID              `json:"metric_id"`
	Version        uint64                `json:"version"`
	DefinitionHash contracts.ContentHash `json:"definition_hash"`
}

func (value MetricReference) Validate() error {
	if err := validateToken("metric_id", string(value.ID)); err != nil {
		return err
	}
	if value.Version == 0 {
		return fmt.Errorf("business outcome: metric version must be positive")
	}
	return value.DefinitionHash.Validate()
}

type MeasureDefinition struct {
	Unit        string `json:"unit"`
	Scale       uint8  `json:"scale"`
	Numerator   string `json:"numerator"`
	Denominator string `json:"denominator"`
	Population  string `json:"population"`
	Inclusion   string `json:"inclusion"`
	Exclusion   string `json:"exclusion"`
}

func (value MeasureDefinition) Validate() error {
	if err := validateToken("metric unit", value.Unit); err != nil {
		return err
	}
	if value.Scale > 12 {
		return fmt.Errorf("business outcome: metric scale exceeds twelve decimal places")
	}
	for name, text := range map[string]string{
		"numerator": value.Numerator, "denominator": value.Denominator,
		"population": value.Population, "inclusion": value.Inclusion,
		"exclusion": value.Exclusion,
	} {
		if err := validateText("metric "+name, text, 2048); err != nil {
			return err
		}
	}
	return nil
}

type AttributionDefinition struct {
	Mode      AttributionMode                    `json:"mode"`
	Subject   string                             `json:"subject"`
	Window    time.Duration                      `json:"window"`
	Procedure contracts.VerificationProcedureRef `json:"procedure"`
}

func (value AttributionDefinition) Validate() error {
	if !value.Mode.Valid() || validateToken("attribution subject", value.Subject) != nil ||
		value.Window <= 0 || value.Window > 5*365*24*time.Hour ||
		value.Procedure.Validate() != nil {
		return fmt.Errorf("business outcome: attribution definition is invalid")
	}
	return nil
}

type FreshnessDefinition struct {
	MaximumObservationAge time.Duration `json:"maximum_observation_age"`
	MaximumCaptureDelay   time.Duration `json:"maximum_capture_delay"`
}

func (value FreshnessDefinition) Validate() error {
	if value.MaximumObservationAge <= 0 || value.MaximumObservationAge > 5*365*24*time.Hour ||
		value.MaximumCaptureDelay <= 0 || value.MaximumCaptureDelay > 30*24*time.Hour ||
		value.MaximumCaptureDelay > value.MaximumObservationAge {
		return fmt.Errorf("business outcome: freshness definition is invalid")
	}
	return nil
}

type ReconciliationDefinition struct {
	Required             bool                               `json:"required"`
	Procedure            contracts.VerificationProcedureRef `json:"procedure"`
	MaximumValuationSkew time.Duration                      `json:"maximum_valuation_skew"`
	IndependentFamilies  []SourceFamily                     `json:"independent_families"`
}

func (value ReconciliationDefinition) Validate() error {
	if value.Procedure.Validate() != nil || value.MaximumValuationSkew < 0 ||
		value.MaximumValuationSkew > 30*24*time.Hour || len(value.IndependentFamilies) > 16 {
		return fmt.Errorf("business outcome: reconciliation definition is invalid")
	}
	if value.Required && (value.MaximumValuationSkew == 0 || len(value.IndependentFamilies) == 0) {
		return fmt.Errorf("business outcome: required reconciliation lacks independent scope")
	}
	if !sortedUniqueFamilies(value.IndependentFamilies) {
		return fmt.Errorf("business outcome: reconciliation families must be sorted and unique")
	}
	return nil
}

type MeasurementValue struct {
	NumeratorMicros int64  `json:"numerator_micros"`
	Denominator     uint64 `json:"denominator"`
	ValueMicros     int64  `json:"value_micros"`
	Scale           uint8  `json:"scale"`
	Unit            string `json:"unit"`
}

func (value MeasurementValue) Validate() error {
	if value.Denominator == 0 || value.Scale > 12 || validateToken("measurement unit", value.Unit) != nil {
		return fmt.Errorf("business outcome: measurement denominator, scale, or unit is invalid")
	}
	expected, err := quotient(value.NumeratorMicros, value.Denominator, value.Scale)
	if err != nil || expected != value.ValueMicros {
		return fmt.Errorf("business outcome: measurement value does not match numerator and denominator")
	}
	return nil
}

type Baseline struct {
	Value          MeasurementValue      `json:"value"`
	Evidence       contracts.EvidenceRef `json:"evidence"`
	ObservedAt     time.Time             `json:"observed_at"`
	UncertaintyBPS uint16                `json:"uncertainty_bps"`
}

func (value Baseline) Validate() error {
	if value.Value.Validate() != nil || value.Evidence.Validate() != nil ||
		!validUTC(value.ObservedAt) || value.Evidence.ObservedAt != value.ObservedAt ||
		value.UncertaintyBPS > 10_000 {
		return fmt.Errorf("business outcome: baseline is invalid")
	}
	return nil
}

type Threshold struct {
	Comparator         Comparator `json:"comparator"`
	ValueMicros        int64      `json:"value_micros"`
	MinimumDenominator uint64     `json:"minimum_denominator"`
}

func (value Threshold) Validate() error {
	if !value.Comparator.Valid() || value.MinimumDenominator == 0 {
		return fmt.Errorf("business outcome: threshold is invalid")
	}
	return nil
}

type ThresholdDefinition struct {
	Success         Threshold     `json:"success"`
	Stop            Threshold     `json:"stop"`
	ReviewAt        time.Time     `json:"review_at"`
	ReviewEvery     time.Duration `json:"review_every"`
	MaximumDuration time.Duration `json:"maximum_duration"`
}

func (value ThresholdDefinition) Validate(registeredAt time.Time) error {
	if value.Success.Validate() != nil || value.Stop.Validate() != nil ||
		(value.Success.Comparator == value.Stop.Comparator && value.Success.ValueMicros == value.Stop.ValueMicros) ||
		!validUTC(value.ReviewAt) || !value.ReviewAt.After(registeredAt) ||
		value.ReviewEvery <= 0 || value.ReviewEvery > 365*24*time.Hour ||
		value.MaximumDuration <= 0 || value.MaximumDuration > 5*365*24*time.Hour ||
		value.ReviewAt.Sub(registeredAt) > value.MaximumDuration {
		return fmt.Errorf("business outcome: threshold schedule is invalid")
	}
	return nil
}

type MetricDefinitionBody struct {
	SchemaVersion         string                   `json:"schema_version"`
	ID                    MetricID                 `json:"metric_id"`
	Version               uint64                   `json:"version"`
	OrganizationID        contracts.OrganizationID `json:"organization_id"`
	InitiativeID          string                   `json:"initiative_id"`
	Name                  string                   `json:"name"`
	OutcomeKind           OutcomeKind              `json:"outcome_kind"`
	Aggregation           Aggregation              `json:"aggregation"`
	Measure               MeasureDefinition        `json:"measure"`
	Attribution           AttributionDefinition    `json:"attribution"`
	Freshness             FreshnessDefinition      `json:"freshness"`
	MaximumUncertaintyBPS uint16                   `json:"maximum_uncertainty_bps"`
	Sources               []SourceFamily           `json:"sources"`
	Reconciliation        ReconciliationDefinition `json:"reconciliation"`
	Baseline              Baseline                 `json:"baseline"`
	Thresholds            ThresholdDefinition      `json:"thresholds"`
	Guardrails            []MetricReference        `json:"guardrails"`
	AuthorSeatID          contracts.SeatID         `json:"author_seat_id"`
	RegisteredAt          time.Time                `json:"registered_at"`
	EffectiveAt           time.Time                `json:"effective_at"`
	ExpiresAt             time.Time                `json:"expires_at"`
	Supersedes            *MetricReference         `json:"supersedes"`
}

func (value MetricDefinitionBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("metric_id", string(value.ID)) != nil ||
		value.Version == 0 || validateToken("organization_id", string(value.OrganizationID)) != nil ||
		validateToken("initiative_id", value.InitiativeID) != nil ||
		validateText("metric name", value.Name, 512) != nil || !value.OutcomeKind.Valid() ||
		!value.Aggregation.Valid() || value.Measure.Validate() != nil ||
		value.Attribution.Validate() != nil || value.Freshness.Validate() != nil ||
		value.MaximumUncertaintyBPS > 10_000 || value.Reconciliation.Validate() != nil ||
		value.Baseline.Validate() != nil || validateToken("author_seat_id", string(value.AuthorSeatID)) != nil {
		return fmt.Errorf("business outcome: metric definition is invalid")
	}
	if len(value.Sources) == 0 || len(value.Sources) > 16 || !sortedUniqueFamilies(value.Sources) {
		return fmt.Errorf("business outcome: metric sources must be sorted and unique")
	}
	for _, source := range value.Sources {
		if !source.Valid() {
			return fmt.Errorf("business outcome: metric source is invalid")
		}
	}
	if value.OutcomeKind == OutcomeEconomic && !value.Reconciliation.Required {
		return fmt.Errorf("business outcome: economic metrics require reconciliation")
	}
	if value.OutcomeKind == OutcomeEconomic && !slices.ContainsFunc(value.Sources, SourceFamily.Financial) {
		return fmt.Errorf("business outcome: economic metrics require an authoritative financial source")
	}
	if value.Baseline.Value.Unit != value.Measure.Unit || value.Baseline.Value.Scale != value.Measure.Scale {
		return fmt.Errorf("business outcome: baseline is incompatible with metric identity")
	}
	if !validUTC(value.RegisteredAt) || !validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) ||
		value.EffectiveAt.Before(value.RegisteredAt) || !value.ExpiresAt.After(value.EffectiveAt) ||
		value.Baseline.ObservedAt.After(value.RegisteredAt) ||
		value.Thresholds.Validate(value.RegisteredAt) != nil ||
		value.Thresholds.ReviewAt.After(value.ExpiresAt) {
		return fmt.Errorf("business outcome: metric registration chronology is invalid")
	}
	if value.Version == 1 && value.Supersedes != nil || value.Version > 1 && value.Supersedes == nil {
		return fmt.Errorf("business outcome: metric version lineage is invalid")
	}
	if value.Supersedes != nil {
		if value.Supersedes.Validate() != nil || value.Supersedes.ID != value.ID ||
			value.Supersedes.Version+1 != value.Version {
			return fmt.Errorf("business outcome: metric supersession is invalid")
		}
	}
	if len(value.Guardrails) > 32 {
		return fmt.Errorf("business outcome: metric guardrail count exceeds bounds")
	}
	for index := range value.Guardrails {
		if value.Guardrails[index].Validate() != nil || value.Guardrails[index].ID == value.ID {
			return fmt.Errorf("business outcome: metric guardrail is invalid")
		}
		if index > 0 && metricReferenceKey(value.Guardrails[index-1]) >= metricReferenceKey(value.Guardrails[index]) {
			return fmt.Errorf("business outcome: metric guardrails must be sorted and unique")
		}
	}
	return nil
}

type MetricDefinition struct {
	Body        MetricDefinitionBody  `json:"body"`
	ContentHash contracts.ContentHash `json:"content_hash"`
	Signature   contracts.Signature   `json:"signature"`
}

func (value MetricDefinition) Validate() error {
	if value.Body.Validate() != nil || value.ContentHash.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("business outcome: signed metric definition is invalid")
	}
	return nil
}

func (value MetricDefinition) Reference() MetricReference {
	return MetricReference{ID: value.Body.ID, Version: value.Body.Version, DefinitionHash: value.ContentHash}
}

func (value MetricDefinition) ComparableWith(other MetricDefinition) bool {
	return value.Validate() == nil && other.Validate() == nil && value.ContentHash == other.ContentHash &&
		value.Body.ID == other.Body.ID && value.Body.Version == other.Body.Version &&
		value.Body.Measure == other.Body.Measure && value.Body.Attribution == other.Body.Attribution &&
		value.Body.Aggregation == other.Body.Aggregation && slices.Equal(value.Body.Sources, other.Body.Sources)
}

type SourceRef struct {
	Family            SourceFamily          `json:"family"`
	Authority         ObservationAuthority  `json:"authority"`
	RecordID          string                `json:"record_id"`
	EventID           string                `json:"event_id"`
	Hash              contracts.ContentHash `json:"hash"`
	Provider          string                `json:"provider"`
	Account           string                `json:"account"`
	ObjectRef         string                `json:"object_ref"`
	ConnectionID      string                `json:"connection_id"`
	ConnectionVersion uint64                `json:"connection_version"`
	Operation         string                `json:"operation"`
	IdempotencyKey    string                `json:"idempotency_key"`
	State             SourceState           `json:"state"`
	ObservedAt        time.Time             `json:"observed_at"`
}

func (value SourceRef) Validate() error {
	if !value.Family.Valid() || !value.Authority.Valid() || !value.State.Valid() ||
		validateToken("source record_id", value.RecordID) != nil ||
		validateToken("source event_id", value.EventID) != nil || value.Hash.Validate() != nil ||
		validateToken("source provider", value.Provider) != nil ||
		validateBounded("source account", value.Account, 512) != nil ||
		validateBounded("source object_ref", value.ObjectRef, 1024) != nil ||
		!validUTC(value.ObservedAt) {
		return fmt.Errorf("business outcome: source reference is invalid")
	}
	if value.ConnectionID == "" {
		if value.ConnectionVersion != 0 || value.Operation != "" || value.IdempotencyKey != "" {
			return fmt.Errorf("business outcome: source connection locator is partial")
		}
	} else if validateToken("source connection_id", value.ConnectionID) != nil ||
		value.ConnectionVersion == 0 || validateToken("source operation", value.Operation) != nil ||
		validateToken("source idempotency_key", value.IdempotencyKey) != nil {
		return fmt.Errorf("business outcome: source connection locator is invalid")
	}
	if value.Authority == AuthorityModelProposed && value.State != SourceProposed {
		return fmt.Errorf("business outcome: model authority is proposal-only")
	}
	if value.Authority == AuthorityReconciledFinancial && !value.Family.Financial() {
		return fmt.Errorf("business outcome: financial authority requires a financial source family")
	}
	return nil
}

type Valuation struct {
	Currency string                `json:"currency"`
	ValuedAt time.Time             `json:"valued_at"`
	Method   string                `json:"method"`
	Evidence contracts.EvidenceRef `json:"evidence"`
}

func (value Valuation) Validate() error {
	if validateToken("valuation currency", value.Currency) != nil ||
		value.Currency != strings.ToUpper(value.Currency) || len(value.Currency) > 12 ||
		!validUTC(value.ValuedAt) || validateToken("valuation method", value.Method) != nil ||
		value.Evidence.Validate() != nil || value.Evidence.ObservedAt != value.ValuedAt {
		return fmt.Errorf("business outcome: valuation is invalid")
	}
	return nil
}

type ReconciliationState string

const (
	ReconciliationNotRequired ReconciliationState = "not_required"
	ReconciliationPending     ReconciliationState = "pending"
	ReconciliationReconciled  ReconciliationState = "reconciled"
	ReconciliationConflicted  ReconciliationState = "conflicted"
)

func (value ReconciliationState) Valid() bool {
	switch value {
	case ReconciliationNotRequired, ReconciliationPending,
		ReconciliationReconciled, ReconciliationConflicted:
		return true
	default:
		return false
	}
}

type Reconciliation struct {
	State        ReconciliationState                `json:"state"`
	Procedure    contracts.VerificationProcedureRef `json:"procedure"`
	Independent  *SourceRef                         `json:"independent_source"`
	Valuation    *Valuation                         `json:"valuation"`
	ReconciledAt *time.Time                         `json:"reconciled_at"`
}

func (value Reconciliation) Validate(primary SourceRef) error {
	if !value.State.Valid() || value.Procedure.Validate() != nil {
		return fmt.Errorf("business outcome: reconciliation is invalid")
	}
	if value.State == ReconciliationNotRequired || value.State == ReconciliationPending {
		if value.Independent != nil || value.Valuation != nil || value.ReconciledAt != nil {
			return fmt.Errorf("business outcome: unresolved reconciliation carries resolution data")
		}
		return nil
	}
	if value.Independent == nil || value.Independent.Validate() != nil ||
		value.Independent.EventID == primary.EventID || value.Independent.Hash == primary.Hash ||
		value.Independent.Provider == primary.Provider || value.ReconciledAt == nil ||
		!validUTC(*value.ReconciledAt) || value.ReconciledAt.Before(primary.ObservedAt) {
		return fmt.Errorf("business outcome: reconciliation lacks an independent authority")
	}
	if value.State == ReconciliationReconciled {
		if value.Valuation == nil || value.Valuation.Validate() != nil {
			return fmt.Errorf("business outcome: reconciled observation lacks valuation")
		}
	} else if value.Valuation != nil && value.Valuation.Validate() != nil {
		return fmt.Errorf("business outcome: conflicted valuation is invalid")
	}
	return nil
}

type ObservationBody struct {
	SchemaVersion       string                   `json:"schema_version"`
	ID                  ObservationID            `json:"observation_id"`
	OrganizationID      contracts.OrganizationID `json:"organization_id"`
	InitiativeID        string                   `json:"initiative_id"`
	AuthorSeatID        contracts.SeatID         `json:"author_seat_id"`
	Metric              MetricReference          `json:"metric"`
	OutcomeKind         OutcomeKind              `json:"outcome_kind"`
	Status              MeasurementStatus        `json:"status"`
	Value               MeasurementValue         `json:"value"`
	SubjectRef          string                   `json:"subject_ref"`
	AttributionProof    contracts.EvidenceRef    `json:"attribution_proof"`
	Primary             SourceRef                `json:"primary"`
	Supporting          []SourceRef              `json:"supporting"`
	Reconciliation      Reconciliation           `json:"reconciliation"`
	ObservedAt          time.Time                `json:"observed_at"`
	CapturedAt          time.Time                `json:"captured_at"`
	FreshUntil          time.Time                `json:"fresh_until"`
	UncertaintyBPS      uint16                   `json:"uncertainty_bps"`
	KnownGaps           []string                 `json:"known_gaps"`
	ConflictingEvidence []contracts.ContentHash  `json:"conflicting_evidence"`
	Supersedes          *ObservationID           `json:"supersedes"`
}

func (value ObservationBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("observation_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil ||
		validateToken("initiative_id", value.InitiativeID) != nil ||
		validateToken("author_seat_id", string(value.AuthorSeatID)) != nil ||
		value.Metric.Validate() != nil || !value.OutcomeKind.Valid() || !value.Status.Valid() ||
		value.Value.Validate() != nil || validateToken("subject_ref", value.SubjectRef) != nil ||
		value.AttributionProof.Validate() != nil || value.Primary.Validate() != nil ||
		value.Reconciliation.Validate(value.Primary) != nil || value.UncertaintyBPS > 10_000 {
		return fmt.Errorf("business outcome: observation is invalid")
	}
	if !validUTC(value.ObservedAt) || !validUTC(value.CapturedAt) || !validUTC(value.FreshUntil) ||
		value.ObservedAt != value.Primary.ObservedAt || value.CapturedAt.Before(value.ObservedAt) ||
		!value.FreshUntil.After(value.CapturedAt) ||
		value.AttributionProof.ObservedAt.After(value.CapturedAt) {
		return fmt.Errorf("business outcome: observation chronology is invalid")
	}
	if value.Status == MeasurementProposed && value.Primary.Authority != AuthorityModelProposed ||
		value.Primary.Authority == AuthorityModelProposed && value.Status != MeasurementProposed {
		return fmt.Errorf("business outcome: proposal status and authority disagree")
	}
	if value.Status == MeasurementReconciled && value.Reconciliation.State != ReconciliationReconciled {
		return fmt.Errorf("business outcome: reconciled status lacks reconciliation")
	}
	if len(value.Supporting) > 64 || len(value.KnownGaps) > 64 || len(value.ConflictingEvidence) > 64 {
		return fmt.Errorf("business outcome: observation evidence exceeds bounds")
	}
	for index := range value.Supporting {
		if value.Supporting[index].Validate() != nil || value.Supporting[index].EventID == value.Primary.EventID {
			return fmt.Errorf("business outcome: supporting source is invalid")
		}
		if index > 0 && sourceKey(value.Supporting[index-1]) >= sourceKey(value.Supporting[index]) {
			return fmt.Errorf("business outcome: supporting sources must be sorted and unique")
		}
	}
	if value.Reconciliation.Independent != nil &&
		!slices.Contains(value.Supporting, *value.Reconciliation.Independent) {
		return fmt.Errorf("business outcome: reconciliation authority is not preserved in supporting lineage")
	}
	if !sortedUniqueStrings(value.KnownGaps) {
		return fmt.Errorf("business outcome: observation gaps must be sorted and unique")
	}
	previous := ""
	for _, hash := range value.ConflictingEvidence {
		if hash.Validate() != nil || previous != "" && hash.Digest <= previous {
			return fmt.Errorf("business outcome: conflicting evidence must be sorted and unique")
		}
		previous = hash.Digest
	}
	if value.Supersedes != nil {
		if validateToken("superseded observation", string(*value.Supersedes)) != nil || *value.Supersedes == value.ID {
			return fmt.Errorf("business outcome: observation supersession is invalid")
		}
	}
	return nil
}

type Observation struct {
	Body        ObservationBody       `json:"body"`
	ContentHash contracts.ContentHash `json:"content_hash"`
	Signature   contracts.Signature   `json:"signature"`
}

func (value Observation) Validate() error {
	if value.Body.Validate() != nil || value.ContentHash.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("business outcome: signed observation is invalid")
	}
	return nil
}

func (value Observation) GateSafe(definition MetricDefinition, now time.Time) error {
	if value.Validate() != nil || definition.Validate() != nil || !validUTC(now) {
		return fmt.Errorf("business outcome: observation or metric definition is invalid")
	}
	body := value.Body
	metric := definition.Body
	if body.Metric != definition.Reference() || body.OrganizationID != metric.OrganizationID ||
		body.InitiativeID != metric.InitiativeID || body.OutcomeKind != metric.OutcomeKind ||
		body.Value.Unit != metric.Measure.Unit || body.Value.Scale != metric.Measure.Scale ||
		body.SubjectRef != metric.Attribution.Subject {
		return fmt.Errorf("business outcome: observation is incompatible with current metric identity")
	}
	if now.Before(metric.EffectiveAt) || !metric.ExpiresAt.After(now) ||
		!body.FreshUntil.After(now) || now.Sub(body.ObservedAt) > metric.Freshness.MaximumObservationAge ||
		body.CapturedAt.Sub(body.ObservedAt) > metric.Freshness.MaximumCaptureDelay {
		return fmt.Errorf("business outcome: observation or metric definition is stale")
	}
	if body.Status != MeasurementObserved && body.Status != MeasurementReconciled ||
		body.Primary.State != SourceCompleted && body.Primary.State != SourceReconciled ||
		body.Primary.Authority == AuthorityModelProposed || len(body.KnownGaps) != 0 ||
		len(body.ConflictingEvidence) != 0 || body.UncertaintyBPS > metric.MaximumUncertaintyBPS {
		return fmt.Errorf("business outcome: observation is proposed, pending, contested, uncertain, or incomplete")
	}
	if !slices.Contains(metric.Sources, body.Primary.Family) {
		return fmt.Errorf("business outcome: observation source is incompatible with metric definition")
	}
	if metric.Reconciliation.Required {
		if body.Status != MeasurementReconciled || body.Reconciliation.State != ReconciliationReconciled ||
			body.Reconciliation.Independent == nil || body.Reconciliation.Valuation == nil ||
			!slices.Contains(metric.Reconciliation.IndependentFamilies, body.Reconciliation.Independent.Family) {
			return fmt.Errorf("business outcome: observation is unreconciled")
		}
		if absoluteDuration(body.Reconciliation.Valuation.ValuedAt.Sub(body.ObservedAt)) > metric.Reconciliation.MaximumValuationSkew {
			return fmt.Errorf("business outcome: observation valuation time is incompatible")
		}
	}
	switch metric.OutcomeKind {
	case OutcomeActivity, OutcomeOutput:
		if body.Primary.Authority != AuthorityInternalVerified && body.Primary.Authority != AuthorityProviderReported {
			return fmt.Errorf("business outcome: activity or output lacks verified execution authority")
		}
	case OutcomeCustomer:
		if body.Primary.Authority != AuthorityCustomerReported && body.Primary.Authority != AuthorityProviderReported {
			return fmt.Errorf("business outcome: customer outcome lacks customer or provider authority")
		}
	case OutcomeCommercial:
		if body.Primary.Authority != AuthorityProviderReported && body.Primary.Authority != AuthorityReconciledFinancial {
			return fmt.Errorf("business outcome: commercial outcome lacks provider authority")
		}
	case OutcomeEconomic:
		if body.Primary.Authority != AuthorityReconciledFinancial || !body.Primary.Family.Financial() {
			return fmt.Errorf("business outcome: economic outcome lacks reconciled financial authority")
		}
	case OutcomeRisk:
		if body.Primary.Authority != AuthorityInternalVerified &&
			body.Primary.Authority != AuthorityProviderReported &&
			body.Primary.Authority != AuthorityReconciledFinancial &&
			(body.Primary.Authority != AuthorityAnalyticallyDerived || len(body.Supporting) == 0) {
			return fmt.Errorf("business outcome: risk outcome lacks authoritative risk evidence")
		}
	case OutcomeStrategicLearning:
		if body.Primary.Authority != AuthorityAnalyticallyDerived || len(body.Supporting) == 0 {
			return fmt.Errorf("business outcome: strategic learning lacks authoritative derivation inputs")
		}
	}
	return nil
}

type ObservationBinding struct {
	ID   ObservationID         `json:"observation_id"`
	Hash contracts.ContentHash `json:"hash"`
}

func (value ObservationBinding) Validate() error {
	if validateToken("observation_id", string(value.ID)) != nil || value.Hash.Validate() != nil {
		return fmt.Errorf("business outcome: observation binding is invalid")
	}
	return nil
}

type ThresholdResult string

const (
	ThresholdSuccess ThresholdResult = "success"
	ThresholdStop    ThresholdResult = "stop"
	ThresholdNeither ThresholdResult = "neither"
)

func (value ThresholdResult) Valid() bool {
	return value == ThresholdSuccess || value == ThresholdStop || value == ThresholdNeither
}

type OutcomeBody struct {
	SchemaVersion   string                             `json:"schema_version"`
	ID              OutcomeID                          `json:"outcome_id"`
	ChainID         OutcomeChainID                     `json:"chain_id"`
	Version         uint64                             `json:"version"`
	OrganizationID  contracts.OrganizationID           `json:"organization_id"`
	InitiativeID    string                             `json:"initiative_id"`
	Metric          MetricReference                    `json:"metric"`
	Kind            OutcomeKind                        `json:"kind"`
	Observations    []ObservationBinding               `json:"observations"`
	Value           MeasurementValue                   `json:"value"`
	ThresholdResult ThresholdResult                    `json:"threshold_result"`
	Derivation      contracts.VerificationProcedureRef `json:"derivation"`
	AuthorSeatID    contracts.SeatID                   `json:"author_seat_id"`
	DerivedAt       time.Time                          `json:"derived_at"`
	FreshUntil      time.Time                          `json:"fresh_until"`
	Supersedes      *OutcomeID                         `json:"supersedes"`
}

func (value OutcomeBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("outcome_id", string(value.ID)) != nil ||
		validateToken("outcome chain_id", string(value.ChainID)) != nil || value.Version == 0 ||
		validateToken("organization_id", string(value.OrganizationID)) != nil ||
		validateToken("initiative_id", value.InitiativeID) != nil || value.Metric.Validate() != nil ||
		!value.Kind.Valid() || value.Value.Validate() != nil || !value.ThresholdResult.Valid() ||
		value.Derivation.Validate() != nil || validateToken("author_seat_id", string(value.AuthorSeatID)) != nil ||
		!validUTC(value.DerivedAt) || !validUTC(value.FreshUntil) || !value.FreshUntil.After(value.DerivedAt) {
		return fmt.Errorf("business outcome: outcome body is invalid")
	}
	if len(value.Observations) == 0 || len(value.Observations) > 1024 {
		return fmt.Errorf("business outcome: outcome requires bounded observations")
	}
	for index := range value.Observations {
		if value.Observations[index].Validate() != nil ||
			index > 0 && observationBindingKey(value.Observations[index-1]) >= observationBindingKey(value.Observations[index]) {
			return fmt.Errorf("business outcome: outcome observations must be sorted and unique")
		}
	}
	if value.Version == 1 && value.Supersedes != nil || value.Version > 1 && value.Supersedes == nil {
		return fmt.Errorf("business outcome: outcome version lineage is invalid")
	}
	if value.Supersedes != nil && (validateToken("superseded outcome", string(*value.Supersedes)) != nil || *value.Supersedes == value.ID) {
		return fmt.Errorf("business outcome: outcome supersession is invalid")
	}
	return nil
}

type OutcomeRecord struct {
	Body      OutcomeBody         `json:"body"`
	Signature contracts.Signature `json:"signature"`
}

func (value OutcomeRecord) Validate() error {
	if value.Body.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("business outcome: signed outcome record is invalid")
	}
	return nil
}

type IndependentReview struct {
	SchemaVersion  string                             `json:"schema_version"`
	ID             contracts.VerdictID                `json:"verdict_id"`
	OutcomeID      OutcomeID                          `json:"outcome_id"`
	OutcomeHash    contracts.ContentHash              `json:"outcome_hash"`
	AuthorSeatID   contracts.SeatID                   `json:"author_seat_id"`
	VerifierSeatID contracts.SeatID                   `json:"verifier_seat_id"`
	Procedure      contracts.VerificationProcedureRef `json:"procedure"`
	Observations   []ObservationBinding               `json:"observations"`
	Disposition    contracts.VerdictOutcome           `json:"disposition"`
	Findings       []string                           `json:"findings"`
	VerifiedAt     time.Time                          `json:"verified_at"`
	ExpiresAt      time.Time                          `json:"expires_at"`
	Signature      contracts.Signature                `json:"signature"`
}

func (value IndependentReview) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("verdict_id", string(value.ID)) != nil ||
		validateToken("outcome_id", string(value.OutcomeID)) != nil || value.OutcomeHash.Validate() != nil ||
		validateToken("author_seat_id", string(value.AuthorSeatID)) != nil ||
		validateToken("verifier_seat_id", string(value.VerifierSeatID)) != nil ||
		value.AuthorSeatID == value.VerifierSeatID || value.Procedure.Validate() != nil ||
		!value.Disposition.Valid() || !validUTC(value.VerifiedAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.VerifiedAt) || value.Signature.Validate() != nil ||
		len(value.Observations) == 0 || len(value.Observations) > 1024 || len(value.Findings) > 128 {
		return fmt.Errorf("business outcome: independent review is invalid")
	}
	for index := range value.Observations {
		if value.Observations[index].Validate() != nil ||
			index > 0 && observationBindingKey(value.Observations[index-1]) >= observationBindingKey(value.Observations[index]) {
			return fmt.Errorf("business outcome: review observation bindings are invalid")
		}
	}
	for _, finding := range value.Findings {
		if validateText("review finding", finding, 4096) != nil {
			return fmt.Errorf("business outcome: review finding is invalid")
		}
	}
	return nil
}

type VerifiedOutcome struct {
	Record OutcomeRecord     `json:"record"`
	Review IndependentReview `json:"review"`
}

func (value VerifiedOutcome) Validate() error {
	return value.ValidateAt(value.Review.VerifiedAt)
}

func (value VerifiedOutcome) ValidateAt(now time.Time) error {
	if value.Record.Validate() != nil || value.Review.Validate() != nil || !validUTC(now) {
		return fmt.Errorf("business outcome: verified outcome is invalid")
	}
	body := value.Record.Body
	hash, err := OutcomeRecordHash(value.Record)
	if err != nil || value.Review.Disposition != contracts.VerdictPass ||
		value.Review.OutcomeID != body.ID || value.Review.OutcomeHash != hash ||
		value.Review.AuthorSeatID != body.AuthorSeatID ||
		!slices.Equal(value.Review.Observations, body.Observations) ||
		value.Review.VerifiedAt.Before(body.DerivedAt) || !body.FreshUntil.After(now) ||
		!value.Review.ExpiresAt.After(now) || value.Review.ExpiresAt.After(body.FreshUntil) {
		return fmt.Errorf("business outcome: outcome lacks a current binding independent review")
	}
	return nil
}

type GatePurpose string

const (
	GateBusinessSuccess     GatePurpose = "business_success"
	GateLifecycleTransition GatePurpose = "lifecycle_transition"
	GateRiskGuardrail       GatePurpose = "risk_guardrail"
	GateLearningReview      GatePurpose = "learning_review"
)

func (value GatePurpose) Valid() bool {
	return value == GateBusinessSuccess || value == GateLifecycleTransition ||
		value == GateRiskGuardrail || value == GateLearningReview
}

type GateRequirement struct {
	SchemaVersion         string                   `json:"schema_version"`
	ID                    GateID                   `json:"gate_id"`
	OrganizationID        contracts.OrganizationID `json:"organization_id"`
	InitiativeID          string                   `json:"initiative_id"`
	Purpose               GatePurpose              `json:"purpose"`
	OutcomeID             OutcomeID                `json:"outcome_id"`
	Metric                MetricReference          `json:"metric"`
	OutcomeKind           OutcomeKind              `json:"outcome_kind"`
	ExpectedResult        ThresholdResult          `json:"expected_result"`
	MinimumDenominator    uint64                   `json:"minimum_denominator"`
	MaximumUncertaintyBPS uint16                   `json:"maximum_uncertainty_bps"`
	RequiredSources       []SourceFamily           `json:"required_sources"`
	PreregisteredAt       time.Time                `json:"preregistered_at"`
	EvaluateAt            time.Time                `json:"evaluate_at"`
}

func (value GateRequirement) Validate() error {
	if value.SchemaVersion != GateSchemaVersion || validateToken("gate_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil ||
		validateToken("initiative_id", value.InitiativeID) != nil || !value.Purpose.Valid() ||
		validateToken("outcome_id", string(value.OutcomeID)) != nil || value.Metric.Validate() != nil ||
		!value.OutcomeKind.Valid() || !value.ExpectedResult.Valid() || value.ExpectedResult == ThresholdNeither ||
		value.MinimumDenominator == 0 || value.MaximumUncertaintyBPS > 10_000 ||
		len(value.RequiredSources) == 0 || len(value.RequiredSources) > 16 ||
		!sortedUniqueFamilies(value.RequiredSources) || !validUTC(value.PreregisteredAt) ||
		!validUTC(value.EvaluateAt) || !value.EvaluateAt.After(value.PreregisteredAt) {
		return fmt.Errorf("business outcome: gate requirement is invalid")
	}
	for _, source := range value.RequiredSources {
		if !source.Valid() {
			return fmt.Errorf("business outcome: gate requires an invalid source")
		}
	}
	if value.Purpose == GateBusinessSuccess && !value.OutcomeKind.BusinessOutcome() {
		return fmt.Errorf("business outcome: activity and output cannot satisfy a business-success gate")
	}
	return nil
}

type GateState string

const (
	GateSatisfied GateState = "satisfied"
	GateOpen      GateState = "open"
	GateBlocked   GateState = "blocked"
)

func (value GateState) Valid() bool {
	return value == GateSatisfied || value == GateOpen || value == GateBlocked
}

type GateDecision struct {
	SchemaVersion string                `json:"schema_version"`
	Requirement   GateRequirement       `json:"requirement"`
	State         GateState             `json:"state"`
	OutcomeHash   contracts.ContentHash `json:"outcome_hash"`
	Observations  []ObservationBinding  `json:"observations"`
	Guardrails    []RecordPointer       `json:"guardrails"`
	Reasons       []string              `json:"reasons"`
	EvaluatedAt   time.Time             `json:"evaluated_at"`
	DecisionHash  contracts.ContentHash `json:"decision_hash"`
}

func (value GateDecision) Validate() error {
	if value.SchemaVersion != GateSchemaVersion || value.Requirement.Validate() != nil ||
		!value.State.Valid() || value.OutcomeHash.Validate() != nil ||
		len(value.Observations) == 0 || len(value.Observations) > 1024 ||
		len(value.Reasons) == 0 || len(value.Reasons) > 64 || !validUTC(value.EvaluatedAt) ||
		value.DecisionHash.Validate() != nil {
		return fmt.Errorf("business outcome: gate decision is invalid")
	}
	for index := range value.Observations {
		if value.Observations[index].Validate() != nil ||
			index > 0 && observationBindingKey(value.Observations[index-1]) >= observationBindingKey(value.Observations[index]) {
			return fmt.Errorf("business outcome: gate observation bindings are invalid")
		}
	}
	if !sortedUniqueStrings(value.Reasons) {
		return fmt.Errorf("business outcome: gate reasons must be sorted and unique")
	}
	if len(value.Guardrails) > 32 {
		return fmt.Errorf("business outcome: gate guardrails exceed bounds")
	}
	for index := range value.Guardrails {
		if value.Guardrails[index].Validate() != nil || value.Guardrails[index].Kind != "outcome" ||
			index > 0 && recordPointerKey(value.Guardrails[index-1]) >= recordPointerKey(value.Guardrails[index]) {
			return fmt.Errorf("business outcome: gate guardrail lineage is invalid")
		}
	}
	expected, err := gateDecisionHash(value)
	if err != nil || expected != value.DecisionHash {
		return fmt.Errorf("business outcome: gate decision hash is invalid")
	}
	return nil
}

type RecordPointer struct {
	Kind string                `json:"kind"`
	ID   string                `json:"id"`
	Hash contracts.ContentHash `json:"hash"`
}

func (value RecordPointer) Validate() error {
	if validateToken("record pointer kind", value.Kind) != nil ||
		validateToken("record pointer id", value.ID) != nil || value.Hash.Validate() != nil {
		return fmt.Errorf("business outcome: record pointer is invalid")
	}
	return nil
}

type LineageEdge struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             LineageEdgeID            `json:"lineage_edge_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   string                   `json:"initiative_id"`
	Source         RecordPointer            `json:"source"`
	Consumer       RecordPointer            `json:"consumer"`
	Relation       string                   `json:"relation"`
	Material       bool                     `json:"material"`
	AuthorSeatID   contracts.SeatID         `json:"author_seat_id"`
	CreatedAt      time.Time                `json:"created_at"`
	Signature      contracts.Signature      `json:"signature"`
}

func (value LineageEdge) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("lineage_edge_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil ||
		validateToken("initiative_id", value.InitiativeID) != nil || value.Source.Validate() != nil ||
		value.Consumer.Validate() != nil || value.Source == value.Consumer ||
		validateToken("lineage relation", value.Relation) != nil ||
		validateToken("author_seat_id", string(value.AuthorSeatID)) != nil ||
		!validUTC(value.CreatedAt) || value.Signature.Validate() != nil {
		return fmt.Errorf("business outcome: lineage edge is invalid")
	}
	return nil
}

type CorrectionBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             CorrectionID             `json:"correction_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   string                   `json:"initiative_id"`
	Target         RecordPointer            `json:"target"`
	Replacement    *RecordPointer           `json:"replacement"`
	Reason         string                   `json:"reason"`
	Evidence       []contracts.EvidenceRef  `json:"evidence"`
	Material       bool                     `json:"material"`
	AuthorSeatID   contracts.SeatID         `json:"author_seat_id"`
	EffectiveAt    time.Time                `json:"effective_at"`
}

func (value CorrectionBody) Validate() error {
	if value.SchemaVersion != CorrectionSchemaVersion || validateToken("correction_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil ||
		validateToken("initiative_id", value.InitiativeID) != nil || value.Target.Validate() != nil ||
		validateText("correction reason", value.Reason, 4096) != nil ||
		len(value.Evidence) == 0 || len(value.Evidence) > 128 ||
		validateToken("author_seat_id", string(value.AuthorSeatID)) != nil || !validUTC(value.EffectiveAt) {
		return fmt.Errorf("business outcome: correction is invalid")
	}
	if value.Replacement != nil && (value.Replacement.Validate() != nil || *value.Replacement == value.Target) {
		return fmt.Errorf("business outcome: correction replacement is invalid")
	}
	for index := range value.Evidence {
		if value.Evidence[index].Validate() != nil ||
			index > 0 && evidenceKey(value.Evidence[index-1]) >= evidenceKey(value.Evidence[index]) {
			return fmt.Errorf("business outcome: correction evidence must be sorted and unique")
		}
	}
	return nil
}

type Correction struct {
	Body        CorrectionBody        `json:"body"`
	ContentHash contracts.ContentHash `json:"content_hash"`
	Signature   contracts.Signature   `json:"signature"`
}

func (value Correction) Validate() error {
	if value.Body.Validate() != nil || value.ContentHash.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("business outcome: signed correction is invalid")
	}
	return nil
}

type CorrectionResolutionBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"resolution_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	CorrectionID   CorrectionID             `json:"correction_id"`
	Replacement    RecordPointer            `json:"replacement"`
	Evidence       []contracts.EvidenceRef  `json:"evidence"`
	AuthorSeatID   contracts.SeatID         `json:"author_seat_id"`
	ResolvedAt     time.Time                `json:"resolved_at"`
}

func (value CorrectionResolutionBody) Validate() error {
	if value.SchemaVersion != CorrectionSchemaVersion || validateToken("resolution_id", value.ID) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil ||
		validateToken("correction_id", string(value.CorrectionID)) != nil || value.Replacement.Validate() != nil ||
		len(value.Evidence) == 0 || len(value.Evidence) > 128 ||
		validateToken("author_seat_id", string(value.AuthorSeatID)) != nil || !validUTC(value.ResolvedAt) {
		return fmt.Errorf("business outcome: correction resolution is invalid")
	}
	for index := range value.Evidence {
		if value.Evidence[index].Validate() != nil ||
			index > 0 && evidenceKey(value.Evidence[index-1]) >= evidenceKey(value.Evidence[index]) {
			return fmt.Errorf("business outcome: resolution evidence must be sorted and unique")
		}
	}
	return nil
}

type CorrectionResolution struct {
	Body        CorrectionResolutionBody `json:"body"`
	ContentHash contracts.ContentHash    `json:"content_hash"`
	Signature   contracts.Signature      `json:"signature"`
}

func (value CorrectionResolution) Validate() error {
	if value.Body.Validate() != nil || value.ContentHash.Validate() != nil || value.Signature.Validate() != nil {
		return fmt.Errorf("business outcome: signed correction resolution is invalid")
	}
	return nil
}

func SignMetricDefinition(value *MetricDefinition, keyID string, privateKey ed25519.PrivateKey) error {
	return signBody(value, keyID, privateKey, func(item *MetricDefinition, hash contracts.ContentHash, signature contracts.Signature) {
		item.ContentHash, item.Signature = hash, signature
	}, func(item MetricDefinition) MetricDefinitionBody { return item.Body })
}

func VerifyMetricDefinition(value MetricDefinition, publicKey ed25519.PublicKey) error {
	if value.Validate() != nil {
		return fmt.Errorf("business outcome: metric definition is invalid")
	}
	expected, err := contracts.HashCanonical(&value.Body)
	if err != nil || expected != value.ContentHash {
		return fmt.Errorf("business outcome: metric content hash mismatch")
	}
	prepared := value
	prepared.Signature = signingShape(value.Signature.KeyID)
	payload, err := contracts.EncodeCanonical(&prepared)
	if err != nil {
		return err
	}
	return verifySignature(payload, value.Signature, publicKey)
}

func SignObservation(value *Observation, keyID string, privateKey ed25519.PrivateKey) error {
	return signBody(value, keyID, privateKey, func(item *Observation, hash contracts.ContentHash, signature contracts.Signature) {
		item.ContentHash, item.Signature = hash, signature
	}, func(item Observation) ObservationBody { return item.Body })
}

func VerifyObservation(value Observation, publicKey ed25519.PublicKey) error {
	if value.Validate() != nil {
		return fmt.Errorf("business outcome: observation is invalid")
	}
	expected, err := contracts.HashCanonical(&value.Body)
	if err != nil || expected != value.ContentHash {
		return fmt.Errorf("business outcome: observation content hash mismatch")
	}
	prepared := value
	prepared.Signature = signingShape(value.Signature.KeyID)
	payload, err := contracts.EncodeCanonical(&prepared)
	if err != nil {
		return err
	}
	return verifySignature(payload, value.Signature, publicKey)
}

func SignOutcomeRecord(value *OutcomeRecord, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil || len(privateKey) != ed25519.PrivateKeySize || validateToken("outcome key_id", keyID) != nil || value.Body.Validate() != nil {
		return fmt.Errorf("business outcome: outcome and Ed25519 signing authority are required")
	}
	payload, err := contracts.EncodeCanonical(&value.Body)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{Algorithm: "ed25519", KeyID: keyID, Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))}
	return value.Validate()
}

func VerifyOutcomeRecord(value OutcomeRecord, publicKey ed25519.PublicKey) error {
	if value.Validate() != nil {
		return fmt.Errorf("business outcome: outcome record is invalid")
	}
	payload, err := contracts.EncodeCanonical(&value.Body)
	if err != nil {
		return err
	}
	return verifySignature(payload, value.Signature, publicKey)
}

func SignIndependentReview(value *IndependentReview, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil || len(privateKey) != ed25519.PrivateKeySize || validateToken("review key_id", keyID) != nil {
		return fmt.Errorf("business outcome: review and Ed25519 signing authority are required")
	}
	value.Signature = signingShape(keyID)
	payload, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	value.Signature.Value = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return value.Validate()
}

func VerifyIndependentReview(value IndependentReview, publicKey ed25519.PublicKey) error {
	if value.Validate() != nil {
		return fmt.Errorf("business outcome: independent review is invalid")
	}
	prepared := value
	prepared.Signature = signingShape(value.Signature.KeyID)
	payload, err := contracts.EncodeCanonical(&prepared)
	if err != nil {
		return err
	}
	return verifySignature(payload, value.Signature, publicKey)
}

func SignLineageEdge(value *LineageEdge, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil || len(privateKey) != ed25519.PrivateKeySize || validateToken("lineage key_id", keyID) != nil {
		return fmt.Errorf("business outcome: lineage edge and Ed25519 signing authority are required")
	}
	value.Signature = signingShape(keyID)
	payload, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	value.Signature.Value = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return value.Validate()
}

func VerifyLineageEdge(value LineageEdge, publicKey ed25519.PublicKey) error {
	if value.Validate() != nil {
		return fmt.Errorf("business outcome: lineage edge is invalid")
	}
	prepared := value
	prepared.Signature = signingShape(value.Signature.KeyID)
	payload, err := contracts.EncodeCanonical(&prepared)
	if err != nil {
		return err
	}
	return verifySignature(payload, value.Signature, publicKey)
}

func SignCorrection(value *Correction, keyID string, privateKey ed25519.PrivateKey) error {
	return signBody(value, keyID, privateKey, func(item *Correction, hash contracts.ContentHash, signature contracts.Signature) {
		item.ContentHash, item.Signature = hash, signature
	}, func(item Correction) CorrectionBody { return item.Body })
}

func VerifyCorrection(value Correction, publicKey ed25519.PublicKey) error {
	if value.Validate() != nil {
		return fmt.Errorf("business outcome: correction is invalid")
	}
	expected, err := contracts.HashCanonical(&value.Body)
	if err != nil || expected != value.ContentHash {
		return fmt.Errorf("business outcome: correction content hash mismatch")
	}
	prepared := value
	prepared.Signature = signingShape(value.Signature.KeyID)
	payload, err := contracts.EncodeCanonical(&prepared)
	if err != nil {
		return err
	}
	return verifySignature(payload, value.Signature, publicKey)
}

func SignCorrectionResolution(value *CorrectionResolution, keyID string, privateKey ed25519.PrivateKey) error {
	return signBody(value, keyID, privateKey, func(item *CorrectionResolution, hash contracts.ContentHash, signature contracts.Signature) {
		item.ContentHash, item.Signature = hash, signature
	}, func(item CorrectionResolution) CorrectionResolutionBody { return item.Body })
}

func VerifyCorrectionResolution(value CorrectionResolution, publicKey ed25519.PublicKey) error {
	if value.Validate() != nil {
		return fmt.Errorf("business outcome: correction resolution is invalid")
	}
	expected, err := contracts.HashCanonical(&value.Body)
	if err != nil || expected != value.ContentHash {
		return fmt.Errorf("business outcome: correction resolution content hash mismatch")
	}
	prepared := value
	prepared.Signature = signingShape(value.Signature.KeyID)
	payload, err := contracts.EncodeCanonical(&prepared)
	if err != nil {
		return err
	}
	return verifySignature(payload, value.Signature, publicKey)
}

func OutcomeRecordHash(value OutcomeRecord) (contracts.ContentHash, error) {
	if value.Validate() != nil {
		return contracts.ContentHash{}, fmt.Errorf("business outcome: outcome record is invalid")
	}
	return contracts.HashCanonical(&value.Body)
}

func AggregateValues(aggregation Aggregation, values []MeasurementValue) (MeasurementValue, error) {
	if !aggregation.Valid() || len(values) == 0 || len(values) > 1024 {
		return MeasurementValue{}, fmt.Errorf("business outcome: aggregation input is invalid")
	}
	unit, scale := values[0].Unit, values[0].Scale
	for _, value := range values {
		if value.Validate() != nil || value.Unit != unit || value.Scale != scale {
			return MeasurementValue{}, fmt.Errorf("business outcome: aggregation values are incompatible")
		}
	}
	switch aggregation {
	case AggregationLatest:
		return values[len(values)-1], nil
	case AggregationMinimum, AggregationMaximum:
		result := values[0]
		for _, value := range values[1:] {
			if aggregation == AggregationMinimum && value.ValueMicros < result.ValueMicros ||
				aggregation == AggregationMaximum && value.ValueMicros > result.ValueMicros {
				result = value
			}
		}
		return result, nil
	case AggregationSum:
		if slices.ContainsFunc(values, func(value MeasurementValue) bool { return value.Denominator != 1 }) {
			return MeasurementValue{}, fmt.Errorf("business outcome: sum aggregation requires unit denominators")
		}
		total := big.NewInt(0)
		for _, value := range values {
			total.Add(total, big.NewInt(value.NumeratorMicros))
		}
		if !total.IsInt64() {
			return MeasurementValue{}, fmt.Errorf("business outcome: aggregate overflows int64")
		}
		result := MeasurementValue{NumeratorMicros: total.Int64(), Denominator: 1, Scale: scale, Unit: unit}
		computed, err := quotient(result.NumeratorMicros, result.Denominator, result.Scale)
		result.ValueMicros = computed
		return result, err
	case AggregationRate:
		numerator := big.NewInt(0)
		denominator := new(big.Int)
		for _, value := range values {
			numerator.Add(numerator, big.NewInt(value.NumeratorMicros))
			denominator.Add(denominator, new(big.Int).SetUint64(value.Denominator))
		}
		if !numerator.IsInt64() || !denominator.IsUint64() || denominator.Sign() <= 0 {
			return MeasurementValue{}, fmt.Errorf("business outcome: rate aggregation overflows")
		}
		result := MeasurementValue{NumeratorMicros: numerator.Int64(), Denominator: denominator.Uint64(), Scale: scale, Unit: unit}
		computed, err := quotient(result.NumeratorMicros, result.Denominator, result.Scale)
		result.ValueMicros = computed
		return result, err
	default:
		return MeasurementValue{}, fmt.Errorf("business outcome: unsupported aggregation")
	}
}

func ThresholdFor(definition MetricDefinitionBody, value MeasurementValue) ThresholdResult {
	if value.Denominator >= definition.Thresholds.Stop.MinimumDenominator &&
		definition.Thresholds.Stop.Comparator.Match(value.ValueMicros, definition.Thresholds.Stop.ValueMicros) {
		return ThresholdStop
	}
	if value.Denominator >= definition.Thresholds.Success.MinimumDenominator &&
		definition.Thresholds.Success.Comparator.Match(value.ValueMicros, definition.Thresholds.Success.ValueMicros) {
		return ThresholdSuccess
	}
	return ThresholdNeither
}

func signBody[T any, B contracts.Validatable](value *T, keyID string, privateKey ed25519.PrivateKey, assign func(*T, contracts.ContentHash, contracts.Signature), body func(T) B) error {
	if value == nil || len(privateKey) != ed25519.PrivateKeySize || validateToken("signature key_id", keyID) != nil {
		return fmt.Errorf("business outcome: record and Ed25519 signing authority are required")
	}
	hash, err := contracts.HashCanonical(body(*value))
	if err != nil {
		return err
	}
	assign(value, hash, signingShape(keyID))
	payload, err := contracts.EncodeCanonical(valueAsValidatable(value))
	if err != nil {
		return err
	}
	signature := signingShape(keyID)
	signature.Value = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	assign(value, hash, signature)
	return valueAsValidatable(value).Validate()
}

func signingShape(keyID string) contracts.Signature {
	return contracts.Signature{Algorithm: "ed25519", KeyID: keyID, Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))}
}

func verifySignature(payload []byte, signature contracts.Signature, publicKey ed25519.PublicKey) error {
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || len(decoded) != ed25519.SignatureSize ||
		!ed25519.Verify(publicKey, payload, decoded) {
		return fmt.Errorf("business outcome: Ed25519 signature is invalid")
	}
	return nil
}

func valueAsValidatable[T any](value *T) interface{ Validate() error } {
	return any(value).(interface{ Validate() error })
}

func quotient(numerator int64, denominator uint64, scale uint8) (int64, error) {
	if denominator == 0 || scale > 12 {
		return 0, fmt.Errorf("business outcome: quotient input is invalid")
	}
	value := new(big.Int).Mul(big.NewInt(numerator), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
	value.Quo(value, new(big.Int).SetUint64(denominator))
	if !value.IsInt64() {
		return 0, fmt.Errorf("business outcome: quotient overflows int64")
	}
	return value.Int64(), nil
}

func numeratorForValue(value int64, denominator uint64, scale uint8) (int64, error) {
	if denominator == 0 || scale > 12 {
		return 0, fmt.Errorf("business outcome: inverse quotient input is invalid")
	}
	numerator := new(big.Int).Mul(big.NewInt(value), new(big.Int).SetUint64(denominator))
	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	remainder := new(big.Int)
	numerator.QuoRem(numerator, divisor, remainder)
	if remainder.Sign() != 0 || !numerator.IsInt64() {
		return 0, fmt.Errorf("business outcome: value cannot be represented by the declared numerator and scale")
	}
	return numerator.Int64(), nil
}

func gateDecisionHash(value GateDecision) (contracts.ContentHash, error) {
	prepared := value
	prepared.DecisionHash = contracts.ContentHash{Algorithm: "sha256", Digest: strings.Repeat("0", 64)}
	encoded, err := json.Marshal(&prepared)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(encoded)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func NewGateDecision(requirement GateRequirement, state GateState, outcomeHash contracts.ContentHash, observations []ObservationBinding, guardrails []RecordPointer, reasons []string, evaluatedAt time.Time) (GateDecision, error) {
	slices.SortFunc(observations, func(left, right ObservationBinding) int {
		return strings.Compare(observationBindingKey(left), observationBindingKey(right))
	})
	slices.Sort(reasons)
	reasons = slices.Compact(reasons)
	slices.SortFunc(guardrails, func(left, right RecordPointer) int {
		return strings.Compare(recordPointerKey(left), recordPointerKey(right))
	})
	guardrails = slices.Compact(guardrails)
	value := GateDecision{SchemaVersion: GateSchemaVersion, Requirement: requirement, State: state, OutcomeHash: outcomeHash, Observations: observations, Guardrails: guardrails, Reasons: reasons, EvaluatedAt: evaluatedAt, DecisionHash: contracts.ContentHash{Algorithm: "sha256", Digest: strings.Repeat("0", 64)}}
	hash, err := gateDecisionHash(value)
	if err != nil {
		return GateDecision{}, err
	}
	value.DecisionHash = hash
	return value, value.Validate()
}

func metricReferenceKey(value MetricReference) string {
	return fmt.Sprintf("%s/%020d/%s", value.ID, value.Version, value.DefinitionHash.Digest)
}
func observationBindingKey(value ObservationBinding) string {
	return string(value.ID) + "/" + value.Hash.Digest
}
func sourceKey(value SourceRef) string {
	return string(value.Family) + "/" + value.EventID + "/" + value.Hash.Digest
}
func recordPointerKey(value RecordPointer) string {
	return value.Kind + "/" + value.ID + "/" + value.Hash.Digest
}
func evidenceKey(value contracts.EvidenceRef) string {
	return string(value.ID) + "/" + value.Hash.Digest
}

func sortedUniqueFamilies(values []SourceFamily) bool {
	return slices.IsSorted(values) && !hasDuplicates(values)
}

func sortedUniqueStrings(values []string) bool {
	return slices.IsSorted(values) && !hasDuplicates(values)
}

func hasDuplicates[T comparable](values []T) bool {
	seen := make(map[T]bool, len(values))
	for _, value := range values {
		if seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}

func validateToken(name, value string) error {
	if strings.TrimSpace(value) != value || value == "" || len(value) > 128 {
		return fmt.Errorf("business outcome: %s must contain 1 to 128 canonical bytes", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-_.:/", character) {
			continue
		}
		return fmt.Errorf("business outcome: %s contains an invalid character", name)
	}
	return nil
}

func validateText(name, value string, maximum int) error {
	if strings.TrimSpace(value) != value || value == "" || len(value) > maximum || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("business outcome: %s must contain 1 to %d canonical bytes", name, maximum)
	}
	return nil
}

func validateBounded(name, value string, maximum int) error {
	if strings.TrimSpace(value) != value || value == "" || len(value) > maximum || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("business outcome: %s is invalid", name)
	}
	return nil
}

func validUTC(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }

func absoluteDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}
