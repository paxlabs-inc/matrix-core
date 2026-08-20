package businessoutcome

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"centra/workforce/internal/commercialcapability"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/productcapability"
	"centra/workforce/internal/productexecution"
	"centra/workforce/internal/provider/customer"
	"centra/workforce/internal/provider/external"
)

type RegistrationPolicy struct {
	OutcomeKind          OutcomeKind                        `json:"outcome_kind"`
	Aggregation          Aggregation                        `json:"aggregation"`
	Scale                uint8                              `json:"scale"`
	Population           string                             `json:"population"`
	Inclusion            string                             `json:"inclusion"`
	Exclusion            string                             `json:"exclusion"`
	AttributionProcedure contracts.VerificationProcedureRef `json:"attribution_procedure"`
	Sources              []SourceFamily                     `json:"sources"`
	Reconciliation       ReconciliationDefinition           `json:"reconciliation"`
	Baseline             Baseline                           `json:"baseline"`
	Thresholds           ThresholdDefinition                `json:"thresholds"`
	Guardrails           []MetricReference                  `json:"guardrails"`
	AuthorSeatID         contracts.SeatID                   `json:"author_seat_id"`
	RegisteredAt         time.Time                          `json:"registered_at"`
	EffectiveAt          time.Time                          `json:"effective_at"`
	ExpiresAt            time.Time                          `json:"expires_at"`
	Supersedes           *MetricReference                   `json:"supersedes"`
}

func (value RegistrationPolicy) Validate() error {
	if !value.OutcomeKind.Valid() || !value.Aggregation.Valid() || value.Scale > 12 ||
		validateText("metric population", value.Population, 2048) != nil ||
		validateText("metric inclusion", value.Inclusion, 2048) != nil ||
		validateText("metric exclusion", value.Exclusion, 2048) != nil ||
		value.AttributionProcedure.Validate() != nil || len(value.Sources) == 0 ||
		len(value.Sources) > 16 || !sortedUniqueFamilies(value.Sources) ||
		value.Reconciliation.Validate() != nil || value.Baseline.Validate() != nil ||
		validateToken("author_seat_id", string(value.AuthorSeatID)) != nil ||
		!validUTC(value.RegisteredAt) || !validUTC(value.EffectiveAt) ||
		!validUTC(value.ExpiresAt) || value.EffectiveAt.Before(value.RegisteredAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) || value.Thresholds.Validate(value.RegisteredAt) != nil {
		return fmt.Errorf("business outcome: metric registration policy is invalid")
	}
	for _, source := range value.Sources {
		if !source.Valid() {
			return fmt.Errorf("business outcome: metric registration source is invalid")
		}
	}
	for index := range value.Guardrails {
		if value.Guardrails[index].Validate() != nil ||
			index > 0 && metricReferenceKey(value.Guardrails[index-1]) >= metricReferenceKey(value.Guardrails[index]) {
			return fmt.Errorf("business outcome: registration guardrails must be sorted and unique")
		}
	}
	return nil
}

func MetricFromProduct(value productcapability.VerifiedRecord, policy RegistrationPolicy, now time.Time) (MetricDefinition, error) {
	if value.ValidateAt(now) != nil || value.Record.Body.Kind != productcapability.RecordMetricDefinition ||
		value.Record.Body.Metric == nil || policy.Validate() != nil {
		return MetricDefinition{}, fmt.Errorf("business outcome: verified product metric and registration policy are required")
	}
	metric := value.Record.Body.Metric
	attribution := AttributionMode(metric.Attribution)
	requiredSource, sourceErr := sourceFamilyForProductMetric(metric.Source)
	if !attribution.Valid() || sourceErr != nil || !slices.Contains(policy.Sources, requiredSource) ||
		policy.Reconciliation.Procedure.ID != metric.ReconciliationProcedureID ||
		!sameGuardrailIDs(policy.Guardrails, metric.GuardrailMetricIDs) {
		return MetricDefinition{}, fmt.Errorf("business outcome: product metric identity disagrees with registration policy")
	}
	body := MetricDefinitionBody{
		SchemaVersion: SchemaVersion, ID: MetricID(metric.ID), Version: metric.Version,
		OrganizationID: metric.OrganizationID, InitiativeID: string(metric.InitiativeID),
		Name: metric.Name, OutcomeKind: policy.OutcomeKind, Aggregation: policy.Aggregation,
		Measure: MeasureDefinition{Unit: metric.Unit, Scale: policy.Scale, Numerator: metric.Numerator,
			Denominator: metric.Denominator, Population: policy.Population,
			Inclusion: policy.Inclusion, Exclusion: policy.Exclusion},
		Attribution: AttributionDefinition{Mode: attribution, Subject: metric.SourceIdentity,
			Window: time.Duration(metric.MaximumAgeSeconds) * time.Second, Procedure: policy.AttributionProcedure},
		Freshness: FreshnessDefinition{MaximumObservationAge: time.Duration(metric.MaximumAgeSeconds) * time.Second,
			MaximumCaptureDelay: time.Duration(metric.MaximumAgeSeconds) * time.Second},
		MaximumUncertaintyBPS: uint16(metric.UncertaintyBasisPoints), Sources: slices.Clone(policy.Sources),
		Reconciliation: policy.Reconciliation, Baseline: policy.Baseline, Thresholds: policy.Thresholds,
		Guardrails: slices.Clone(policy.Guardrails), AuthorSeatID: policy.AuthorSeatID,
		RegisteredAt: policy.RegisteredAt, EffectiveAt: policy.EffectiveAt,
		ExpiresAt: policy.ExpiresAt, Supersedes: policy.Supersedes,
	}
	if body.ExpiresAt.After(metric.ExpiresAt) {
		return MetricDefinition{}, fmt.Errorf("business outcome: metric registration outlives the verified product definition")
	}
	return MetricDefinition{Body: body}, body.Validate()
}

