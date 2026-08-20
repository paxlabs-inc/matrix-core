package customer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/provider/external"
	"centra/workforce/internal/skills"
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
		return nil, fmt.Errorf("customer adapter: exact store, upstream, and connection identity are required")
	}
	return &Adapter{
		store: store, upstream: upstream, organizationID: organizationID,
		connectionID: connectionID, name: adapterName,
	}, nil
}

func LoadAdapters(
	ctx context.Context,
	store *Store,
	organizationID contracts.OrganizationID,
	upstreams map[string]effect.Adapter,
) ([]effect.Adapter, error) {
	if store == nil || len(upstreams) == 0 {
		return nil, fmt.Errorf("customer adapter: store and upstream adapter registry are required")
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
			return nil, fmt.Errorf("customer adapter: exact upstream adapter is missing or duplicated")
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
	kind := attemptDispatch
	if authorized.policy.CompensatesOperation != "" {
		kind = attemptCompensate
	}
	claim, err := adapter.store.claimAttempt(ctx, authorized, operation.IdempotencyKey, kind)
	if err != nil {
		incidentKind, safeCode := admissionIncident(err)
		if incidentKind != "" {
			_ = adapter.store.recordIncident(
				context.WithoutCancel(ctx), operation.OrganizationID,
				authorized.connection.connection.ID, authorized.connection.connection.Version,
				authorized.policy.Name, operation.IdempotencyKey, incidentKind, safeCode,
			)
		}
		return effect.DispatchResult{}, err
	}
	externalInput, err := contracts.EncodeCanonical(&authorized.envelope.External)
	if err != nil {
		_ = adapter.store.completeAttempt(context.WithoutCancel(ctx), claim,
			"failed", "external_envelope_encode_failed", "", "")
		return effect.DispatchResult{}, err
	}
	subOperation := operation
	subOperation.Name = authorized.policy.ExternalOperation
	subOperation.Input = externalInput
	result, dispatchErr := adapter.upstream.Dispatch(ctx, subOperation)
	if dispatchErr != nil {
		state, safeCode := "failed", "provider_dispatch_not_started"
		failureKind := "provider_outage"
		if authorized.policy.CompensatesOperation != "" {
			failureKind = "compensation_failed"
		}
		if result.Started {
			state, safeCode = "ambiguous", "customer_effect_outcome_unknown"
			_ = adapter.store.recordDrift(context.WithoutCancel(ctx), claim)
			_ = adapter.store.recordIncident(
				context.WithoutCancel(ctx), operation.OrganizationID,
				claim.connectionID, claim.version, claim.operation, claim.idempotencyKey,
				"customer_effect_ambiguity", safeCode,
			)
		} else {
			_ = adapter.store.recordIncident(
				context.WithoutCancel(ctx), operation.OrganizationID,
				claim.connectionID, claim.version, claim.operation, claim.idempotencyKey,
				failureKind, safeCode,
			)
		}
		observationHash := ""
		if len(result.Observation) != 0 {
			if _, encoded, hash, observationErr := adapter.customerObservation(
				ctx, now, authorized, operation.IdempotencyKey, result.Observation,
			); observationErr == nil {
				result.Observation = encoded
				observationHash = hash.Digest
			}
		}
		_ = adapter.store.completeAttempt(
			context.WithoutCancel(ctx), claim, state, safeCode,
			result.ExternalID, observationHash,
		)
		if result.Started {
			return result, ErrAmbiguous
		}
		return result, dispatchErr
	}
	observation, encoded, observationHash, err := adapter.customerObservation(
		ctx, now, authorized, operation.IdempotencyKey, result.Observation,
	)
	if err != nil {
		_ = adapter.store.completeAttempt(context.WithoutCancel(ctx), claim,
			"ambiguous", "invalid_customer_observation", result.ExternalID, "")
		_ = adapter.store.recordDrift(context.WithoutCancel(ctx), claim)
		kind := "observation_conflict"
		if errors.Is(err, errRecipientSubstitution) {
			kind = "recipient_substitution"
		} else if errors.Is(err, errAccountConfusion) {
			kind = "account_confusion"
		}
		_ = adapter.store.recordIncident(
			context.WithoutCancel(ctx), operation.OrganizationID,
			claim.connectionID, claim.version, claim.operation, claim.idempotencyKey,
			kind, "invalid_customer_observation",
		)
		return effect.DispatchResult{
			Started: true, ExternalID: result.ExternalID, ObservedAt: result.ObservedAt,
		}, ErrAmbiguous
	}
	if authorized.policy.Action.Mutates() &&
		(observation.State != external.ExternalCompleted ||
			observation.Authority == external.AuthorityUntrustedExternal) {
		_ = adapter.store.completeAttempt(context.WithoutCancel(ctx), claim,
			"ambiguous", "mutation_not_authoritatively_observed",
			result.ExternalID, observationHash.Digest)
		_ = adapter.store.recordDrift(context.WithoutCancel(ctx), claim)
		return effect.DispatchResult{
			Started: true, ExternalID: result.ExternalID,
			Observation: encoded, ObservedAt: observation.ProviderObservedAt,
		}, ErrAmbiguous
	}
	if err := adapter.store.completeAttempt(ctx, claim, "completed", "",
		result.ExternalID, observationHash.Digest); err != nil {
		return effect.DispatchResult{
			Started: true, ExternalID: result.ExternalID,
			Observation: encoded, ObservedAt: observation.ProviderObservedAt,
		}, err
	}
	if authorized.policy.CompensatesOperation != "" {
		_ = adapter.store.resolveDrift(
			context.WithoutCancel(ctx), operation.OrganizationID,
			authorized.connection.connection.ID, authorized.connection.connection.Version,
			authorized.policy.CompensatesOperation,
			authorized.envelope.Request.CompensatesKey, "compensated",
		)
	} else if authorized.policy.Action.Mutates() {
		_ = adapter.store.resolveDrift(
			context.WithoutCancel(ctx), operation.OrganizationID,
			authorized.connection.connection.ID, authorized.connection.connection.Version,
			authorized.policy.Name, operation.IdempotencyKey, "reconciled",
		)
	}
	return effect.DispatchResult{
		Started: true, ExternalID: result.ExternalID,
		Observation: encoded, ObservedAt: observation.ProviderObservedAt,
	}, nil
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
	claim, err := adapter.store.claimAttempt(ctx, authorized, operation.IdempotencyKey, attemptProbe)
	if err != nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	externalInput, err := contracts.EncodeCanonical(&authorized.envelope.External)
	if err != nil {
		_ = adapter.store.completeAttempt(context.WithoutCancel(ctx), claim,
			"failed", "external_probe_envelope_encode_failed", "", "")
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	subOperation := operation
	subOperation.Name = authorized.policy.ExternalOperation
	subOperation.Input = externalInput
	probe, probeErr := adapter.upstream.Probe(ctx, subOperation)
	if probeErr != nil || !probe.Outcome.Valid() {
		_ = adapter.store.completeAttempt(context.WithoutCancel(ctx), claim,
			"failed", "customer_probe_inconclusive", probe.Dispatch.ExternalID, "")
		_ = adapter.store.recordIncident(
			context.WithoutCancel(ctx), operation.OrganizationID,
			claim.connectionID, claim.version, claim.operation, claim.idempotencyKey,
			"provider_outage", "customer_probe_inconclusive",
		)
		return effect.ProbeResult{
			Outcome: skills.ProbeUnknown, Dispatch: probe.Dispatch,
			Reason: "customer_probe_inconclusive",
		}, probeErr
	}
	observationHash := ""
	if len(probe.Dispatch.Observation) != 0 {
		observation, encoded, hash, err := adapter.customerObservation(
			ctx, now, authorized, operation.IdempotencyKey, probe.Dispatch.Observation,
		)
		if err != nil {
			_ = adapter.store.completeAttempt(context.WithoutCancel(ctx), claim,
				"failed", "invalid_customer_probe_observation",
				probe.Dispatch.ExternalID, "")
			return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
		}
		probe.Dispatch.Observation = encoded
		probe.Dispatch.ObservedAt = observation.ProviderObservedAt
		observationHash = hash.Digest
	}
	state := "completed"
	if probe.Outcome == skills.ProbeUnknown {
		state = "failed"
	}
	if err := adapter.store.completeAttempt(ctx, claim, state, "",
		probe.Dispatch.ExternalID, observationHash); err != nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	if probe.Outcome != skills.ProbeUnknown {
		_ = adapter.store.resolveDrift(
			context.WithoutCancel(ctx), operation.OrganizationID,
			claim.connectionID, claim.version, claim.operation,
			claim.idempotencyKey, "reconciled",
		)
	}
	return effect.ProbeResult{
		Outcome: probe.Outcome, Dispatch: probe.Dispatch,
		Reason: "authoritative_customer_probe_" + string(probe.Outcome),
	}, nil
}

var (
	errRecipientSubstitution = errors.New("customer adapter: provider recipient substitution")
	errAccountConfusion      = errors.New("customer adapter: provider account confusion")
)

func (adapter *Adapter) customerObservation(
	ctx context.Context,
	now time.Time,
	authorized authorizedOperation,
	idempotencyKey string,
	externalEncoded []byte,
) (Observation, []byte, contracts.ContentHash, error) {
	var externalObservation external.Observation
	if err := decodeStrict(externalEncoded, &externalObservation); err != nil ||
		externalObservation.Validate() != nil {
		return Observation{}, nil, contracts.ContentHash{}, fmt.Errorf("customer adapter: external observation is invalid")
	}
	canonicalExternal, err := contracts.EncodeCanonical(&externalObservation)
	if err != nil || !bytes.Equal(canonicalExternal, externalEncoded) {
		return Observation{}, nil, contracts.ContentHash{}, fmt.Errorf("customer adapter: external observation is not canonical")
	}
	connection := authorized.connection.connection
	request := authorized.envelope.Request
	if externalObservation.OrganizationID != connection.OrganizationID ||
		externalObservation.ConnectionID != connection.ExternalConnectionID ||
		externalObservation.ConnectionVersion != connection.ExternalConnectionVersion ||
		externalObservation.Operation != authorized.policy.ExternalOperation ||
		externalObservation.Action != authorized.policy.ExternalAction ||
		externalObservation.IdempotencyKey != idempotencyKey {
		return Observation{}, nil, contracts.ContentHash{}, fmt.Errorf("customer adapter: external observation binding mismatch")
	}
	if externalObservation.AccountID != request.AccountID ||
		externalObservation.IdentityID != request.IdentityID {
		return Observation{}, nil, contracts.ContentHash{}, errAccountConfusion
	}
	var outcome ProviderOutcome
	if err := decodeStrict(externalObservation.Output, &outcome); err != nil || outcome.Validate() != nil {
		return Observation{}, nil, contracts.ContentHash{}, fmt.Errorf("customer adapter: provider outcome is invalid")
	}
	canonicalOutcome, err := contracts.EncodeCanonical(&outcome)
	if err != nil || !bytes.Equal(canonicalOutcome, externalObservation.Output) {
		return Observation{}, nil, contracts.ContentHash{}, fmt.Errorf("customer adapter: provider outcome is not canonical")
	}
	if outcome.CustomerID != request.CustomerID || outcome.RecipientRef != request.RecipientRef ||
		outcome.DestinationHash != request.DestinationHash || outcome.Channel != request.Channel ||
		outcome.Purpose != request.Purpose {
		return Observation{}, nil, contracts.ContentHash{}, errRecipientSubstitution
	}
	if outcome.AccountID != request.AccountID || outcome.IdentityID != request.IdentityID {
		return Observation{}, nil, contracts.ContentHash{}, errAccountConfusion
	}
	if outcome.ExternalID != externalObservation.ExternalID ||
		outcome.State != externalObservation.State ||
		!outcome.ObservedAt.Equal(externalObservation.ProviderObservedAt) {
		return Observation{}, nil, contracts.ContentHash{}, fmt.Errorf("customer adapter: provider outcome conflicts with authoritative observation")
	}
	externalSum := sha256.Sum256(externalEncoded)
	outcomeSum := sha256.Sum256(canonicalOutcome)
	observation := Observation{
		SchemaVersion: SchemaVersion, OrganizationID: connection.OrganizationID,
		ConnectionID: connection.ID, ConnectionVersion: connection.Version,
		Family: connection.Family, Operation: authorized.policy.Name,
		Action: authorized.policy.Action, CustomerID: request.CustomerID,
		CustomerVersion: request.CustomerVersion, RecipientHash: request.DestinationHash,
		ConsentID: request.ConsentID, ConsentVersion: request.ConsentVersion,
		Channel: request.Channel, Purpose: request.Purpose, Jurisdiction: request.Jurisdiction,
		DataClassification: request.DataClassification,
		ExternalID:         externalObservation.ExternalID, IdempotencyKey: idempotencyKey,
		State: externalObservation.State, Authority: externalObservation.Authority,
		UntrustedData: true, ProviderObservedAt: externalObservation.ProviderObservedAt,
		CapturedAt:   now,
		ExternalHash: contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(externalSum[:])},
		OutcomeHash:  contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(outcomeSum[:])},
		Outcome:      outcome,
	}
	if err := observation.Validate(); err != nil {
		return Observation{}, nil, contracts.ContentHash{}, err
	}
	hash, err := adapter.store.recordObservation(ctx, observation)
	if err != nil {
		return Observation{}, nil, contracts.ContentHash{}, err
	}
	encoded, err := contracts.EncodeCanonical(&observation)
	if err != nil {
		return Observation{}, nil, contracts.ContentHash{}, err
	}
	return observation, encoded, hash, nil
}

