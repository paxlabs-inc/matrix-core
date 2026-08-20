package financial

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/lease"
	"centra/workforce/internal/provider/external"
	"centra/workforce/internal/skills"
)

type authorizedOperation struct {
	connection  loadedConnection
	valuation   loadedValuation
	risk        loadedRisk
	policy      OperationPolicy
	venue       VenueScope
	envelope    Envelope
	requestHash contracts.ContentHash
	command     ProviderCommand
	commandBody []byte
	reserved    bool
}

func (adapter *Adapter) authorize(
	ctx context.Context,
	now time.Time,
	operation effect.Operation,
	reconcile bool,
) (authorizedOperation, error) {
	envelope, err := contracts.DecodeCanonical[Envelope, *Envelope](operation.Input)
	if err != nil {
		return authorizedOperation{}, fmt.Errorf("%w: canonical envelope is invalid", ErrDenied)
	}
	if !bytes.Equal(operation.Input, mustCanonical(&envelope)) {
		return authorizedOperation{}, fmt.Errorf("%w: envelope is not canonical", ErrDenied)
	}
	var connection loadedConnection
	if reconcile {
		connection, err = adapter.store.loadConnectionVersion(
			ctx, operation.OrganizationID, envelope.ConnectionID, envelope.ConnectionVersion,
		)
		if err == nil && connection.hash != envelope.ConnectionHash {
			err = ErrIntegrity
		}
	} else {
		connection, err = adapter.store.loadActive(
			ctx, operation.OrganizationID, envelope.ConnectionID,
			envelope.ConnectionVersion, envelope.ConnectionHash,
		)
	}
	if err != nil {
		return authorizedOperation{}, err
	}
	if connection.connection.OrganizationID != adapter.organizationID ||
		connection.connection.ID != adapter.connectionID ||
		connection.connection.AdapterName != adapter.name ||
		connection.connection.ExternalAdapterName != adapter.upstream.Name() {
		return authorizedOperation{}, fmt.Errorf("%w: adapter or connection binding mismatch", ErrDenied)
	}
	grant := envelope.Grant
	if grant.Request.Validate() != nil || grant.Fence.Validate() != nil ||
		grant.State != lease.StateActive || grant.OrganizationID != operation.OrganizationID ||
		grant.SeatID != operation.SeatID || grant.ID != operation.LeaseID ||
		grant.Fence != operation.Fence || !reconcile && !grant.ExpiresAt.After(now) {
		return authorizedOperation{}, fmt.Errorf("%w: lease and fence binding mismatch", ErrDenied)
	}
	for _, required := range connection.connection.Authority.PolicyRefs {
		if !containsPolicy(grant.Policies, required) {
			return authorizedOperation{}, fmt.Errorf("%w: lease omits financial policy authority", ErrDenied)
		}
	}
	policy, found := connection.connection.Operation(operation.Name)
	request := envelope.Request
	if !found || request.Operation != operation.Name || request.Family != policy.Family ||
		request.Action != policy.Action || request.ProposalID != operation.ProposalID ||
		request.IntentID != operation.IntentID || request.IdempotencyKey != operation.IdempotencyKey ||
		operation.EffectClass != policy.EffectClass || operation.Irreversible != request.Action.Mutates() ||
		request.ApprovalID != operation.ApprovalID || request.ApprovalCostMicrounits != operation.ApprovalCost {
		return authorizedOperation{}, fmt.Errorf("%w: gateway proposal and financial request binding mismatch", ErrDenied)
	}
	if !reconcile && (!request.ExpiresAt.After(now) || request.IssuedAt.After(now) ||
		request.ExpiresAt.After(grant.ExpiresAt) || request.ExpiresAt.After(connection.connection.ExpiresAt) ||
		operation.Deadline.IsZero() || operation.Deadline.Location() != time.UTC ||
		request.ExpiresAt.After(operation.Deadline)) {
		return authorizedOperation{}, fmt.Errorf("%w: request is outside exact time authority", ErrDenied)
	}
	if request.Action.Mutates() {
		if operation.EffectClass != skills.EffectIrreversible || !operation.Irreversible ||
			operation.ApprovalID == "" || operation.ApprovalCost == 0 {
			return authorizedOperation{}, fmt.Errorf("%w: financial mutation did not traverse exact gateway approval", ErrDenied)
		}
		total, ok := add(request.NotionalMicrounits, request.FeeCeilingMicrounits)
		if !ok || operation.ApprovalCost != total {
			return authorizedOperation{}, fmt.Errorf("%w: approval cost does not bind maximum capital movement", ErrDenied)
		}
	} else if operation.EffectClass != skills.EffectRead || operation.Irreversible ||
		operation.ApprovalID != "" || operation.ApprovalCost != 0 {
		return authorizedOperation{}, fmt.Errorf("%w: read-only financial observation borrowed mutation authority", ErrDenied)
	}
	if err := authorizeGovernance(connection.connection, policy, request); err != nil {
		return authorizedOperation{}, err
	}
	valuation, err := adapter.store.loadValuation(
		ctx, connection, request.ValuationID, request.ValuationVersion,
		request.ValuationHash, !reconcile,
	)
	if err != nil {
		return authorizedOperation{}, err
	}
	price, ok := valuation.valuation.Price(request.Amount.Asset)
	if !ok {
		return authorizedOperation{}, fmt.Errorf("%w: asset lacks exact authoritative valuation", ErrStaleValuation)
	}
	notional, err := NotionalMicrounits(request.Amount, price)
	if err != nil || notional != request.NotionalMicrounits {
		return authorizedOperation{}, fmt.Errorf("%w: request notional does not match bound valuation", ErrDenied)
	}
	if valuation.valuation.BaseCurrency != request.BaseCurrency ||
		request.BaseCurrency != connection.connection.Capital.BaseCurrency ||
		request.FeeCeilingMicrounits > connection.connection.Capital.MaxFeeMicrounits ||
		request.PriceToleranceBPS > connection.connection.Capital.MaxToleranceBPS ||
		request.MaximumLossMicrounits > request.NotionalMicrounits+request.FeeCeilingMicrounits ||
		request.ExposureIncreaseMicrounits > request.NotionalMicrounits {
		return authorizedOperation{}, ErrLimit
	}
	switch policy.RiskClass {
	case RiskInflow:
		if request.ExposureIncreaseMicrounits != 0 || request.MaximumLossMicrounits > request.FeeCeilingMicrounits {
			return authorizedOperation{}, ErrLimit
		}
	case RiskOutflow:
		if request.MaximumLossMicrounits < request.NotionalMicrounits {
			return authorizedOperation{}, ErrLimit
		}
	case RiskExposure:
		if request.ExposureIncreaseMicrounits == 0 {
			return authorizedOperation{}, ErrLimit
		}
	case RiskNeutral:
		if request.ExposureIncreaseMicrounits != 0 || request.MaximumLossMicrounits != 0 {
			return authorizedOperation{}, ErrLimit
		}
	}
	risk, err := adapter.store.loadRisk(
		ctx, connection, request.RiskSnapshotID, request.RiskSnapshotVersion,
		request.RiskSnapshotHash, !reconcile,
	)
	if err != nil {
		return authorizedOperation{}, err
	}
	if risk.state.BaseCurrency != request.BaseCurrency ||
		risk.state.ResourceVersion != request.ExpectedProviderResourceVersion {
		return authorizedOperation{}, fmt.Errorf("%w: request does not bind current provider resource version", ErrStaleRisk)
	}
	requestHash, err := CanonicalHash(&request)
	if err != nil {
		return authorizedOperation{}, err
	}
	venue, _ := connection.connection.Governance.Venue(request.Venue, request.Rail)
	reserved := policy.FounderReserved || policy.Action.ConstitutionReserved() || venue.FounderReserved
	if err := adapter.verifyFounderReservation(now, connection.connection, request, requestHash, envelope.Founder, reserved, reconcile); err != nil {
		return authorizedOperation{}, err
	}
	command := providerCommand(operation.OrganizationID, connection.connection.ProviderContract, request)
	commandBody, err := contracts.EncodeCanonical(&command)
	if err != nil {
		return authorizedOperation{}, err
	}
	if err := validateExternalEnvelope(connection.connection, policy, request, envelope.External, commandBody); err != nil {
		return authorizedOperation{}, err
	}
	return authorizedOperation{
		connection: connection, valuation: valuation, risk: risk, policy: policy,
		venue: venue, envelope: envelope, requestHash: requestHash,
		command: command, commandBody: commandBody, reserved: reserved,
	}, nil
}