func MetricFromCommercial(value commercialcapability.VerifiedRecord, metricID commercialcapability.MetricID, policy RegistrationPolicy, now time.Time) (MetricDefinition, error) {
	if value.ValidateAt(now) != nil || policy.Validate() != nil {
		return MetricDefinition{}, fmt.Errorf("business outcome: verified commercial record and registration policy are required")
	}
	index := slices.IndexFunc(value.Record.Body.Metrics, func(metric commercialcapability.MetricDefinition) bool { return metric.ID == metricID })
	if index < 0 {
		return MetricDefinition{}, fmt.Errorf("business outcome: commercial metric is absent")
	}
	metric := value.Record.Body.Metrics[index]
	attribution := AttributionMode(metric.Attribution)
	if !attribution.Valid() || !slices.Contains(policy.Sources, sourceFamilyForCommercial(metric.SourceClass)) ||
		policy.Reconciliation.Procedure.ID != metric.Reconciliation ||
		!sameGuardrailIDs(policy.Guardrails, metric.Guardrails) {
		return MetricDefinition{}, fmt.Errorf("business outcome: commercial metric identity disagrees with registration policy")
	}
	body := MetricDefinitionBody{
		SchemaVersion: SchemaVersion, ID: MetricID(metric.ID), Version: metric.Version,
		OrganizationID: value.Record.Body.OrganizationID, InitiativeID: string(value.Record.Body.InitiativeID),
		Name: metric.Name, OutcomeKind: policy.OutcomeKind, Aggregation: policy.Aggregation,
		Measure: MeasureDefinition{Unit: metric.Unit, Scale: policy.Scale, Numerator: metric.Numerator,
			Denominator: metric.Denominator, Population: policy.Population,
			Inclusion: policy.Inclusion, Exclusion: policy.Exclusion},
		Attribution: AttributionDefinition{Mode: attribution, Subject: metric.SourceProvider,
			Window: metric.Freshness, Procedure: policy.AttributionProcedure},
		Freshness: FreshnessDefinition{MaximumObservationAge: metric.Freshness,
			MaximumCaptureDelay: metric.Freshness},
		MaximumUncertaintyBPS: metric.MaximumUncertaintyBPS, Sources: slices.Clone(policy.Sources),
		Reconciliation: policy.Reconciliation, Baseline: policy.Baseline, Thresholds: policy.Thresholds,
		Guardrails: slices.Clone(policy.Guardrails), AuthorSeatID: policy.AuthorSeatID,
		RegisteredAt: policy.RegisteredAt, EffectiveAt: policy.EffectiveAt,
		ExpiresAt: policy.ExpiresAt, Supersedes: policy.Supersedes,
	}
	if body.ExpiresAt.After(value.Record.Body.FreshUntil) {
		return MetricDefinition{}, fmt.Errorf("business outcome: metric registration outlives verified commercial evidence")
	}
	return MetricDefinition{Body: body}, body.Validate()
}

