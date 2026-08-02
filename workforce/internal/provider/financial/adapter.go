package financial

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/effect"
	"matrix/workforce/internal/provider/external"
	"matrix/workforce/internal/skills"
)

type Adapter struct {
	store          *Store
	upstream       effect.Adapter
	organizationID contracts.OrganizationID
	connectionID   string
	name           string
}

func NewAdapter(
	store *Store,
	upstream effect.Adapter,
	organizationID contracts.OrganizationID,
	connectionID string,
	adapterName string,
) (*Adapter, error) {
	if store == nil || upstream == nil || token("organization id", string(organizationID)) != nil ||
		token("connection id", connectionID) != nil || token("adapter name", adapterName) != nil ||
		token("external adapter name", upstream.Name()) != nil {
		return nil, fmt.Errorf("financial adapter: exact store, upstream, and connection identity are required")
	}
	return &Adapter{store: store, upstream: upstream, organizationID: organizationID,
		connectionID: connectionID, name: adapterName}, nil
}

func LoadAdapters(
	ctx context.Context,
	store *Store,
	organizationID contracts.OrganizationID,
	upstreams map[string]effect.Adapter,
) ([]effect.Adapter, error) {
	if store == nil || len(upstreams) == 0 {
		return nil, fmt.Errorf("financial adapter: store and upstream registry are required")
	}
	views, err := store.ListActiveConnections(ctx, organizationID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(views))
	adapters := make([]effect.Adapter, 0, len(views))
	for _, view := range views {
		connection := view.Connection
		upstream, ok := upstreams[connection.ExternalAdapterName]
		if !ok || upstream == nil || upstream.Name() != connection.ExternalAdapterName ||
			seen[connection.AdapterName] {
			return nil, fmt.Errorf("financial adapter: exact upstream adapter is missing or duplicated")
		}
		adapter, err := NewAdapter(store, upstream, organizationID,
			connection.ID, connection.AdapterName)
		if err != nil {
			return nil, err
		}
		seen[connection.AdapterName] = true
		adapters = append(adapters, adapter)
	}
	return adapters, nil
}

func (adapter *Adapter) Name() string {
	if adapter == nil {
		return ""
	}
	return adapter.name
}