func authorizeGovernance(connection Connection, policy OperationPolicy, request Request) error {
	governance := connection.Governance
	if request.AccountID != connection.AccountID || request.IdentityID != connection.IdentityID ||
		request.Family != connection.Family || !contains(governance.Initiatives, request.InitiativeID) ||
		!contains(governance.Purposes, request.Purpose) || !contains(governance.Assets, request.Amount.Asset) ||
		!contains(governance.Instruments, request.Instrument) ||
		!contains(governance.Counterparties, request.Counterparty) ||
		!contains(governance.Jurisdictions, request.Jurisdiction) ||
		!containsHash(governance.DestinationHashes, request.DestinationHash) ||
		!contains(governance.LegalPolicyRefs, request.LegalPolicyRef) ||
		!contains(governance.SecurityPolicyRefs, request.SecurityPolicyRef) ||
		!contains(governance.ReconciliationPolicyRefs, request.ReconciliationPolicyRef) ||
		request.AccountingMethodologyID != connection.Authority.AccountingMethodologyID ||
		!containsPolicy(connection.Authority.PolicyRefs, request.FinancialPolicy) {
		return fmt.Errorf("%w: request exceeds exact account, initiative, asset, counterparty, or policy scope", ErrDenied)
	}
	venue, ok := governance.Venue(request.Venue, request.Rail)
	if !ok {
		return fmt.Errorf("%w: venue and rail are not activated", ErrDenied)
	}
	switch policy.Action {
	case ActionPlaceTrade, ActionCancelTrade:
		if !venue.AllowsTrading {
			return fmt.Errorf("%w: venue does not authorize trading", ErrDenied)
		}
	case ActionLeverage:
		if !venue.AllowsLeverage {
			return fmt.Errorf("%w: venue does not authorize leverage", ErrDenied)
		}
	case ActionBorrow, ActionRepayDebt:
		if !venue.AllowsDebt {
			return fmt.Errorf("%w: venue does not authorize debt", ErrDenied)
		}
	case ActionWithdraw:
		if !venue.AllowsWithdrawal {
			return fmt.Errorf("%w: venue does not authorize withdrawal", ErrDenied)
		}
	case ActionCustodyTransfer:
		if !venue.AllowsCustodyMove {
			return fmt.Errorf("%w: venue does not authorize custody transfer", ErrDenied)
		}
	}
	if request.NotionalMicrounits > connection.Capital.PerEffectMicrounits {
		return ErrLimit
	}
	for _, scoped := range []struct {
		kind ScopeKind
		key  string
	}{
		{ScopeAsset, request.Amount.Asset},
		{ScopeVenue, request.Venue + "/" + request.Rail},
		{ScopeCounterparty, request.Counterparty},
		{ScopeInitiative, request.InitiativeID},
	} {
		limit, found := connection.Capital.Limit(scoped.kind, scoped.key)
		if !found || request.NotionalMicrounits > limit.PerEffectMicrounits ||
			request.ExposureIncreaseMicrounits > limit.MaxExposureMicrounits {
			return ErrLimit
		}
	}
	return nil
}

