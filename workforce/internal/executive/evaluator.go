package executive

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/mission"
)

var (
	// ErrUnauthorized reports identity, signature, organization, or exact-policy mismatch.
	ErrUnauthorized = errors.New("executive: unauthorized")
	// ErrAuthorityNotCurrent reports an authority that is not yet effective, expired, or revoked.
	ErrAuthorityNotCurrent = errors.New("executive: authority is not current")
	// ErrConflict reports immutable identity or idempotency reuse with different content.
	ErrConflict = errors.New("executive: immutable conflict")
	// ErrNotFound intentionally combines absent and out-of-scope Executive state.
	ErrNotFound = errors.New("executive: record not found")
	// ErrIntegrity reports sealed-record, hash, canonical, or signature failure.
	ErrIntegrity = errors.New("executive: record integrity failure")
)

// EvaluationContext is the transactionally observed rolling authority use for
// one server-computed semantic scope and the whole organization.
type EvaluationContext struct {
	At                              time.Time
	ScopeRollingCapitalMicrounits   uint64
	ScopeRollingExposureMicrounits  uint64
	GlobalRollingCapitalMicrounits  uint64
	GlobalRollingExposureMicrounits uint64
	PolicyRevoked                   bool
}

func (value EvaluationContext) validate() error {
	if !validUTC(value.At) || value.ScopeRollingCapitalMicrounits > maxMicrounits ||
		value.ScopeRollingExposureMicrounits > maxMicrounits ||
		value.GlobalRollingCapitalMicrounits > maxMicrounits ||
		value.GlobalRollingExposureMicrounits > maxMicrounits {
		return fmt.Errorf("executive: evaluation context is invalid")
	}
	return nil
}

type evaluation struct {
	outcome         DecisionOutcome
	reasonCode      string
	reasons         []string
	reservedKind    mission.ReservedDecisionKind
	incidentKind    IncidentKind
	reviewBindings  []ReviewBinding
	rollingCapital  uint64
	rollingExposure uint64
	authorizedUntil time.Time
}