func (adapter *Adapter) Dispatch(
	ctx context.Context,
	operation effect.Operation,
) (effect.DispatchResult, error) {
	if adapter == nil || adapter.store == nil || adapter.upstream == nil {
		return effect.DispatchResult{}, ErrUnavailable
	}
	now, err := adapter.store.currentTime()
	if err != nil {
		return effect.DispatchResult{}, err
	}
	authorized, err := adapter.authorize(ctx, now, operation, false)
	if err != nil {
		adapter.recordAuthorizationDenial(context.WithoutCancel(ctx), operation, err)
		return effect.DispatchResult{}, err
	}
	claim, err := adapter.store.claimDispatch(ctx, authorized, operation)
	if err != nil {
		adapter.recordAuthorizationDenial(context.WithoutCancel(ctx), operation, err)
		return effect.DispatchResult{}, err
	}
	subOperation, err := upstreamOperation(operation, authorized)
	if err != nil {
		_ = adapter.store.markDispatchFailed(context.WithoutCancel(ctx), claim, "", "financial_transport_encode_failed")
		return effect.DispatchResult{}, err
	}
	result, dispatchErr := adapter.upstream.Dispatch(ctx, subOperation)
	if dispatchErr != nil && !result.Started {
		_ = adapter.store.markDispatchFailed(context.WithoutCancel(ctx), claim,
			result.ExternalID, "financial_provider_dispatch_not_started")
		return result, dispatchErr
	}
	var observation Observation
	var encoded []byte
	observationHash := ""
	if len(result.Observation) != 0 {
		observation, encoded, err = adapter.financialObservation(
			now, authorized, operation.IdempotencyKey, result.Observation,
			!authorized.policy.Action.Mutates(),
		)
		if err == nil {
			result.Observation = encoded
			result.ObservedAt = observation.ProviderObservedAt
			if authorized.policy.Action.Mutates() {
				record, recordErr := adapter.store.recordPreliminaryObservation(ctx, claim, observation)
				if recordErr != nil {
					err = recordErr
				} else {
					observationHash = record.hash.Digest
				}
			}
		}
	}
	if err != nil {
		_ = adapter.store.markAmbiguous(
			context.WithoutCancel(ctx), claim, authorized.envelope.Request,
			result.ExternalID, observationHash, "invalid_financial_observation",
			"accounting_conflict",
		)
		return effect.DispatchResult{Started: true, ExternalID: result.ExternalID}, ErrAmbiguous
	}
	if authorized.policy.Action.Mutates() {
		safeCode, incident := "financial_mutation_requires_reconciliation", "financial_ambiguity"
		if !observation.State.Inconclusive() {
			safeCode, incident = "dispatch_state_not_settlement_proof", "pending_as_settled"
		}
		if err := adapter.store.markAmbiguous(
			context.WithoutCancel(ctx), claim, authorized.envelope.Request,
			result.ExternalID, observationHash, safeCode, incident,
		); err != nil {
			return effect.DispatchResult{Started: true, ExternalID: result.ExternalID}, err
		}
		return effect.DispatchResult{
			Started: true, ExternalID: result.ExternalID,
			Observation: encoded, ObservedAt: result.ObservedAt,
		}, ErrAmbiguous
	}
	if dispatchErr == nil && len(encoded) != 0 && observation.Reconciled &&
		observation.Outcome.Authoritative &&
		observation.Authority != external.AuthorityUntrustedExternal &&
		observation.State.DefinitiveFailure() {
		if _, err := adapter.store.commitDefinitiveFailure(ctx, claim, observation); err != nil {
			return effect.DispatchResult{Started: false, ExternalID: result.ExternalID}, err
		}
		return effect.DispatchResult{Started: false, ExternalID: result.ExternalID}, ErrDenied
	}
	if dispatchErr != nil || len(encoded) == 0 || !observation.Reconciled ||
		!observation.Outcome.Authoritative ||
		observation.Authority == external.AuthorityUntrustedExternal ||
		!authorized.policy.Succeeds(observation.State) {
		_ = adapter.store.markAmbiguous(
			context.WithoutCancel(ctx), claim, authorized.envelope.Request,
			result.ExternalID, observationHash, "financial_observation_not_authoritative",
			"financial_ambiguity",
		)
		return effect.DispatchResult{Started: result.Started, ExternalID: result.ExternalID}, ErrAmbiguous
	}
	record, err := adapter.store.commitFinal(ctx, claim, authorized, observation)
	if err != nil {
		if errors.Is(err, ErrOutOfBandChange) {
			_ = adapter.store.openOutOfBandIncident(context.WithoutCancel(ctx), claim,
				authorized.envelope.Request, result.ExternalID, observationHash)
			return effect.DispatchResult{Started: true, ExternalID: result.ExternalID}, ErrAmbiguous
		}
		return effect.DispatchResult{Started: true, ExternalID: result.ExternalID}, err
	}
	return effect.DispatchResult{Started: true, ExternalID: result.ExternalID,
		Observation: record.canonical, ObservedAt: observation.ProviderObservedAt}, nil
}