func (adapter *Adapter) verifyFounderReservation(
	now time.Time,
	connection Connection,
	request Request,
	requestHash contracts.ContentHash,
	reservation *FounderReservation,
	required bool,
	reconcile bool,
) error {
	if !required {
		if reservation != nil {
			return fmt.Errorf("%w: unrelated founder authority cannot be borrowed", ErrReserved)
		}
		return nil
	}
	if reservation == nil || VerifyFounderReservation(*reservation, adapter.store.founderKey, adapter.store.founderPub) != nil ||
		reservation.OrganizationID != connection.OrganizationID || reservation.ConnectionID != connection.ID ||
		reservation.ConnectionVersion != connection.Version || reservation.RequestHash != requestHash ||
		reservation.ProposalID != request.ProposalID || reservation.IntentID != request.IntentID ||
		reservation.Operation != request.Operation || reservation.ApprovalID != request.ApprovalID ||
		reservation.ApprovalCostMicrounits != request.ApprovalCostMicrounits ||
		reservation.MaximumNotional < request.NotionalMicrounits ||
		reservation.DestinationHash != request.DestinationHash || reservation.IssuedAt.After(request.IssuedAt) ||
		reservation.ExpiresAt.Before(request.ExpiresAt) || !reconcile && !reservation.ExpiresAt.After(now) {
		return ErrReserved
	}
	return nil
}