type MetricMeasurement struct {
	Metric              MetricReference         `json:"metric"`
	OutcomeKind         OutcomeKind             `json:"outcome_kind"`
	Status              MeasurementStatus       `json:"status"`
	Value               MeasurementValue        `json:"value"`
	SubjectRef          string                  `json:"subject_ref"`
	AttributionProof    contracts.EvidenceRef   `json:"attribution_proof"`
	ObservedAt          time.Time               `json:"observed_at"`
	FreshUntil          time.Time               `json:"fresh_until"`
	UncertaintyBPS      uint16                  `json:"uncertainty_bps"`
	KnownGaps           []string                `json:"known_gaps"`
	ConflictingEvidence []contracts.ContentHash `json:"conflicting_evidence"`
}

func (value MetricMeasurement) Validate() error {
	if value.Metric.Validate() != nil || !value.OutcomeKind.Valid() || !value.Status.Valid() ||
		value.Status == MeasurementReconciled || value.Value.Validate() != nil ||
		validateToken("measurement subject_ref", value.SubjectRef) != nil ||
		value.AttributionProof.Validate() != nil || !validUTC(value.ObservedAt) ||
		!validUTC(value.FreshUntil) || !value.FreshUntil.After(value.ObservedAt) ||
		value.AttributionProof.ObservedAt.After(value.ObservedAt) || value.UncertaintyBPS > 10_000 ||
		len(value.KnownGaps) > 64 || !sortedUniqueStrings(value.KnownGaps) ||
		len(value.ConflictingEvidence) > 64 {
		return fmt.Errorf("business outcome: provider metric measurement is invalid")
	}
	previous := ""
	for _, hash := range value.ConflictingEvidence {
		if hash.Validate() != nil || previous != "" && hash.Digest <= previous {
			return fmt.Errorf("business outcome: provider conflicting evidence is invalid")
		}
		previous = hash.Digest
	}
	return nil
}

type MetricEnvelope struct {
	SchemaVersion string              `json:"schema_version"`
	Measurements  []MetricMeasurement `json:"measurements"`
}

func (value MetricEnvelope) Validate() error {
	if value.SchemaVersion != MetricEnvelopeVersion || len(value.Measurements) == 0 || len(value.Measurements) > 256 {
		return fmt.Errorf("business outcome: metric envelope is invalid")
	}
	for index := range value.Measurements {
		if value.Measurements[index].Validate() != nil ||
			index > 0 && metricReferenceKey(value.Measurements[index-1].Metric) >= metricReferenceKey(value.Measurements[index].Metric) {
			return fmt.Errorf("business outcome: metric envelope measurements must be sorted and unique")
		}
	}
	return nil
}

type IngestScope struct {
	InitiativeID string           `json:"initiative_id"`
	AuthorSeatID contracts.SeatID `json:"author_seat_id"`
	CapturedAt   time.Time        `json:"captured_at"`
}

func (value IngestScope) Validate() error {
	if validateToken("initiative_id", value.InitiativeID) != nil ||
		validateToken("author_seat_id", string(value.AuthorSeatID)) != nil || !validUTC(value.CapturedAt) {
		return fmt.Errorf("business outcome: ingest scope is invalid")
	}
	return nil
}

func ObservationsFromExternal(scope IngestScope, value external.Observation) ([]Observation, error) {
	if scope.Validate() != nil || value.Validate() != nil || value.State != external.ExternalCompleted ||
		(value.Authority != external.AuthorityProvider && value.Authority != external.AuthorityControlPlane) {
		return nil, fmt.Errorf("business outcome: external observation is not a completed authoritative provider fact")
	}
	envelope, err := contracts.DecodeCanonical[MetricEnvelope, *MetricEnvelope](value.Output)
	if err != nil {
		return nil, fmt.Errorf("business outcome: external output is not a typed metric envelope: %w", err)
	}
	hash, err := external.CanonicalHash(&value)
	if err != nil {
		return nil, err
	}
	primary := SourceRef{
		Family: sourceFamilyForExternal(value.Family), Authority: AuthorityProviderReported,
		RecordID: stableToken("external-record", hash.Digest), EventID: stableToken("external-event", value.ConnectionID, fmt.Sprint(value.ConnectionVersion), value.Operation, value.IdempotencyKey, string(value.State)),
		Hash: hash, Provider: value.Provider, Account: value.AccountID, ObjectRef: value.ExternalID,
		ConnectionID: value.ConnectionID, ConnectionVersion: value.ConnectionVersion,
		Operation: value.Operation, IdempotencyKey: value.IdempotencyKey,
		State: SourceCompleted, ObservedAt: value.ProviderObservedAt,
	}
	return observationsFromEnvelope(scope, value.OrganizationID, envelope, primary)
}