func (adapter *Adapter) Probe(
	ctx context.Context,
	operation effect.Operation,
) (effect.ProbeResult, error) {
	if adapter == nil || adapter.store == nil || adapter.upstream == nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, ErrUnavailable
	}
	now, err := adapter.store.currentTime()
	if err != nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	authorized, err := adapter.authorize(ctx, now, operation, true)
	if err != nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	claim, err := adapter.store.claimProbe(ctx, authorized, operation)
	if err != nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	subOperation, err := upstreamOperation(operation, authorized)
	if err != nil {
		_ = adapter.store.markProbeInconclusive(context.WithoutCancel(ctx), claim,
			"", "", "financial_probe_encode_failed")
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	probe, probeErr := adapter.upstream.Probe(ctx, subOperation)
	if probeErr != nil && len(probe.Dispatch.Observation) == 0 {
		_ = adapter.store.markProbeInconclusive(context.WithoutCancel(ctx), claim,
			probe.Dispatch.ExternalID, "", "financial_probe_unavailable")
		return effect.ProbeResult{Outcome: skills.ProbeUnknown, Dispatch: probe.Dispatch,
			Reason: "financial_probe_unavailable"}, probeErr
	}
	observation, encoded, observationErr := adapter.financialObservation(
		now, authorized, operation.IdempotencyKey, probe.Dispatch.Observation, true,
	)
	if observationErr != nil {
		_ = adapter.store.markProbeInconclusive(context.WithoutCancel(ctx), claim,
			probe.Dispatch.ExternalID, "", "invalid_financial_probe_observation")
		return effect.ProbeResult{Outcome: skills.ProbeUnknown,
			Dispatch: effect.DispatchResult{ExternalID: probe.Dispatch.ExternalID},
			Reason:   "invalid_financial_probe_observation"}, observationErr
	}
	probe.Dispatch.Observation = encoded
	probe.Dispatch.ObservedAt = observation.ProviderObservedAt
	if observation.State.Inconclusive() || !observation.Outcome.Authoritative ||
		observation.Authority == external.AuthorityUntrustedExternal {
		record, recordErr := adapter.store.recordPreliminaryObservation(ctx, claim, observation)
		if recordErr != nil {
			return effect.ProbeResult{Outcome: skills.ProbeUnknown}, recordErr
		}
		_ = adapter.store.markProbeInconclusive(context.WithoutCancel(ctx), claim,
			probe.Dispatch.ExternalID, record.hash.Digest, "financial_state_still_pending")
		return effect.ProbeResult{Outcome: skills.ProbeUnknown, Dispatch: probe.Dispatch,
			Reason: "financial_state_still_pending"}, ErrAmbiguous
	}
	if observation.State.DefinitiveFailure() {
		record, err := adapter.store.commitDefinitiveFailure(ctx, claim, observation)
		if err != nil {
			return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
		}
		probe.Dispatch.Observation = record.canonical
		probe.Dispatch.Started = true
		return effect.ProbeResult{Outcome: skills.ProbeUnchanged, Dispatch: probe.Dispatch,
			Reason: "authoritative_financial_rejection", TerminalFailure: true}, nil
	}
	if !authorized.policy.Succeeds(observation.State) {
		record, recordErr := adapter.store.recordPreliminaryObservation(ctx, claim, observation)
		if recordErr != nil {
			return effect.ProbeResult{Outcome: skills.ProbeUnknown}, recordErr
		}
		_ = adapter.store.markAmbiguous(context.WithoutCancel(ctx), claim,
			authorized.envelope.Request, probe.Dispatch.ExternalID, record.hash.Digest,
			"financial_terminal_state_not_authorized", "accounting_conflict")
		return effect.ProbeResult{Outcome: skills.ProbeUnknown, Dispatch: probe.Dispatch,
			Reason: "financial_terminal_state_not_authorized"}, ErrAmbiguous
	}
	record, err := adapter.store.commitFinal(ctx, claim, authorized, observation)
	if err != nil {
		if errors.Is(err, ErrOutOfBandChange) {
			_ = adapter.store.openOutOfBandIncident(context.WithoutCancel(ctx), claim,
				authorized.envelope.Request, probe.Dispatch.ExternalID, "")
			return effect.ProbeResult{Outcome: skills.ProbeUnknown,
				Dispatch: effect.DispatchResult{ExternalID: probe.Dispatch.ExternalID},
				Reason:   "provider_resource_version_changed"}, ErrAmbiguous
		}
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	probe.Dispatch.Observation = record.canonical
	probe.Dispatch.Started = true
	return effect.ProbeResult{Outcome: skills.ProbeCompletedOutOfBand,
		Dispatch: probe.Dispatch, Reason: "authoritative_financial_reconciliation"}, nil
}

func upstreamOperation(operation effect.Operation, authorized authorizedOperation) (effect.Operation, error) {
	externalEnvelope := authorized.envelope.External
	externalEnvelope.Request.Body = append([]byte(nil), authorized.commandBody...)
	encoded, err := contracts.EncodeCanonical(&externalEnvelope)
	if err != nil {
		return effect.Operation{}, err
	}
	subOperation := operation
	subOperation.Name = authorized.policy.ExternalOperation
	subOperation.Input = encoded
	return subOperation, nil
}

func (adapter *Adapter) financialObservation(
	now time.Time,
	authorized authorizedOperation,
	idempotencyKey string,
	externalEncoded []byte,
	reconciled bool,
) (Observation, []byte, error) {
	externalObservation, err := contracts.DecodeCanonical[external.Observation, *external.Observation](externalEncoded)
	if err != nil {
		return Observation{}, nil, fmt.Errorf("financial adapter: external observation is invalid")
	}
	connection := authorized.connection.connection
	request := authorized.envelope.Request
	if externalObservation.OrganizationID != connection.OrganizationID ||
		externalObservation.ConnectionID != connection.ExternalConnectionID ||
		externalObservation.ConnectionVersion != connection.ExternalConnectionVersion ||
		externalObservation.Operation != authorized.policy.ExternalOperation ||
		externalObservation.Action != authorized.policy.ExternalAction ||
		externalObservation.IdempotencyKey != idempotencyKey ||
		externalObservation.AccountID != request.AccountID ||
		externalObservation.IdentityID != request.IdentityID {
		return Observation{}, nil, fmt.Errorf("financial adapter: external account or operation binding mismatch")
	}
	outcome, err := contracts.DecodeCanonical[ProviderOutcome, *ProviderOutcome](externalObservation.Output)
	if err != nil {
		return Observation{}, nil, fmt.Errorf("financial adapter: provider outcome is invalid")
	}
	if outcome.OrganizationID != connection.OrganizationID ||
		outcome.InitiativeID != request.InitiativeID || outcome.AccountID != request.AccountID ||
		outcome.IdentityID != request.IdentityID || outcome.Family != request.Family ||
		outcome.Action != request.Action || outcome.Asset != request.Amount.Asset ||
		outcome.Instrument != request.Instrument || outcome.Venue != request.Venue ||
		outcome.Rail != request.Rail || outcome.Counterparty != request.Counterparty ||
		outcome.DestinationHash != request.DestinationHash ||
		outcome.IdempotencyKey != idempotencyKey || outcome.ExternalID != externalObservation.ExternalID ||
		outcome.PrincipalMicrounits != request.NotionalMicrounits ||
		outcome.FeeMicrounits > request.FeeCeilingMicrounits || outcome.ValuationID != request.ValuationID ||
		outcome.ValuationVersion != request.ValuationVersion || outcome.ValuationHash != request.ValuationHash ||
		!outcome.ObservedAt.Equal(externalObservation.ProviderObservedAt) {
		return Observation{}, nil, fmt.Errorf("financial adapter: provider substituted financial scope or amount")
	}
	if outcome.Reference.NetworkID != connection.ProviderContract.NetworkID ||
		outcome.Reference.ChainID != connection.ProviderContract.ChainID {
		return Observation{}, nil, fmt.Errorf("financial adapter: provider network or chain binding mismatch")
	}
	if (connection.ProviderContract.Kind == ContractPaxeerEVM ||
		connection.ProviderContract.Kind == ContractLayerXAccount) &&
		outcome.Authoritative && !outcome.State.Inconclusive() && !outcome.State.DefinitiveFailure() &&
		outcome.Reference.Confirmations < connection.ProviderContract.RequiredConfirmations {
		return Observation{}, nil, fmt.Errorf("financial adapter: chain confirmation threshold is not satisfied")
	}
	if outcome.Authoritative && externalObservation.Authority == external.AuthorityUntrustedExternal {
		return Observation{}, nil, fmt.Errorf("financial adapter: untrusted transport claimed financial authority")
	}
	if outcome.Authoritative && !outcome.State.Inconclusive() && !outcome.State.DefinitiveFailure() &&
		externalObservation.State != external.ExternalCompleted {
		return Observation{}, nil, fmt.Errorf("financial adapter: terminal outcome lacks completed authoritative transport evidence")
	}
	if outcome.State.DefinitiveFailure() && externalObservation.State != external.ExternalCompleted &&
		externalObservation.State != external.ExternalRejected {
		return Observation{}, nil, fmt.Errorf("financial adapter: definitive rejection conflicts with transport state")
	}
	externalSum := sha256.Sum256(externalEncoded)
	outcomeCanonical, err := contracts.EncodeCanonical(&outcome)
	if err != nil || !bytes.Equal(outcomeCanonical, externalObservation.Output) {
		return Observation{}, nil, fmt.Errorf("financial adapter: provider outcome is not canonical")
	}
	outcomeSum := sha256.Sum256(outcomeCanonical)
	economicTruth := reconciled && authorized.policy.Succeeds(outcome.State) && outcome.Authoritative &&
		externalObservation.Authority != external.AuthorityUntrustedExternal &&
		!outcome.State.Inconclusive() && !outcome.State.DefinitiveFailure()
	observation := Observation{
		SchemaVersion: SchemaVersion, OrganizationID: connection.OrganizationID,
		ConnectionID: connection.ID, ConnectionVersion: connection.Version,
		Family: connection.Family, Operation: authorized.policy.Name, Action: authorized.policy.Action,
		InitiativeID: request.InitiativeID, Asset: request.Amount.Asset,
		Venue: request.Venue, Rail: request.Rail, Counterparty: request.Counterparty,
		DestinationHash: request.DestinationHash, ExternalID: externalObservation.ExternalID,
		IdempotencyKey: idempotencyKey, State: outcome.State,
		Authority: externalObservation.Authority, Reconciled: reconciled,
		EconomicTruth: economicTruth, ValuationTime: authorized.valuation.valuation.ObservedAt,
		ProviderObservedAt: externalObservation.ProviderObservedAt, CapturedAt: now,
		ExternalHash: contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(externalSum[:])},
		OutcomeHash:  contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(outcomeSum[:])},
		Outcome:      outcome,
	}
	encoded, err := contracts.EncodeCanonical(&observation)
	if err != nil {
		return Observation{}, nil, err
	}
	return observation, encoded, nil
}

