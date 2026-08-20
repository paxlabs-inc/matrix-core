package commercialcapability

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/lease"
	"centra/workforce/internal/skills"
)

type AdapterInput struct {
	SchemaVersion  string                `json:"schema_version"`
	Grant          lease.Grant           `json:"grant"`
	SeatID         contracts.SeatID      `json:"seat_id"`
	IntentID       contracts.IntentID    `json:"intent_id"`
	IdempotencyKey string                `json:"idempotency_key"`
	DataScopes     []string              `json:"data_scopes"`
	SourceDigest   contracts.ContentHash `json:"source_digest"`
	Body           RecordBody            `json:"body"`
}

func (value AdapterInput) Validate() error {
	return value.ValidateAt(value.Body.EffectiveAt)
}

func (value AdapterInput) ValidateAt(now time.Time) error {
	if value.SchemaVersion != SchemaVersion || value.Grant.State != lease.StateActive ||
		value.Grant.Fence == 0 {
		return fmt.Errorf("commercial capability adapter: schema or active fence is invalid")
	}
	if err := value.Grant.Request.Validate(); err != nil {
		return err
	}
	if err := value.Grant.Fence.Validate(); err != nil {
		return err
	}
	for _, token := range []struct{ name, value string }{
		{"seat_id", string(value.SeatID)}, {"intent_id", string(value.IntentID)},
		{"idempotency_key", value.IdempotencyKey},
	} {
		if err := validateToken(token.name, token.value); err != nil {
			return err
		}
	}
	if err := value.SourceDigest.Validate(); err != nil {
		return fmt.Errorf("commercial capability adapter: source digest: %w", err)
	}
	if err := value.Body.Validate(); err != nil {
		return err
	}
	profile, err := Profile(value.Body.SkillID)
	if err != nil {
		return err
	}
	expectedScopes := append([]string(nil), profile.DataScopes...)
	actualScopes := append([]string(nil), value.DataScopes...)
	slices.Sort(expectedScopes)
	slices.Sort(actualScopes)
	if !slices.Equal(actualScopes, expectedScopes) || hasDuplicate(actualScopes) {
		return fmt.Errorf("commercial capability adapter: data scopes exceed the signed skill")
	}
	if value.Grant.OrganizationID != value.Body.OrganizationID ||
		value.Grant.SeatID != value.SeatID || value.Body.AuthorSeatID != value.SeatID ||
		contracts.IntentID(value.Grant.NodeID) != value.IntentID ||
		!validUTC(now) || !value.Grant.ExpiresAt.After(now) ||
		!value.Body.FreshUntil.After(now) {
		return fmt.Errorf("commercial capability adapter: lease, seat, organization, or freshness mismatch")
	}
	return nil
}

type Adapter struct {
	now func() time.Time
}

func NewAdapter(now func() time.Time) (*Adapter, error) {
	if now == nil {
		return nil, fmt.Errorf("commercial capability adapter: UTC time source is required")
	}
	return &Adapter{now: now}, nil
}

func (adapter *Adapter) Name() string {
	if adapter == nil {
		return ""
	}
	return commercialCapabilityProvider
}

func (adapter *Adapter) Dispatch(ctx context.Context, operation effect.Operation) (effect.DispatchResult, error) {
	if adapter == nil || adapter.now == nil {
		return effect.DispatchResult{}, fmt.Errorf("commercial capability adapter is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return effect.DispatchResult{}, err
	}
	input, err := contracts.DecodeStrict[AdapterInput, *AdapterInput](operation.Input)
	if err != nil {
		return effect.DispatchResult{}, err
	}
	now := adapter.now()
	if err := input.ValidateAt(now); err != nil {
		return effect.DispatchResult{}, err
	}
	if operation.OrganizationID != input.Body.OrganizationID || operation.SeatID != input.SeatID ||
		operation.LeaseID != input.Grant.ID || operation.Fence != input.Grant.Fence ||
		operation.Name != string(input.Body.SkillID) || operation.IdempotencyKey != input.IdempotencyKey {
		return effect.DispatchResult{}, fmt.Errorf("commercial capability adapter: compiled authority mismatch")
	}
	hash, err := contracts.HashCanonical(&input.Body)
	if err != nil {
		return effect.DispatchResult{}, err
	}
	observation, err := json.Marshal(struct {
		SchemaVersion             string                `json:"schema_version"`
		Outcome                   string                `json:"outcome"`
		RecordID                  RecordID              `json:"record_id"`
		Kind                      RecordKind            `json:"kind"`
		RecordHash                contracts.ContentHash `json:"record_hash"`
		RequiresIndependentReview bool                  `json:"requires_independent_review"`
		EffectAuthority           string                `json:"effect_authority"`
	}{
		SchemaVersion: SchemaVersion, Outcome: "proposed", RecordID: input.Body.ID,
		Kind: input.Body.Kind, RecordHash: hash, RequiresIndependentReview: true,
		EffectAuthority: "none",
	})
	if err != nil {
		return effect.DispatchResult{}, fmt.Errorf("commercial capability adapter: encode proposal: %w", err)
	}
	return effect.DispatchResult{
		Started: true, ExternalID: commercialCapabilityProvider + ":" + hash.Digest[:32],
		Observation: observation, ObservedAt: now,
	}, nil
}

func (adapter *Adapter) Probe(ctx context.Context, operation effect.Operation) (effect.ProbeResult, error) {
	result, err := adapter.Dispatch(ctx, operation)
	if err != nil {
		return effect.ProbeResult{
			Outcome: skills.ProbeUnknown, Dispatch: result,
			Reason: "typed_commercial_capability_unavailable",
		}, err
	}
	return effect.ProbeResult{
		Outcome: skills.ProbeCompletedOutOfBand, Dispatch: result,
		Reason: "content_addressed_commercial_capability_proposal",
	}, nil
}

var _ effect.Adapter = (*Adapter)(nil)