func ObservationsFromCustomer(scope IngestScope, value customer.Observation) ([]Observation, error) {
	if scope.Validate() != nil || value.Validate() != nil || value.State != external.ExternalCompleted ||
		(value.Authority != external.AuthorityProvider && value.Authority != external.AuthorityControlPlane) {
		return nil, fmt.Errorf("business outcome: customer observation is not a completed authoritative provider fact")
	}
	envelope, err := contracts.DecodeCanonical[MetricEnvelope, *MetricEnvelope](value.Outcome.Details)
	if err != nil {
		return nil, fmt.Errorf("business outcome: customer outcome is not a typed metric envelope: %w", err)
	}
	hash, err := customer.CanonicalHash(&value)
	if err != nil {
		return nil, err
	}
	authority := AuthorityProviderReported
	if value.Family == customer.FamilyCustomerObservation {
		authority = AuthorityCustomerReported
	}
	primary := SourceRef{
		Family: sourceFamilyForCustomer(value.Family), Authority: authority,
		RecordID: stableToken("customer-record", hash.Digest), EventID: stableToken("customer-event", value.ConnectionID, fmt.Sprint(value.ConnectionVersion), value.Operation, value.IdempotencyKey, string(value.State)),
		Hash: hash, Provider: "customer-adapter", Account: value.Outcome.AccountID,
		ObjectRef: value.CustomerID, ConnectionID: value.ConnectionID,
		ConnectionVersion: value.ConnectionVersion, Operation: value.Operation,
		IdempotencyKey: value.IdempotencyKey, State: SourceCompleted,
		ObservedAt: value.ProviderObservedAt,
	}
	return observationsFromEnvelope(scope, value.OrganizationID, envelope, primary)
}

func observationsFromEnvelope(scope IngestScope, organizationID contracts.OrganizationID, envelope MetricEnvelope, primary SourceRef) ([]Observation, error) {
	result := make([]Observation, 0, len(envelope.Measurements))
	for _, measurement := range envelope.Measurements {
		status := measurement.Status
		if status != MeasurementObserved && status != MeasurementPending && status != MeasurementContested {
			return nil, fmt.Errorf("business outcome: provider envelope cannot assert proposal, reconciliation, or retraction")
		}
		body := ObservationBody{
			SchemaVersion:  SchemaVersion,
			ID:             ObservationID(stableToken("observation", primary.EventID, string(measurement.Metric.ID), fmt.Sprint(measurement.Metric.Version))),
			OrganizationID: organizationID, InitiativeID: scope.InitiativeID,
			AuthorSeatID: scope.AuthorSeatID, Metric: measurement.Metric,
			OutcomeKind: measurement.OutcomeKind, Status: status, Value: measurement.Value,
			SubjectRef: measurement.SubjectRef, AttributionProof: measurement.AttributionProof,
			Primary: primary, Supporting: []SourceRef{}, Reconciliation: Reconciliation{
				State: ReconciliationNotRequired, Procedure: measurement.AttributionProofProcedure(),
			},
			ObservedAt: measurement.ObservedAt, CapturedAt: scope.CapturedAt,
			FreshUntil: measurement.FreshUntil, UncertaintyBPS: measurement.UncertaintyBPS,
			KnownGaps:           slices.Clone(measurement.KnownGaps),
			ConflictingEvidence: slices.Clone(measurement.ConflictingEvidence),
		}
		primary.ObservedAt = measurement.ObservedAt
		body.Primary = primary
		if err := body.Validate(); err != nil {
			return nil, err
		}
		result = append(result, Observation{Body: body})
	}
	return result, nil
}

func (value MetricMeasurement) AttributionProofProcedure() contracts.VerificationProcedureRef {
	return contracts.VerificationProcedureRef{ID: "reconciliation:not-required", Version: 1, Digest: value.AttributionProof.Hash}
}