func (adapter *Adapter) recordAuthorizationDenial(ctx context.Context, operation effect.Operation, denial error) {
	envelope, err := contracts.DecodeCanonical[Envelope, *Envelope](operation.Input)
	if err != nil {
		return
	}
	kind, safeCode := "limit_denial", "financial_authority_scope_denied"
	switch {
	case errors.Is(denial, ErrStaleValuation):
		kind, safeCode = "stale_valuation", "financial_valuation_not_current"
	case errors.Is(denial, ErrStaleRisk):
		kind, safeCode = "stale_risk", "financial_risk_state_not_current"
	case errors.Is(denial, ErrReserved):
		kind, safeCode = "reserved_action_denied", "founder_reserved_financial_action_denied"
	case errors.Is(denial, ErrCircuitOpen):
		kind, safeCode = "circuit_open", "financial_operation_circuit_open"
	}
	now, timeErr := adapter.store.currentTime()
	if timeErr != nil {
		return
	}
	tx, txErr := adapter.store.pool.Begin(ctx)
	if txErr != nil {
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	_ = adapter.store.recordIncidentTx(ctx, tx, operation.OrganizationID, "",
		envelope.ConnectionID, envelope.ConnectionVersion, operation.Name,
		operation.IdempotencyKey, kind, safeCode, now)
	_ = tx.Commit(ctx)
}

var _ effect.Adapter = (*Adapter)(nil)
