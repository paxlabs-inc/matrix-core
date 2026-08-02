// Package learning owns preregistered hypotheses, deterministic evaluation,
// independent review, and the evidence-backed next-cycle decision.
package learning

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

const (
	HypothesisSchemaVersion  = "workforce.learning-hypothesis.v1"
	ObservationSchemaVersion = "workforce.learning-observation.v1"
	EvaluationSchemaVersion  = "workforce.learning-evaluation.v1"
	ReviewSchemaVersion      = "workforce.learning-review.v1"
	ConclusionSchemaVersion  = "workforce.learning-conclusion.v1"
)

type Result string

const (
	ResultSucceeded     Result = "succeeded"
	ResultFailed        Result = "failed"
	ResultInconclusive  Result = "inconclusive"
	ResultRequiresHuman Result = "requires_human"
)

func (value Result) Valid() bool {
	return value == ResultSucceeded || value == ResultFailed ||
		value == ResultInconclusive || value == ResultRequiresHuman
}

type NextAction string

const (
	ActionScale       NextAction = "SCALE"
	ActionPivot       NextAction = "PIVOT"
	ActionMaintain    NextAction = "MAINTAIN"
	ActionTerminate   NextAction = "TERMINATE"
	ActionDiscover    NextAction = "DISCOVER"
	ActionHumanReview NextAction = "REQUIRES_HUMAN"
)

func (value NextAction) Valid() bool {
	switch value {
	case ActionScale, ActionPivot, ActionMaintain, ActionTerminate,
		ActionDiscover, ActionHumanReview:
		return true
	default:
		return false
	}
}

type Comparator string

const (
	ComparatorAtLeast Comparator = "at_least"
	ComparatorAtMost  Comparator = "at_most"
)

func (value Comparator) Valid() bool {
	return value == ComparatorAtLeast || value == ComparatorAtMost
}

type MetricThreshold struct {
	MetricID          string     `json:"metric_id"`
	MetricVersion     uint64     `json:"metric_version"`
	Comparator        Comparator `json:"comparator"`
	SuccessValue      int64      `json:"success_value"`
	StopValue         int64      `json:"stop_value"`
	DenominatorMetric string     `json:"denominator_metric_id"`
	MaximumAgeSeconds uint64     `json:"maximum_age_seconds"`
}

func (value MetricThreshold) Validate() error {
	if token(value.MetricID) != nil || value.MetricVersion == 0 ||
		!value.Comparator.Valid() || value.MaximumAgeSeconds == 0 ||
		value.MaximumAgeSeconds > uint64((365*24*time.Hour)/time.Second) {
		return fmt.Errorf("learning: metric threshold is invalid")
	}
	if value.DenominatorMetric != "" && token(value.DenominatorMetric) != nil {
		return fmt.Errorf("learning: denominator identity is invalid")
	}
	if value.Comparator == ComparatorAtLeast && value.StopValue >= value.SuccessValue ||
		value.Comparator == ComparatorAtMost && value.StopValue <= value.SuccessValue {
		return fmt.Errorf("learning: success and stop thresholds overlap")
	}
	return nil
}

type Hypothesis struct {
	SchemaVersion       string                   `json:"schema_version"`
	ID                  string                   `json:"hypothesis_id"`
	Version             uint64                   `json:"version"`
	OrganizationID      contracts.OrganizationID `json:"organization_id"`
	InitiativeID        string                   `json:"initiative_id"`
	Statement           string                   `json:"statement"`
	RegistrarSeatID     contracts.SeatID         `json:"registrar_seat_id"`
	MetricThresholds    []MetricThreshold        `json:"metric_thresholds"`
	EvidenceSourceKinds []string                 `json:"evidence_source_kinds"`
	AttributionRule     string                   `json:"attribution_rule"`
	RegisteredAt        time.Time                `json:"registered_at"`
	ReviewAt            time.Time                `json:"review_at"`
	MaximumDurationAt   time.Time                `json:"maximum_duration_at"`
	Signature           contracts.Signature      `json:"signature"`
}