func ObservationFromCommercial(scope IngestScope, value commercialcapability.VerifiedRecord, observationID commercialcapability.ObservationID, metricID commercialcapability.MetricID, metric MetricReference, now time.Time) (Observation, error) {
	if scope.Validate() != nil || value.ValidateAt(now) != nil || metric.Validate() != nil {
		return Observation{}, fmt.Errorf("business outcome: verified commercial observation and ingest scope are required")
	}
	observationIndex := slices.IndexFunc(value.Record.Body.Observations, func(item commercialcapability.AuthoritativeObservation) bool { return item.ID == observationID })
	metricIndex := slices.IndexFunc(value.Record.Body.Metrics, func(item commercialcapability.MetricDefinition) bool { return item.ID == metricID })
	if observationIndex < 0 || metricIndex < 0 {
		return Observation{}, fmt.Errorf("business outcome: commercial observation or metric is absent")
	}
	observed := value.Record.Body.Observations[observationIndex]
	definition := value.Record.Body.Metrics[metricIndex]
	if !slices.Contains(observed.MetricIDs, metricID) || MetricID(metricID) != metric.ID ||
		definition.Unit != observed.Value.Unit {
		return Observation{}, fmt.Errorf("business outcome: commercial observation is not bound to the requested metric")
	}
	recordHash, err := commercialcapability.RecordHash(value.Record)
	if err != nil {
		return Observation{}, err
	}
	primary := SourceRef{
		Family: sourceFamilyForCommercial(observed.Primary.Class), Authority: AuthorityProviderReported,
		RecordID: string(value.Record.Body.ID), EventID: string(observed.ID), Hash: recordHash,
		Provider: observed.Primary.Provider, Account: observed.Primary.Account,
		ObjectRef: observed.Primary.ObjectRef, State: SourceCompleted, ObservedAt: observed.ObservedAt,
	}
	reconciliation := Reconciliation{State: ReconciliationNotRequired, Procedure: contracts.VerificationProcedureRef{ID: "commercial:source-validation", Version: 1, Digest: observed.Primary.Evidence.Hash}}
	status := MeasurementObserved
	supporting := []SourceRef{}
	if observed.Kind.Economic() {
		if observed.Reconciliation == nil {
			return Observation{}, fmt.Errorf("business outcome: economic commercial observation is unreconciled")
		}
		primary.Authority = AuthorityReconciledFinancial
		primary.Family = sourceFamilyForCommercial(observed.Primary.Class)
		primary.State = SourceReconciled
		independent := SourceRef{
			Family: sourceFamilyForCommercial(observed.Reconciliation.Class), Authority: AuthorityReconciledFinancial,
			RecordID: string(value.Record.Body.ID), EventID: stableToken("commercial-reconciliation", string(observed.ID), observed.Reconciliation.Evidence.Hash.Digest),
			Hash: observed.Reconciliation.Evidence.Hash, Provider: observed.Reconciliation.Provider,
			Account: observed.Reconciliation.Account, ObjectRef: observed.Reconciliation.ObjectRef,
			State: SourceReconciled, ObservedAt: observed.Reconciliation.Evidence.ObservedAt,
		}
		valuedAt := observed.Reconciliation.Evidence.ObservedAt
		valuation := Valuation{Currency: observed.Value.Currency, ValuedAt: valuedAt,
			Method: "commercial-independent-reconciliation", Evidence: observed.Reconciliation.Evidence}
		reconciledAt := observed.Reconciliation.Evidence.ObservedAt
		reconciliation = Reconciliation{State: ReconciliationReconciled,
			Procedure:   contracts.VerificationProcedureRef{ID: "commercial:financial-reconciliation", Version: 1, Digest: observed.Reconciliation.Evidence.Hash},
			Independent: &independent, Valuation: &valuation, ReconciledAt: &reconciledAt}
		status = MeasurementReconciled
		supporting = append(supporting, independent)
	}
	valueUnit := observed.Value.Unit
	numerator, err := numeratorForValue(observed.Value.ValueMicros, observed.Value.Denominator, observed.Value.Scale)
	if err != nil {
		return Observation{}, err
	}
	measurement := MeasurementValue{NumeratorMicros: numerator,
		Denominator: observed.Value.Denominator, ValueMicros: observed.Value.ValueMicros,
		Scale: observed.Value.Scale, Unit: valueUnit}
	body := ObservationBody{
		SchemaVersion:  SchemaVersion,
		ID:             ObservationID(stableToken("observation", string(value.Record.Body.ID), string(observed.ID), string(metric.ID), fmt.Sprint(metric.Version))),
		OrganizationID: value.Record.Body.OrganizationID, InitiativeID: string(value.Record.Body.InitiativeID),
		AuthorSeatID: scope.AuthorSeatID, Metric: metric, OutcomeKind: OutcomeKind(value.Record.Body.Outcome.Kind),
		Status: status, Value: measurement, SubjectRef: observed.SubjectRef,
		AttributionProof: observed.Primary.Evidence, Primary: primary, Supporting: supporting,
		Reconciliation: reconciliation, ObservedAt: observed.ObservedAt, CapturedAt: scope.CapturedAt,
		FreshUntil: observed.FreshUntil, UncertaintyBPS: observed.UncertaintyBPS,
		KnownGaps: slices.Clone(observed.KnownGaps), ConflictingEvidence: slices.Clone(observed.ConflictingEvidence),
	}
	return Observation{Body: body}, body.Validate()
}