func evaluate(
	authority CompiledAuthority,
	request DecisionRequest,
	reviews []Review,
	context EvaluationContext,
) (evaluation, error) {
	if err := authority.Validate(); err != nil {
		return evaluation{}, err
	}
	if err := request.Validate(); err != nil {
		return evaluation{}, err
	}
	if err := context.validate(); err != nil {
		return evaluation{}, err
	}
	if context.PolicyRevoked || context.At.Before(authority.EffectiveAt) || !context.At.Before(authority.ExpiresAt) {
		return evaluation{}, ErrAuthorityNotCurrent
	}
	if request.CreatedAt.After(context.At) || !context.At.Before(request.ExpiresAt) {
		return evaluation{}, ErrAuthorityNotCurrent
	}
	if request.OrganizationID != authority.OrganizationID || request.PolicyID != authority.PolicyID ||
		request.PolicyVersion != authority.PolicyVersion || request.PolicyHash != authority.PolicyHash {
		return evaluation{}, ErrUnauthorized
	}
	maker, found := findDecisionMaker(authority.DecisionMakers, request.RequesterSeatID)
	if !found || maker.MandateID != request.RequesterMandateID ||
		maker.MandateVersion != request.RequesterMandateVersion {
		return evaluation{}, ErrUnauthorized
	}
	publicKey, err := decodePublicKey(maker.SigningPublicKey)
	if err != nil || VerifyDecisionRequest(request, maker.SigningKeyID, publicKey) != nil {
		return evaluation{}, ErrUnauthorized
	}
	if request.ExpiresAt.After(authority.ExpiresAt) || request.NextReviewAt.After(authority.ExpiresAt) {
		return founderEvaluation(
			"authority_lifetime_exceeded", mission.ReservedControlRelaxation,
			[]string{"The request or its next review would outlive the current compiled authority."},
			"",
		), nil
	}
	clause, found := findClause(authority.Clauses, request.ClauseID)
	if !found {
		return founderEvaluation(
			"clause_not_delegated", mission.ReservedControlRelaxation,
			[]string{"The requested authority clause is not present in the current compiled policy."},
			IncidentAuthorityEvasion,
		), nil
	}
	if clause.Action != request.Action {
		return founderEvaluation(
			"authority_downgrade_attempt", mission.ReservedControlRelaxation,
			[]string{"The request action does not match the exact compiled authority clause."},
			IncidentAuthorityEvasion,
		), nil
	}
	if reservedKind, reserved := request.Operation.Class.ReservedKind(); reserved {
		return founderEvaluation(
			"founder_reserved_operation", reservedKind,
			[]string{"The bound downstream operation is explicitly founder-reserved."},
			IncidentAuthorityEvasion,
		), nil
	}
	if request.Operation.Class != OperationBoundedCompanyDecision {
		return evaluation{}, ErrUnauthorized
	}
	if !slices.Contains(clause.AllowedLifecycleStates, request.LifecycleState) {
		return founderEvaluation(
			"lifecycle_state_not_delegated", mission.ReservedControlRelaxation,
			[]string{"The current lifecycle state is outside the clause's exact state set."},
			"",
		), nil
	}
	if !scopePermitted(request.Jurisdiction, clause.PermittedJurisdictions) {
		return founderEvaluation(
			"jurisdiction_not_delegated", mission.ReservedRestrictedRegion,
			[]string{"The requested jurisdiction is not delegated by the exact clause."},
			"",
		), nil
	}
	if !scopePermitted(request.Counterparty, clause.PermittedCounterparties) {
		return founderEvaluation(
			"counterparty_not_delegated", mission.ReservedControlRelaxation,
			[]string{"The requested counterparty is not delegated by the exact clause."},
			"",
		), nil
	}
	if request.CapitalMicrounits > clause.MaxRequestCapitalMicrounits ||
		request.ExposureMicrounits > clause.MaxRequestExposureMicrounits {
		return founderEvaluation(
			"request_threshold_exceeded", mission.ReservedCapitalIncrease,
			[]string{"The request exceeds an inclusive per-request capital or exposure boundary."},
			"",
		), nil
	}
	if request.ResourceUnits > clause.MaxResourceUnits ||
		request.PriceChangeBPS > clause.MaxPriceChangeBPS ||
		request.DurationSeconds > clause.MaxDurationSeconds {
		return founderEvaluation(
			"material_threshold_exceeded", mission.ReservedControlRelaxation,
			[]string{"The request exceeds an inclusive resource, pricing, or duration boundary."},
			"",
		), nil
	}
	if request.NextReviewAt.After(context.At.Add(time.Duration(clause.NextReviewWithinSeconds) * time.Second)) {
		return founderEvaluation(
			"review_window_exceeded", mission.ReservedControlRelaxation,
			[]string{"The next review is later than the exact clause permits."},
			"",
		), nil
	}

	scopeCapital, ok := safeAdd(context.ScopeRollingCapitalMicrounits, request.CapitalMicrounits)
	if !ok {
		return evaluation{}, ErrConflict
	}
	scopeExposure, ok := safeAdd(context.ScopeRollingExposureMicrounits, request.ExposureMicrounits)
	if !ok {
		return evaluation{}, ErrConflict
	}
	globalCapital, ok := safeAdd(context.GlobalRollingCapitalMicrounits, request.CapitalMicrounits)
	if !ok {
		return evaluation{}, ErrConflict
	}
	globalExposure, ok := safeAdd(context.GlobalRollingExposureMicrounits, request.ExposureMicrounits)
	if !ok {
		return evaluation{}, ErrConflict
	}
	if scopeCapital > clause.MaxRollingCapitalMicrounits ||
		scopeExposure > clause.MaxRollingExposureMicrounits ||
		globalCapital > authority.MaxRollingCapitalMicrounits ||
		globalExposure > authority.MaxRollingExposureMicrounits {
		return founderEvaluation(
			"split_request_boundary_exceeded", mission.ReservedCapitalIncrease,
			[]string{"Related authorized decisions plus this request exceed a rolling capital or exposure boundary."},
			IncidentAuthorityEvasion,
		), nil
	}
	if request.Action == ActionEmergencyPause {
		if len(reviews) != 0 {
			return evaluation{}, fmt.Errorf("executive: emergency pause must not be delayed by review")
		}
		return evaluation{
			outcome:         DecisionEmergencyPaused,
			reasonCode:      "emergency_pause",
			reasons:         []string{"The exact emergency-pause clause authorized a fail-closed pause without an external effect."},
			rollingCapital:  globalCapital,
			rollingExposure: globalExposure,
			authorizedUntil: minTime(request.ExpiresAt, authority.ExpiresAt),
		}, nil
	}
	if !request.EvidenceFreshUntil.After(context.At) {
		return founderEvaluation(
			"stale_evidence", mission.ReservedControlRelaxation,
			[]string{"The bound decision evidence is no longer fresh."},
			IncidentStaleEvidence,
		), nil
	}

	reviewEvaluation, err := evaluateReviews(authority, request, reviews, clause, context.At)
	if err != nil {
		return evaluation{}, err
	}
	reviewEvaluation.rollingCapital = globalCapital
	reviewEvaluation.rollingExposure = globalExposure
	reviewEvaluation.authorizedUntil = minTime(request.ExpiresAt, authority.ExpiresAt)
	return reviewEvaluation, nil
}