func (value Hypothesis) Validate() error {
	if value.SchemaVersion != HypothesisSchemaVersion || value.Version != 1 ||
		token(value.ID) != nil || token(string(value.OrganizationID)) != nil ||
		token(value.InitiativeID) != nil || token(string(value.RegistrarSeatID)) != nil ||
		strings.TrimSpace(value.Statement) == "" || len(value.Statement) > 4096 ||
		strings.TrimSpace(value.AttributionRule) == "" || len(value.AttributionRule) > 2048 ||
		len(value.MetricThresholds) == 0 || len(value.MetricThresholds) > 32 ||
		!utc(value.RegisteredAt) || !utc(value.ReviewAt) || !utc(value.MaximumDurationAt) ||
		!value.ReviewAt.After(value.RegisteredAt) ||
		value.MaximumDurationAt.Before(value.ReviewAt) {
		return fmt.Errorf("learning: hypothesis is incomplete")
	}
	seen := make(map[string]bool, len(value.MetricThresholds))
	for _, threshold := range value.MetricThresholds {
		if threshold.Validate() != nil || seen[threshold.MetricID] {
			return fmt.Errorf("learning: hypothesis metric registry is invalid")
		}
		seen[threshold.MetricID] = true
	}
	if err := tokenSet(value.EvidenceSourceKinds, 1, 32); err != nil {
		return err
	}
	return value.Signature.Validate()
}

type ObservationAuthority string

const (
	AuthorityProviderReported ObservationAuthority = "provider_reported"
	AuthorityCustomerReported ObservationAuthority = "customer_reported"
	AuthorityFinancial        ObservationAuthority = "reconciled_financial"
	AuthorityAnalytical       ObservationAuthority = "analytically_derived"
)

func (value ObservationAuthority) Valid() bool {
	return value == AuthorityProviderReported || value == AuthorityCustomerReported ||
		value == AuthorityFinancial || value == AuthorityAnalytical
}

type Observation struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"observation_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   string                   `json:"initiative_id"`
	HypothesisID   string                   `json:"hypothesis_id"`
	MetricID       string                   `json:"metric_id"`
	MetricVersion  uint64                   `json:"metric_version"`
	Value          int64                    `json:"value"`
	Denominator    *int64                   `json:"denominator"`
	Authority      ObservationAuthority     `json:"authority"`
	Reconciled     bool                     `json:"reconciled"`
	SourceID       string                   `json:"source_id"`
	Evidence       contracts.EvidenceRef    `json:"evidence"`
	ProducerSeatID contracts.SeatID         `json:"producer_seat_id"`
	ObservedAt     time.Time                `json:"observed_at"`
	FreshUntil     time.Time                `json:"fresh_until"`
	Signature      contracts.Signature      `json:"signature"`
}

func (value Observation) ValidateAt(now time.Time) error {
	if err := value.Validate(); err != nil || !utc(now) ||
		!value.FreshUntil.After(now) || value.ObservedAt.After(now) {
		return fmt.Errorf("learning: observation is not authoritative and current")
	}
	return nil
}

func (value Observation) Validate() error {
	if value.SchemaVersion != ObservationSchemaVersion || token(value.ID) != nil ||
		token(string(value.OrganizationID)) != nil || token(value.InitiativeID) != nil ||
		token(value.HypothesisID) != nil || token(value.MetricID) != nil ||
		value.MetricVersion == 0 || !value.Authority.Valid() || token(value.SourceID) != nil ||
		token(string(value.ProducerSeatID)) != nil || !value.Reconciled ||
		!utc(value.ObservedAt) || !utc(value.FreshUntil) ||
		!value.FreshUntil.After(value.ObservedAt) ||
		value.Evidence.Validate() != nil || value.Evidence.ObservedAt != value.ObservedAt {
		return fmt.Errorf("learning: observation is not authoritative and complete")
	}
	return value.Signature.Validate()
}

type MetricResult struct {
	MetricID      string                `json:"metric_id"`
	Result        Result                `json:"result"`
	Value         int64                 `json:"value"`
	ObservationID string                `json:"observation_id"`
	EvidenceHash  contracts.ContentHash `json:"evidence_hash"`
}

type Evaluation struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"evaluation_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   string                   `json:"initiative_id"`
	HypothesisID   string                   `json:"hypothesis_id"`
	HypothesisHash contracts.ContentHash    `json:"hypothesis_hash"`
	Results        []MetricResult           `json:"results"`
	Result         Result                   `json:"result"`
	EvaluatedAt    time.Time                `json:"evaluated_at"`
	Signature      contracts.Signature      `json:"signature"`
}

func (value Evaluation) Validate() error {
	if value.SchemaVersion != EvaluationSchemaVersion || token(value.ID) != nil ||
		token(string(value.OrganizationID)) != nil || token(value.InitiativeID) != nil ||
		token(value.HypothesisID) != nil || value.HypothesisHash.Validate() != nil ||
		len(value.Results) == 0 || len(value.Results) > 32 || !value.Result.Valid() ||
		!utc(value.EvaluatedAt) {
		return fmt.Errorf("learning: evaluation is invalid")
	}
	for _, result := range value.Results {
		if token(result.MetricID) != nil || !result.Result.Valid() {
			return fmt.Errorf("learning: metric evaluation is invalid")
		}
		missing := result.ObservationID == ""
		if missing {
			if result.Result != ResultInconclusive || result.EvidenceHash != (contracts.ContentHash{}) {
				return fmt.Errorf("learning: missing metric evidence has an invalid result")
			}
		} else if token(result.ObservationID) != nil || result.EvidenceHash.Validate() != nil {
			return fmt.Errorf("learning: metric evidence is invalid")
		}
	}
	return value.Signature.Validate()
}