type OccurrencePolicy struct {
	Metric           MetricReference       `json:"metric"`
	OutcomeKind      OutcomeKind           `json:"outcome_kind"`
	Value            MeasurementValue      `json:"value"`
	SubjectRef       string                `json:"subject_ref"`
	AttributionProof contracts.EvidenceRef `json:"attribution_proof"`
	FreshUntil       time.Time             `json:"fresh_until"`
	UncertaintyBPS   uint16                `json:"uncertainty_bps"`
}

func (value OccurrencePolicy) Validate() error {
	if value.Metric.Validate() != nil || !value.OutcomeKind.Valid() || value.Value.Validate() != nil ||
		validateToken("occurrence subject_ref", value.SubjectRef) != nil || value.AttributionProof.Validate() != nil ||
		!validUTC(value.FreshUntil) || value.UncertaintyBPS > 10_000 {
		return fmt.Errorf("business outcome: occurrence policy is invalid")
	}
	return nil
}

func ObservationFromProductRecord(scope IngestScope, value productcapability.VerifiedRecord, policy OccurrencePolicy, now time.Time) (Observation, error) {
	if scope.Validate() != nil || value.ValidateAt(now) != nil || policy.Validate() != nil ||
		value.Record.Body.Kind == productcapability.RecordMetricDefinition {
		return Observation{}, fmt.Errorf("business outcome: verified product occurrence and policy are required")
	}
	kind := policy.OutcomeKind
	if value.Record.Body.Kind == productcapability.RecordReliabilityIncident {
		if kind != OutcomeRisk {
			return Observation{}, fmt.Errorf("business outcome: reliability incidents are risk outcomes")
		}
	} else if kind != OutcomeActivity && kind != OutcomeOutput {
		return Observation{}, fmt.Errorf("business outcome: product work records are activity or output only")
	}
	hash, err := productcapability.RecordHash(value.Record)
	if err != nil {
		return Observation{}, err
	}
	primary := SourceRef{Family: SourceProduct, Authority: AuthorityInternalVerified,
		RecordID: string(value.Record.Body.ID), EventID: stableToken("product-record", string(value.Record.Body.ID), fmt.Sprint(value.Record.Body.Version)),
		Hash: hash, Provider: "workforce-product-capability", Account: string(value.Record.Body.OrganizationID),
		ObjectRef: string(value.Record.Body.ChainID), State: SourceCompleted, ObservedAt: value.Record.Body.EffectiveAt}
	body := ObservationBody{SchemaVersion: SchemaVersion,
		ID:             ObservationID(stableToken("observation", primary.EventID, string(policy.Metric.ID), fmt.Sprint(policy.Metric.Version))),
		OrganizationID: value.Record.Body.OrganizationID, InitiativeID: string(value.Record.Body.InitiativeID),
		AuthorSeatID: scope.AuthorSeatID, Metric: policy.Metric, OutcomeKind: kind,
		Status: MeasurementObserved, Value: policy.Value, SubjectRef: policy.SubjectRef,
		AttributionProof: policy.AttributionProof, Primary: primary, Supporting: []SourceRef{},
		Reconciliation: Reconciliation{State: ReconciliationNotRequired,
			Procedure: contracts.VerificationProcedureRef{ID: "product:record-verification", Version: 1, Digest: value.Verification.ProcedureDigest}},
		ObservedAt: value.Record.Body.EffectiveAt, CapturedAt: scope.CapturedAt,
		FreshUntil: policy.FreshUntil, UncertaintyBPS: policy.UncertaintyBPS,
		KnownGaps: []string{}, ConflictingEvidence: []contracts.ContentHash{},
	}
	return Observation{Body: body}, body.Validate()
}