func providerCommand(organizationID contracts.OrganizationID, contract ProviderContract, request Request) ProviderCommand {
	return ProviderCommand{
		SchemaVersion: SchemaVersion, OrganizationID: organizationID,
		InitiativeID: request.InitiativeID, Family: request.Family, Action: request.Action,
		ProviderContract: contract,
		Purpose:          request.Purpose, AccountID: request.AccountID, IdentityID: request.IdentityID,
		Amount: request.Amount, BaseCurrency: request.BaseCurrency, Instrument: request.Instrument,
		Venue: request.Venue, Rail: request.Rail, Counterparty: request.Counterparty,
		Jurisdiction: request.Jurisdiction, Destination: request.Destination,
		DestinationHash: request.DestinationHash, NotionalMicrounits: request.NotionalMicrounits,
		ExposureIncreaseMicrounits: request.ExposureIncreaseMicrounits,
		MaximumLossMicrounits:      request.MaximumLossMicrounits,
		FeeCeilingMicrounits:       request.FeeCeilingMicrounits,
		PriceToleranceBPS:          request.PriceToleranceBPS, ValuationID: request.ValuationID,
		ValuationVersion: request.ValuationVersion, ValuationHash: request.ValuationHash,
		ExpectedResourceVersion: request.ExpectedProviderResourceVersion,
		LegalPolicyRef:          request.LegalPolicyRef, SecurityPolicyRef: request.SecurityPolicyRef,
		ReconciliationPolicyRef: request.ReconciliationPolicyRef,
		IdempotencyKey:          request.IdempotencyKey,
	}
}

func validateExternalEnvelope(
	connection Connection,
	policy OperationPolicy,
	request Request,
	envelope external.Envelope,
	commandBody []byte,
) error {
	bound := envelope.Request
	if envelope.ConnectionID != connection.ExternalConnectionID ||
		envelope.ConnectionVersion != connection.ExternalConnectionVersion ||
		envelope.ConnectionHash != connection.ExternalConnectionHash ||
		bound.Action != policy.ExternalAction || bound.TargetURL != connection.ProviderTargetURL ||
		bound.AccountID != request.AccountID || bound.IdentityID != request.IdentityID ||
		bound.DataClassification != external.DataFinancial || bound.Counterparty != request.Counterparty ||
		bound.Jurisdiction != request.Jurisdiction || bound.LegalPolicyRef != request.LegalPolicyRef ||
		bound.SecurityPolicyRef != request.SecurityPolicyRef ||
		bound.ExpectedResourceVersion != request.ExpectedProviderResourceVersion ||
		bound.OutputBytes > connection.Capital.OutputBytes || bound.Recipient != request.Destination ||
		!bytes.Equal(bound.Body, commandBody) || bound.Upload != nil || bound.Download {
		return fmt.Errorf("%w: external transport envelope does not match exact financial command", ErrDenied)
	}
	var decoded ProviderCommand
	decoder := json.NewDecoder(bytes.NewReader(bound.Body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil || decoded.Validate() != nil ||
		decoded != providerCommand(connection.OrganizationID, connection.ProviderContract, request) {
		return fmt.Errorf("%w: external financial command is malformed or substituted", ErrDenied)
	}
	return nil
}

func mustCanonical[T contracts.Validatable](value T) []byte {
	encoded, err := contracts.EncodeCanonical(value)
	if err != nil {
		return nil
	}
	return encoded
}