func admissionIncident(err error) (string, string) {
	switch {
	case errors.Is(err, ErrCircuitOpen):
		return "circuit_open", "customer_operation_circuit_open"
	case errors.Is(err, ErrFrequencyLimit):
		return "frequency_exhausted", "customer_frequency_limit_reached"
	case errors.Is(err, ErrCapacity):
		return "capacity_exhausted", "customer_capacity_exhausted"
	case errors.Is(err, ErrUnsubscribed):
		return "unsubscribed", "customer_unsubscribed"
	case errors.Is(err, ErrConsent):
		return "consent_withdrawn", "current_customer_consent_missing"
	case errors.Is(err, ErrAmbiguous):
		return "duplicate_communication", "customer_effect_requires_reconciliation"
	default:
		return "", ""
	}
}

func (adapter *Adapter) recordAuthorizationDenial(
	ctx context.Context,
	operation effect.Operation,
	denial error,
) {
	var envelope Envelope
	if decodeStrict(operation.Input, &envelope) != nil ||
		token("connection id", envelope.ConnectionID) != nil || envelope.ConnectionVersion == 0 {
		return
	}
	kind, safeCode := "privacy_scope_denied", "customer_authority_scope_denied"
	switch {
	case errors.Is(denial, ErrPromptInjection):
		kind, safeCode = "prompt_injection", "untrusted_payload_authority_injection"
	case errors.Is(denial, ErrUnsubscribed):
		kind, safeCode = "unsubscribed", "customer_unsubscribed"
	case errors.Is(denial, ErrConsent):
		kind, safeCode = "consent_withdrawn", "current_customer_consent_missing"
	}
	_ = adapter.store.recordIncident(
		ctx, operation.OrganizationID, envelope.ConnectionID,
		envelope.ConnectionVersion, operation.Name, operation.IdempotencyKey,
		kind, safeCode,
	)
}

var _ effect.Adapter = (*Adapter)(nil)