func ObservationFromProductExecution(scope IngestScope, organizationID contracts.OrganizationID, initiativeID string, value productexecution.Event, policy OccurrencePolicy) (Observation, error) {
	if scope.Validate() != nil || value.Validate() != nil || policy.Validate() != nil ||
		initiativeID != scope.InitiativeID || policy.OutcomeKind != OutcomeActivity && policy.OutcomeKind != OutcomeOutput {
		return Observation{}, fmt.Errorf("business outcome: product execution event or occurrence policy is invalid")
	}
	hash, err := contracts.HashCanonical(&value)
	if err != nil {
		return Observation{}, err
	}
	primary := SourceRef{Family: SourceOperational, Authority: AuthorityInternalVerified,
		RecordID: string(value.ExecutionID),
		EventID:  stableToken("product-execution-event", string(value.ExecutionID), fmt.Sprint(value.Sequence)),
		Hash:     hash, Provider: "workforce-product-execution", Account: string(organizationID),
		ObjectRef: fmt.Sprint(value.Sequence), State: SourceCompleted, ObservedAt: value.CreatedAt}
	body := ObservationBody{SchemaVersion: SchemaVersion,
		ID:             ObservationID(stableToken("observation", primary.EventID, string(policy.Metric.ID), fmt.Sprint(policy.Metric.Version))),
		OrganizationID: organizationID, InitiativeID: initiativeID, AuthorSeatID: scope.AuthorSeatID,
		Metric: policy.Metric, OutcomeKind: policy.OutcomeKind, Status: MeasurementObserved,
		Value: policy.Value, SubjectRef: policy.SubjectRef, AttributionProof: policy.AttributionProof,
		Primary: primary, Supporting: []SourceRef{}, Reconciliation: Reconciliation{
			State:     ReconciliationNotRequired,
			Procedure: contracts.VerificationProcedureRef{ID: "product-execution:event", Version: 1, Digest: hash}},
		ObservedAt: value.CreatedAt, CapturedAt: scope.CapturedAt, FreshUntil: policy.FreshUntil,
		UncertaintyBPS: policy.UncertaintyBPS, KnownGaps: []string{}, ConflictingEvidence: []contracts.ContentHash{},
	}
	return Observation{Body: body}, body.Validate()
}

type FinancialFact struct {
	SchemaVersion    string                             `json:"schema_version"`
	ID               ObservationID                      `json:"observation_id"`
	OrganizationID   contracts.OrganizationID           `json:"organization_id"`
	InitiativeID     string                             `json:"initiative_id"`
	AuthorSeatID     contracts.SeatID                   `json:"author_seat_id"`
	Metric           MetricReference                    `json:"metric"`
	OutcomeKind      OutcomeKind                        `json:"outcome_kind"`
	Value            MeasurementValue                   `json:"value"`
	SubjectRef       string                             `json:"subject_ref"`
	AttributionProof contracts.EvidenceRef              `json:"attribution_proof"`
	Primary          SourceRef                          `json:"primary"`
	Independent      SourceRef                          `json:"independent"`
	Valuation        Valuation                          `json:"valuation"`
	Procedure        contracts.VerificationProcedureRef `json:"procedure"`
	ObservedAt       time.Time                          `json:"observed_at"`
	CapturedAt       time.Time                          `json:"captured_at"`
	FreshUntil       time.Time                          `json:"fresh_until"`
	ReconciledAt     time.Time                          `json:"reconciled_at"`
	UncertaintyBPS   uint16                             `json:"uncertainty_bps"`
}

func (value FinancialFact) Validate() error {
	if value.SchemaVersion != SchemaVersion || validateToken("financial observation_id", string(value.ID)) != nil ||
		validateToken("organization_id", string(value.OrganizationID)) != nil ||
		validateToken("initiative_id", value.InitiativeID) != nil ||
		validateToken("author_seat_id", string(value.AuthorSeatID)) != nil || value.Metric.Validate() != nil ||
		value.OutcomeKind != OutcomeEconomic && value.OutcomeKind != OutcomeCommercial ||
		value.Value.Validate() != nil || validateToken("financial subject_ref", value.SubjectRef) != nil ||
		value.AttributionProof.Validate() != nil || value.Primary.Validate() != nil || value.Independent.Validate() != nil ||
		!value.Primary.Family.Financial() || !value.Independent.Family.Financial() ||
		value.Primary.Authority != AuthorityReconciledFinancial || value.Independent.Authority != AuthorityReconciledFinancial ||
		value.Primary.EventID == value.Independent.EventID || value.Primary.Hash == value.Independent.Hash ||
		value.Primary.Provider == value.Independent.Provider || value.Valuation.Validate() != nil ||
		value.Procedure.Validate() != nil || !validUTC(value.ObservedAt) || !validUTC(value.CapturedAt) ||
		!validUTC(value.FreshUntil) || !validUTC(value.ReconciledAt) ||
		value.Primary.ObservedAt != value.ObservedAt || value.CapturedAt.Before(value.ObservedAt) ||
		value.ReconciledAt.Before(value.ObservedAt) || !value.FreshUntil.After(value.ReconciledAt) ||
		value.UncertaintyBPS > 10_000 {
		return fmt.Errorf("business outcome: financial fact is not independently reconciled")
	}
	return nil
}