func evaluateReviews(
	authority CompiledAuthority,
	request DecisionRequest,
	reviews []Review,
	clause DecisionClause,
	at time.Time,
) (evaluation, error) {
	if len(reviews) != len(clause.RequiredReviews) {
		return founderEvaluation(
			"independent_review_missing", mission.ReservedControlRelaxation,
			[]string{"One or more required independent review disciplines are unavailable."},
			"",
		), nil
	}
	conflicts := make(map[contracts.SeatID]bool, len(request.ConflictedSeatIDs))
	for _, seatID := range request.ConflictedSeatIDs {
		conflicts[seatID] = true
	}
	seenKinds := make(map[ReviewKind]bool, len(reviews))
	seenSeats := make(map[contracts.SeatID]bool, len(reviews))
	bindings := make([]ReviewBinding, 0, len(reviews))
	reviewIDs := make([]ReviewID, 0, len(reviews))
	outcomeSet := make(map[ReviewOutcome]bool, 3)
	var reasons []string
	var anyDissent bool
	for index := range reviews {
		review := reviews[index]
		if err := review.Validate(); err != nil {
			return evaluation{}, fmt.Errorf("executive: review %d: %w", index, err)
		}
		if review.OrganizationID != request.OrganizationID || review.RequestID != request.ID ||
			!slices.Contains(clause.RequiredReviews, review.Kind) || seenKinds[review.Kind] {
			return evaluation{}, ErrUnauthorized
		}
		binding, found := findReviewer(authority.Reviewers, review.ReviewerSeatID, review.Kind)
		if !found {
			return evaluation{}, ErrUnauthorized
		}
		publicKey, err := decodePublicKey(binding.SigningPublicKey)
		if err != nil || VerifyReview(review, binding.SigningKeyID, publicKey) != nil {
			return evaluation{}, ErrUnauthorized
		}
		if conflicts[review.ReviewerSeatID] || seenSeats[review.ReviewerSeatID] {
			return evaluation{
				outcome:      DecisionDenied,
				reasonCode:   "self_approval",
				reasons:      []string{"A conflicted or already-used reviewer seat attempted to approve the material decision."},
				incidentKind: IncidentSelfApproval,
			}, nil
		}
		if review.ReviewedAt.Before(request.CreatedAt) || review.ReviewedAt.After(at) ||
			!review.ExpiresAt.After(at) {
			return founderEvaluation(
				"stale_independent_review", mission.ReservedControlRelaxation,
				[]string{"A required independent review is stale or predates the request."},
				IncidentStaleEvidence,
			), nil
		}
		hash, err := contracts.HashCanonical(&review)
		if err != nil {
			return evaluation{}, err
		}
		bindings = append(bindings, ReviewBinding{
			ID: review.ID, Kind: review.Kind, Outcome: review.Outcome,
			SeatID: review.ReviewerSeatID, Hash: hash,
		})
		reviewIDs = append(reviewIDs, review.ID)
		seenKinds[review.Kind] = true
		seenSeats[review.ReviewerSeatID] = true
		outcomeSet[review.Outcome] = true
		anyDissent = anyDissent || len(review.Dissent) > 0
		for _, dissent := range review.Dissent {
			reasons = append(reasons, string(review.Kind)+": "+dissent)
		}
	}
	slices.SortFunc(bindings, func(left, right ReviewBinding) int {
		return stringsCompare(string(left.Kind), string(right.Kind))
	})
	slices.SortFunc(reviewIDs, func(left, right ReviewID) int {
		return stringsCompare(string(left), string(right))
	})
	for _, required := range clause.RequiredReviews {
		if !seenKinds[required] {
			return founderEvaluation(
				"independent_review_missing", mission.ReservedControlRelaxation,
				[]string{"A required independent review discipline is absent."},
				"",
			), nil
		}
	}
	if len(outcomeSet) > 1 || anyDissent {
		if len(reasons) == 0 {
			reasons = []string{"Independent review outcomes materially disagree."}
		}
		slices.Sort(reasons)
		return evaluation{
			outcome:        DecisionFounderRequired,
			reasonCode:     "material_review_disagreement",
			reasons:        reasons,
			reservedKind:   mission.ReservedControlRelaxation,
			incidentKind:   IncidentMaterialDisagreement,
			reviewBindings: bindings,
		}, nil
	}
	if outcomeSet[ReviewRequiresHuman] {
		return evaluation{
			outcome:        DecisionFounderRequired,
			reasonCode:     "review_requires_human",
			reasons:        []string{"Independent review could not establish authoritative proof."},
			reservedKind:   mission.ReservedControlRelaxation,
			reviewBindings: bindings,
		}, nil
	}
	if outcomeSet[ReviewReject] {
		return evaluation{
			outcome:        DecisionDenied,
			reasonCode:     "independent_review_rejected",
			reasons:        []string{"Every independent review rejected the material decision."},
			reviewBindings: bindings,
		}, nil
	}
	return evaluation{
		outcome:        DecisionAuthorized,
		reasonCode:     "compiled_clause_satisfied",
		reasons:        []string{"The request satisfied the exact compiled clause and all independent reviews approved it."},
		reviewBindings: bindings,
	}, nil
}