type ReviewDecision string

const (
	ReviewApprove       ReviewDecision = "approve"
	ReviewReject        ReviewDecision = "reject"
	ReviewRequiresHuman ReviewDecision = "requires_human"
)

type IndependentReview struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"review_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   string                   `json:"initiative_id"`
	HypothesisID   string                   `json:"hypothesis_id"`
	EvaluationID   string                   `json:"evaluation_id"`
	EvaluationHash contracts.ContentHash    `json:"evaluation_hash"`
	AuditorSeatID  contracts.SeatID         `json:"auditor_seat_id"`
	Decision       ReviewDecision           `json:"decision"`
	ReasonCode     string                   `json:"reason_code"`
	ReviewedAt     time.Time                `json:"reviewed_at"`
	Signature      contracts.Signature      `json:"signature"`
}

func (value IndependentReview) Validate() error {
	if value.SchemaVersion != ReviewSchemaVersion || token(value.ID) != nil ||
		token(string(value.OrganizationID)) != nil || token(value.InitiativeID) != nil ||
		token(value.HypothesisID) != nil || token(value.EvaluationID) != nil ||
		value.EvaluationHash.Validate() != nil || token(string(value.AuditorSeatID)) != nil ||
		(value.Decision != ReviewApprove && value.Decision != ReviewReject &&
			value.Decision != ReviewRequiresHuman) || token(value.ReasonCode) != nil ||
		!utc(value.ReviewedAt) {
		return fmt.Errorf("learning: independent review is invalid")
	}
	return value.Signature.Validate()
}

type Conclusion struct {
	SchemaVersion       string                   `json:"schema_version"`
	ID                  string                   `json:"conclusion_id"`
	OrganizationID      contracts.OrganizationID `json:"organization_id"`
	InitiativeID        string                   `json:"initiative_id"`
	HypothesisID        string                   `json:"hypothesis_id"`
	EvaluationID        string                   `json:"evaluation_id"`
	ReviewID            string                   `json:"review_id"`
	Result              Result                   `json:"result"`
	NextAction          NextAction               `json:"next_action"`
	SupersededRecordIDs []string                 `json:"superseded_record_ids"`
	PortfolioFeedbackID string                   `json:"portfolio_feedback_id"`
	NextReviewAt        time.Time                `json:"next_review_at"`
	CommittedAt         time.Time                `json:"committed_at"`
	Signature           contracts.Signature      `json:"signature"`
}

func (value Conclusion) Validate() error {
	if value.SchemaVersion != ConclusionSchemaVersion || token(value.ID) != nil ||
		token(string(value.OrganizationID)) != nil || token(value.InitiativeID) != nil ||
		token(value.HypothesisID) != nil || token(value.EvaluationID) != nil ||
		token(value.ReviewID) != nil || !value.Result.Valid() || !value.NextAction.Valid() ||
		token(value.PortfolioFeedbackID) != nil || !utc(value.CommittedAt) ||
		!utc(value.NextReviewAt) || !value.NextReviewAt.After(value.CommittedAt) ||
		tokenSet(value.SupersededRecordIDs, 0, 128) != nil {
		return fmt.Errorf("learning: conclusion is invalid")
	}
	if value.Result == ResultRequiresHuman && value.NextAction != ActionHumanReview ||
		value.Result != ResultRequiresHuman && value.NextAction == ActionHumanReview {
		return fmt.Errorf("learning: conclusion action does not match result")
	}
	return value.Signature.Validate()
}

func token(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("learning: identity is invalid")
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-_.:", character) {
			continue
		}
		return fmt.Errorf("learning: identity is invalid")
	}
	return nil
}

func tokenSet(values []string, minimum, maximum int) error {
	if len(values) < minimum || len(values) > maximum || !slices.IsSorted(values) {
		return fmt.Errorf("learning: identity set is not canonical")
	}
	for index, value := range values {
		if token(value) != nil || index > 0 && values[index-1] == value {
			return fmt.Errorf("learning: identity set is not canonical")
		}
	}
	return nil
}

func utc(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}