func ObservationFromFinancial(value FinancialFact) (Observation, error) {
	if value.Validate() != nil {
		return Observation{}, fmt.Errorf("business outcome: reconciled financial fact is required")
	}
	independent := value.Independent
	reconciledAt := value.ReconciledAt
	body := ObservationBody{SchemaVersion: SchemaVersion, ID: value.ID,
		OrganizationID: value.OrganizationID, InitiativeID: value.InitiativeID,
		AuthorSeatID: value.AuthorSeatID, Metric: value.Metric, OutcomeKind: value.OutcomeKind,
		Status: MeasurementReconciled, Value: value.Value, SubjectRef: value.SubjectRef,
		AttributionProof: value.AttributionProof, Primary: value.Primary,
		Supporting: []SourceRef{value.Independent}, Reconciliation: Reconciliation{
			State: ReconciliationReconciled, Procedure: value.Procedure,
			Independent: &independent, Valuation: &value.Valuation, ReconciledAt: &reconciledAt},
		ObservedAt: value.ObservedAt, CapturedAt: value.CapturedAt, FreshUntil: value.FreshUntil,
		UncertaintyBPS: value.UncertaintyBPS, KnownGaps: []string{},
		ConflictingEvidence: []contracts.ContentHash{},
	}
	slices.SortFunc(body.Supporting, func(left, right SourceRef) int { return strings.Compare(sourceKey(left), sourceKey(right)) })
	return Observation{Body: body}, body.Validate()
}

func sourceFamilyForExternal(value external.Family) SourceFamily {
	switch value {
	case external.FamilyProductAnalytics:
		return SourceProductTelemetry
	case external.FamilyDeployment, external.FamilyInfrastructure:
		return SourceDeployment
	default:
		return SourceExternalProvider
	}
}

func sourceFamilyForProductMetric(value string) (SourceFamily, error) {
	switch value {
	case "product_telemetry":
		return SourceProductTelemetry, nil
	case "customer_observation":
		return SourceCustomer, nil
	case "deployment_observation":
		return SourceDeployment, nil
	case "support_observation":
		return SourceSupport, nil
	case "analytical_derivation":
		return SourceAnalytical, nil
	default:
		return "", fmt.Errorf("business outcome: product metric source is unsupported")
	}
}

func sameGuardrailIDs(references []MetricReference, expected []string) bool {
	if len(references) != len(expected) {
		return false
	}
	for _, id := range expected {
		if !slices.ContainsFunc(references, func(reference MetricReference) bool {
			return string(reference.ID) == id
		}) {
			return false
		}
	}
	return true
}

func sourceFamilyForCustomer(value customer.Family) SourceFamily {
	switch value {
	case customer.FamilyCRM, customer.FamilySalesPipeline:
		return SourceCRM
	case customer.FamilySupport:
		return SourceSupport
	case customer.FamilyCustomerObservation, customer.FamilyCustomerOnboarding:
		return SourceCustomer
	default:
		return SourceChannel
	}
}

func sourceFamilyForCommercial(value commercialcapability.SourceClass) SourceFamily {
	switch value {
	case commercialcapability.SourceCRM:
		return SourceCRM
	case commercialcapability.SourceSupportSystem:
		return SourceSupport
	case commercialcapability.SourceProductAnalytics:
		return SourceProductTelemetry
	case commercialcapability.SourceBillingLedger:
		return SourceBilling
	case commercialcapability.SourceAccountingLedger, commercialcapability.SourceBankLedger:
		return SourceAccounting
	default:
		return SourceCommercial
	}
}

func stableToken(prefix string, values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		hash.Write([]byte{0})
		hash.Write([]byte(value))
	}
	return prefix + ":" + hex.EncodeToString(hash.Sum(nil))[:32]
}
