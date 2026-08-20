package external

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/skills"
)

type Adapter struct {
	store          *Store
	organizationID contracts.OrganizationID
	connectionID   string
	name           string
	transport      *http.Transport
}

type providerCall struct {
	response  ProviderResponse
	started   bool
	authority ObservationAuthority
	untrusted bool
	safeCode  string
}

func NewAdapter(
	store *Store,
	organizationID contracts.OrganizationID,
	connectionID string,
	adapterName string,
) (*Adapter, error) {
	if store == nil || token("organization id", string(organizationID)) != nil ||
		token("connection id", connectionID) != nil ||
		token("adapter name", adapterName) != nil {
		return nil, fmt.Errorf("external adapter: store and exact connection identity are required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{
		Timeout: 30 * time.Second, KeepAlive: 30 * time.Second,
	}).DialContext
	transport.ForceAttemptHTTP2 = true
	transport.MaxConnsPerHost = 4
	transport.MaxIdleConnsPerHost = 4
	transport.MaxResponseHeaderBytes = 1 << 20
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &Adapter{
		store: store, organizationID: organizationID,
		connectionID: connectionID, name: adapterName, transport: transport,
	}, nil
}

func LoadAdapters(
	ctx context.Context,
	store *Store,
	organizationID contracts.OrganizationID,
) ([]effect.Adapter, error) {
	if store == nil {
		return nil, fmt.Errorf("external adapter: store is required")
	}
	views, err := store.ListActiveConnections(ctx, organizationID)
	if err != nil {
		return nil, err
	}
	registry := make(map[string]bool, len(views))
	adapters := make([]effect.Adapter, 0, len(views))
	for _, view := range views {
		connection := view.Connection
		if connection.OrganizationID != organizationID || registry[connection.AdapterName] {
			return nil, fmt.Errorf("external adapter: active adapter name is duplicated or misbound")
		}
		adapter, err := NewAdapter(
			store, organizationID, connection.ID, connection.AdapterName,
		)
		if err != nil {
			return nil, err
		}
		registry[connection.AdapterName] = true
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
	if adapter == nil || adapter.store == nil || adapter.transport == nil {
		return effect.DispatchResult{}, ErrUnavailable
	}
	now, err := adapter.store.currentTime()
	if err != nil {
		return effect.DispatchResult{}, err
	}
	authorized, err := adapter.authorize(ctx, now, operation)
	if err != nil {
		return effect.DispatchResult{}, err
	}
	defer wipeCredential(&authorized.loaded.credential)
	kind := attemptDispatch
	if authorized.policy.CompensatesOperation != "" {
		kind = attemptCompensate
	}
	claim, err := adapter.store.claimAttempt(
		ctx, authorized.loaded.connection, authorized.policy,
		operation.IdempotencyKey, authorized.requestHash, kind,
	)
	if err != nil {
		incidentKind, safeCode := "", ""
		switch {
		case errors.Is(err, ErrCircuitOpen):
			incidentKind, safeCode = "circuit_open", "operation_circuit_open"
		case errors.Is(err, ErrCapacity), errors.Is(err, ErrRetryExhausted):
			incidentKind, safeCode = "capacity_exhausted", "operation_budget_exhausted"
		case errors.Is(err, ErrDenied):
			incidentKind, safeCode = "drift_ceiling", "drift_blind_ceiling_reached"
		}
		if incidentKind != "" {
			_ = adapter.store.recordIncident(
				context.WithoutCancel(ctx), operation.OrganizationID,
				authorized.loaded.connection.ID, authorized.loaded.connection.Version,
				operation.Name, operation.IdempotencyKey, incidentKind, safeCode,
			)
		}
		return effect.DispatchResult{}, err
	}
	call, callErr := adapter.callProvider(ctx, authorized, operation, false)
	if callErr != nil {
		state := "failed"
		if call.started && authorized.policy.Action.Mutates() {
			state = "ambiguous"
		}
		_ = adapter.store.completeAttempt(
			context.WithoutCancel(ctx), claim, state, call.safeCode,
			call.response.ExternalID, "",
		)
		if call.started && authorized.policy.Action.Mutates() {
			_ = adapter.store.recordIncident(
				context.WithoutCancel(ctx), operation.OrganizationID,
				authorized.loaded.connection.ID, authorized.loaded.connection.Version,
				operation.Name, operation.IdempotencyKey,
				"external_ambiguity", call.safeCode,
			)
			return effect.DispatchResult{
				Started: true, ExternalID: call.response.ExternalID,
				ObservedAt: now,
			}, fmt.Errorf("%w: %s", ErrAmbiguous, call.safeCode)
		}
		return effect.DispatchResult{Started: false}, callErr
	}
	observation, err := adapter.observation(
		now, authorized, operation, call,
	)
	if err != nil {
		_ = adapter.store.completeAttempt(
			context.WithoutCancel(ctx), claim, "ambiguous", "invalid_provider_observation",
			call.response.ExternalID, "",
		)
		return effect.DispatchResult{
			Started: call.started, ExternalID: call.response.ExternalID,
			ObservedAt: now,
		}, fmt.Errorf("%w: invalid_provider_observation", ErrAmbiguous)
	}
	observationHash, err := adapter.store.recordObservation(ctx, observation)
	if err != nil {
		_ = adapter.store.completeAttempt(
			context.WithoutCancel(ctx), claim, "ambiguous", "observation_commit_failed",
			call.response.ExternalID, "",
		)
		return effect.DispatchResult{
			Started: call.started, ExternalID: call.response.ExternalID,
			ObservedAt: now,
		}, err
	}
	encoded, err := contracts.EncodeCanonical(&observation)
	if err != nil {
		_ = adapter.store.completeAttempt(
			context.WithoutCancel(ctx), claim, "ambiguous", "observation_encode_failed",
			call.response.ExternalID, observationHash.Digest,
		)
		return effect.DispatchResult{}, err
	}
	completed := call.response.State == ExternalCompleted
	authoritative := call.authority != AuthorityUntrustedExternal
	if call.response.State == ExternalRejected && authoritative {
		if authorized.policy.Action.Mutates() && !authorized.policy.ProbeAuthoritative {
			_ = adapter.store.resolveDrift(
				context.WithoutCancel(ctx), operation.OrganizationID,
				authorized.loaded.connection.ID, authorized.loaded.connection.Version,
				operation.Name, operation.IdempotencyKey, "reconciled",
			)
		}
		if err := adapter.store.completeAttempt(
			ctx, claim, "completed", "provider_authoritative_rejection",
			call.response.ExternalID, observationHash.Digest,
		); err != nil {
			return effect.DispatchResult{Started: true}, err
		}
		return effect.DispatchResult{Started: false}, fmt.Errorf(
			"%w: provider_authoritative_rejection", ErrDenied,
		)
	}
	if authorized.policy.Action.Mutates() &&
		(!completed || !authorized.policy.DispatchAuthoritative || !authoritative) {
		_ = adapter.store.completeAttempt(
			context.WithoutCancel(ctx), claim, "ambiguous", "mutation_not_authoritatively_observed",
			call.response.ExternalID, observationHash.Digest,
		)
		_ = adapter.store.recordIncident(
			context.WithoutCancel(ctx), operation.OrganizationID,
			authorized.loaded.connection.ID, authorized.loaded.connection.Version,
			operation.Name, operation.IdempotencyKey,
			"external_ambiguity", "mutation_not_authoritatively_observed",
		)
		return effect.DispatchResult{
			Started: true, ExternalID: call.response.ExternalID,
			Observation: encoded, ObservedAt: observation.ProviderObservedAt,
		}, fmt.Errorf("%w: mutation_not_authoritatively_observed", ErrAmbiguous)
	}
	if !completed {
		_ = adapter.store.completeAttempt(
			context.WithoutCancel(ctx), claim, "failed", "external_state_not_completed",
			call.response.ExternalID, observationHash.Digest,
		)
		return effect.DispatchResult{Started: false}, fmt.Errorf(
			"%w: external_state_not_completed", ErrDenied,
		)
	}
	if authorized.policy.CompensatesOperation != "" {
		_ = adapter.store.resolveDrift(
			context.WithoutCancel(ctx), operation.OrganizationID,
			authorized.loaded.connection.ID, authorized.loaded.connection.Version,
			authorized.policy.CompensatesOperation,
			authorized.envelope.Request.CompensatesKey, "compensated",
		)
	}
	if authorized.policy.Action.Mutates() && authorized.policy.DispatchAuthoritative &&
		!authorized.policy.ProbeAuthoritative {
		_ = adapter.store.resolveDrift(
			context.WithoutCancel(ctx), operation.OrganizationID,
			authorized.loaded.connection.ID, authorized.loaded.connection.Version,
			operation.Name, operation.IdempotencyKey, "reconciled",
		)
	}
	if err := adapter.store.completeAttempt(
		ctx, claim, "completed", "", call.response.ExternalID, observationHash.Digest,
	); err != nil {
		return effect.DispatchResult{
			Started: call.started, ExternalID: call.response.ExternalID,
			Observation: encoded, ObservedAt: observation.ProviderObservedAt,
		}, err
	}
	return effect.DispatchResult{
		Started: true, ExternalID: call.response.ExternalID,
		Observation: encoded, ObservedAt: observation.ProviderObservedAt,
	}, nil
}

func (adapter *Adapter) Probe(
	ctx context.Context,
	operation effect.Operation,
) (effect.ProbeResult, error) {
	if adapter == nil || adapter.store == nil || adapter.transport == nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, ErrUnavailable
	}
	now, err := adapter.store.currentTime()
	if err != nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	authorized, err := adapter.authorize(ctx, now, operation)
	if err != nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	defer wipeCredential(&authorized.loaded.credential)
	claim, err := adapter.store.claimAttempt(
		ctx, authorized.loaded.connection, authorized.policy,
		operation.IdempotencyKey, authorized.requestHash, attemptProbe,
	)
	if err != nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	call, callErr := adapter.callProvider(ctx, authorized, operation, true)
	if callErr != nil {
		_ = adapter.store.completeAttempt(
			context.WithoutCancel(ctx), claim, "failed", call.safeCode,
			call.response.ExternalID, "",
		)
		return effect.ProbeResult{
			Outcome: skills.ProbeUnknown,
			Dispatch: effect.DispatchResult{
				Started: false, ExternalID: call.response.ExternalID,
			},
			Reason: call.safeCode,
		}, callErr
	}
	observation, err := adapter.observation(now, authorized, operation, call)
	if err != nil {
		_ = adapter.store.completeAttempt(
			context.WithoutCancel(ctx), claim, "failed", "invalid_probe_observation",
			call.response.ExternalID, "",
		)
		return effect.ProbeResult{
			Outcome: skills.ProbeUnknown, Reason: "invalid_probe_observation",
		}, err
	}
	observationHash, err := adapter.store.recordObservation(ctx, observation)
	if err != nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	encoded, err := contracts.EncodeCanonical(&observation)
	if err != nil {
		_ = adapter.store.completeAttempt(
			context.WithoutCancel(ctx), claim, "failed", "probe_observation_encode_failed",
			call.response.ExternalID, observationHash.Digest,
		)
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	outcome := skills.ProbeUnknown
	if authorized.policy.ProbeAuthoritative && call.authority != AuthorityUntrustedExternal {
		switch call.response.State {
		case ExternalCompleted:
			outcome = skills.ProbeCompletedOutOfBand
		case ExternalRejected:
			outcome = skills.ProbeUnchanged
		case ExternalReversed:
			outcome = skills.ProbeReversed
		case ExternalDrifted:
			outcome = skills.ProbeDrifted
		case ExternalConflicted:
			outcome = skills.ProbeConflicted
		case ExternalPending, ExternalUnknown:
			outcome = skills.ProbeUnknown
		}
	}
	if outcome == skills.ProbeCompletedOutOfBand {
		_ = adapter.store.resolveDrift(
			context.WithoutCancel(ctx), operation.OrganizationID,
			authorized.loaded.connection.ID, authorized.loaded.connection.Version,
			operation.Name, operation.IdempotencyKey, "reconciled",
		)
	}
	if err := adapter.store.completeAttempt(
		ctx, claim, "completed", "", call.response.ExternalID, observationHash.Digest,
	); err != nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	dispatch := effect.DispatchResult{
		Started:     outcome == skills.ProbeCompletedOutOfBand,
		ExternalID:  call.response.ExternalID,
		Observation: encoded, ObservedAt: observation.ProviderObservedAt,
	}
	if outcome != skills.ProbeCompletedOutOfBand {
		dispatch.Observation = nil
	}
	return effect.ProbeResult{
		Outcome: outcome, Dispatch: dispatch,
		Reason: "authoritative_external_probe_" + string(outcome),
	}, nil
}

func (adapter *Adapter) observation(
	now time.Time,
	authorized authorizedOperation,
	operation effect.Operation,
	call providerCall,
) (Observation, error) {
	sum := sha256.Sum256(call.response.Output)
	outputHash := contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
	value := Observation{
		SchemaVersion:      SchemaVersion,
		OrganizationID:     operation.OrganizationID,
		ConnectionID:       authorized.loaded.connection.ID,
		ConnectionVersion:  authorized.loaded.connection.Version,
		Family:             authorized.loaded.connection.Family,
		Provider:           authorized.loaded.connection.Provider,
		Operation:          operation.Name,
		Action:             authorized.policy.Action,
		TargetOrigin:       authorized.origin,
		AccountID:          authorized.loaded.connection.AccountID,
		IdentityID:         authorized.loaded.connection.IdentityID,
		DataClassification: authorized.envelope.Request.DataClassification,
		ExternalID:         call.response.ExternalID,
		IdempotencyKey:     operation.IdempotencyKey,
		State:              call.response.State,
		Authority:          call.authority,
		UntrustedData:      call.untrusted,
		FinalURL:           call.response.FinalURL,
		ResourceVersion:    call.response.ResourceVersion,
		ProviderObservedAt: call.response.ObservedAt,
		CapturedAt:         now,
		OutputHash:         outputHash,
		Output:             append([]byte(nil), call.response.Output...),
	}
	if err := value.Validate(); err != nil {
		return Observation{}, err
	}
	return value, nil
}

var _ effect.Adapter = (*Adapter)(nil)

func wipeCredential(value *CredentialMaterial) {
	if value == nil {
		return
	}
	for index := range value.Secret {
		value.Secret[index] = 0
	}
	value.Secret = nil
}