func founderEvaluation(
	reasonCode string,
	reservedKind mission.ReservedDecisionKind,
	reasons []string,
	incidentKind IncidentKind,
) evaluation {
	slices.Sort(reasons)
	return evaluation{
		outcome:      DecisionFounderRequired,
		reasonCode:   reasonCode,
		reasons:      reasons,
		reservedKind: reservedKind,
		incidentKind: incidentKind,
	}
}

func findDecisionMaker(values []SeatAuthorityBinding, seatID contracts.SeatID) (SeatAuthorityBinding, bool) {
	for _, value := range values {
		if value.SeatID == seatID {
			return value, true
		}
	}
	return SeatAuthorityBinding{}, false
}

func findReviewer(values []SeatAuthorityBinding, seatID contracts.SeatID, kind ReviewKind) (SeatAuthorityBinding, bool) {
	for _, value := range values {
		if value.SeatID == seatID && slices.Contains(value.ReviewKinds, kind) {
			return value, true
		}
	}
	return SeatAuthorityBinding{}, false
}

func findClause(values []DecisionClause, clauseID string) (DecisionClause, bool) {
	for _, value := range values {
		if value.ClauseID == clauseID {
			return value, true
		}
	}
	return DecisionClause{}, false
}

func scopePermitted(requested string, permitted []string) bool {
	return requested == "" || slices.Contains(permitted, requested)
}

func safeAdd(left, right uint64) (uint64, bool) {
	if left > maxMicrounits || right > maxMicrounits || right > maxMicrounits-left {
		return 0, false
	}
	return left + right, true
}

func minTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

func decodePublicKey(encoded string) (ed25519.PublicKey, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("executive: policy-bound public key is invalid")
	}
	return ed25519.PublicKey(decoded), nil
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
