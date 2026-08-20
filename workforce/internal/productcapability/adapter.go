package productcapability

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/lease"
	"centra/workforce/internal/projectbrain"
	"centra/workforce/internal/skills"
)

const productCapabilityProvider = "product_capability"

// AdapterInput is the exact typed proposal entering the internal capability
// adapter after the effect gateway has compiled its skill and authority.
type AdapterInput struct {
	SchemaVersion  string                      `json:"schema_version"`
	Grant          lease.Grant                 `json:"grant"`
	OrganizationID contracts.OrganizationID    `json:"organization_id"`
	InitiativeID   InitiativeID                `json:"initiative_id"`
	ProjectID      contracts.ProjectID         `json:"project_id"`
	WorkspaceID    contracts.WorkspaceID       `json:"workspace_id"`
	SeatID         contracts.SeatID            `json:"seat_id"`
	IntentID       contracts.IntentID          `json:"intent_id"`
	SkillID        contracts.SkillID           `json:"skill_id"`
	IdempotencyKey string                      `json:"idempotency_key"`
	Objective      string                      `json:"objective"`
	RecordID       string                      `json:"record_id"`
	Kind           ArtifactKind                `json:"kind"`
	Summary        string                      `json:"summary"`
	Evidence       []contracts.EvidenceRef     `json:"evidence"`
	DataScopes     []string                    `json:"data_scopes"`
	Source         *projectbrain.GraphSnapshot `json:"source"`
	ObservedAt     time.Time                   `json:"observed_at"`
	EffectiveAt    time.Time                   `json:"effective_at"`
	FreshUntil     time.Time                   `json:"fresh_until"`
}

// Validate performs structural validation for canonical decoding.
func (value AdapterInput) Validate() error {
	return value.ValidateAt(value.EffectiveAt)
}

// ValidateAt verifies the exact live lease, fence, skill, artifact, source,
// evidence, and resource scopes without accepting model-defined authority.
func (value AdapterInput) ValidateAt(now time.Time) error {
	if value.SchemaVersion != contracts.SchemaVersionV1 ||
		value.Grant.State != lease.StateActive || value.Grant.Fence == 0 {
		return fmt.Errorf("product capability adapter: schema or active fence is invalid")
	}
	if err := value.Grant.Request.Validate(); err != nil {
		return err
	}
	if err := value.Grant.Fence.Validate(); err != nil {
		return err
	}
	for name, tokenValue := range map[string]string{
		"organization_id": string(value.OrganizationID),
		"initiative_id":   string(value.InitiativeID),
		"project_id":      string(value.ProjectID),
		"workspace_id":    string(value.WorkspaceID),
		"seat_id":         string(value.SeatID),
		"intent_id":       string(value.IntentID),
		"skill_id":        string(value.SkillID),
		"idempotency_key": value.IdempotencyKey,
		"record_id":       value.RecordID,
	} {
		if err := validateToken(name, tokenValue); err != nil {
			return err
		}
	}
	expectedKind, ok := artifactKindForSkill(value.SkillID)
	if !ok || expectedKind != value.Kind {
		return fmt.Errorf("product capability adapter: skill and artifact kind disagree")
	}
	if value.Grant.OrganizationID != value.OrganizationID ||
		value.Grant.SeatID != value.SeatID ||
		contracts.IntentID(value.Grant.NodeID) != value.IntentID ||
		!validUTC(now) || !value.Grant.ExpiresAt.After(now) {
		return fmt.Errorf("product capability adapter: lease scope is stale or unrelated")
	}
	if strings.TrimSpace(value.Objective) == "" || len(value.Objective) > 4096 ||
		strings.TrimSpace(value.Summary) == "" || len(value.Summary) > 4096 {
		return fmt.Errorf("product capability adapter: objective or summary is invalid")
	}
	if len(value.Evidence) == 0 || len(value.Evidence) > 128 {
		return fmt.Errorf("product capability adapter: evidence is outside bounds")
	}
	for _, evidence := range value.Evidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
	}
	if err := validateTokenSet("data scope", value.DataScopes, 1, 32); err != nil {
		return err
	}
	expectedScopes := dataScopesForSkill(value.SkillID)
	if !sameStringSet(value.DataScopes, expectedScopes) {
		return fmt.Errorf("product capability adapter: data scopes exceed the skill contract")
	}
	if !validUTC(value.ObservedAt) || !validUTC(value.EffectiveAt) ||
		!validUTC(value.FreshUntil) || value.EffectiveAt.Before(value.ObservedAt) ||
		!value.FreshUntil.After(now) {
		return fmt.Errorf("product capability adapter: evidence times are invalid")
	}
	if engineeringArtifact(value.Kind) != (value.Source != nil) {
		return fmt.Errorf("product capability adapter: source binding is inconsistent")
	}
	if value.Source != nil {
		if err := value.Source.Validate(); err != nil || !value.Source.Fresh ||
			value.Source.CapturedAt.After(value.EffectiveAt) {
			return fmt.Errorf("product capability adapter: source is stale or invalid")
		}
	}
	return nil
}

// Adapter is the real deterministic internal provider that validates and
// content-addresses Product, Design, Reliability, and Analytics proposals. It
// owns no external credentials and never turns a proposal into verified truth.
type Adapter struct {
	now func() time.Time
}

// NewAdapter constructs the product capability provider.
func NewAdapter(now func() time.Time) (*Adapter, error) {
	if now == nil {
		return nil, fmt.Errorf("product capability adapter: UTC time source is required")
	}
	return &Adapter{now: now}, nil
}

// Name returns the exact provider identity declared by capability contracts.
func (adapter *Adapter) Name() string {
	if adapter == nil {
		return ""
	}
	return productCapabilityProvider
}

// Dispatch validates the live compiled authority and emits one immutable
// content-addressed proposal observation. Verification and Store.Commit remain
// separate independent boundaries.
func (adapter *Adapter) Dispatch(
	ctx context.Context,
	operation effect.Operation,
) (effect.DispatchResult, error) {
	if adapter == nil || adapter.now == nil {
		return effect.DispatchResult{}, fmt.Errorf("product capability adapter is unavailable")
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
	if operation.OrganizationID != input.OrganizationID ||
		operation.SeatID != input.SeatID || operation.LeaseID != input.Grant.ID ||
		operation.Fence != input.Grant.Fence || operation.Name != string(input.SkillID) ||
		operation.IdempotencyKey != input.IdempotencyKey {
		return effect.DispatchResult{}, fmt.Errorf("product capability adapter: compiled authority mismatch")
	}
	payload, err := contracts.EncodeCanonical(input)
	if err != nil {
		return effect.DispatchResult{}, err
	}
	digest := sha256Digest(payload)
	artifact := Artifact{
		SchemaVersion: SchemaVersion, ID: input.RecordID, Kind: input.Kind,
		OrganizationID: input.OrganizationID, InitiativeID: input.InitiativeID,
		ProjectID: input.ProjectID, WorkspaceID: input.WorkspaceID,
		AuthorSeatID: input.SeatID, Summary: input.Summary,
		Artifact: contracts.ArtifactRef{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            contracts.ArtifactID("artifact:product-capability:" + digest.Digest[:32]),
			Hash:          digest, MediaType: "application/vnd.matrix.product-capability+json",
			SizeBytes: uint64(len(payload)),
		},
		Evidence:   append([]contracts.EvidenceRef(nil), input.Evidence...),
		DataScopes: append([]string(nil), input.DataScopes...), Source: input.Source,
		ObservedAt: input.ObservedAt, EffectiveAt: input.EffectiveAt,
		FreshUntil: input.FreshUntil,
	}
	if err := artifact.ValidateAt(now); err != nil {
		return effect.DispatchResult{}, err
	}
	observation, err := json.Marshal(struct {
		SchemaVersion string                  `json:"schema_version"`
		Outcome       string                  `json:"outcome"`
		Artifact      Artifact                `json:"artifact"`
		Evidence      []contracts.EvidenceRef `json:"evidence"`
		RequiresHuman bool                    `json:"requires_human"`
	}{
		SchemaVersion: SchemaVersion, Outcome: "proposed", Artifact: artifact,
		Evidence: artifact.Evidence, RequiresHuman: false,
	})
	if err != nil {
		return effect.DispatchResult{}, fmt.Errorf("product capability adapter: encode observation: %w", err)
	}
	return effect.DispatchResult{
		Started: true, ExternalID: productCapabilityProvider + ":" + digest.Digest[:32],
		Observation: observation, ObservedAt: now,
	}, nil
}

// Probe deterministically reproduces the content-addressed proposal and never
// claims that an external deployment, customer, or business effect occurred.
func (adapter *Adapter) Probe(
	ctx context.Context,
	operation effect.Operation,
) (effect.ProbeResult, error) {
	result, err := adapter.Dispatch(ctx, operation)
	if err != nil {
		return effect.ProbeResult{
			Outcome: skills.ProbeUnknown, Dispatch: result,
			Reason: "typed_product_capability_unavailable",
		}, err
	}
	return effect.ProbeResult{
		Outcome: skills.ProbeCompletedOutOfBand, Dispatch: result,
		Reason: "content_addressed_product_capability_result",
	}, nil
}

func artifactKindForSkill(id contracts.SkillID) (ArtifactKind, bool) {
	for _, definition := range capabilityDefinitions() {
		if definition.id == id {
			return definition.kind, true
		}
	}
	return "", false
}

func dataScopesForSkill(id contracts.SkillID) []string {
	for _, definition := range capabilityDefinitions() {
		if definition.id == id {
			return append([]string(nil), definition.dataScopes...)
		}
	}
	return nil
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]bool, len(left))
	for _, value := range left {
		seen[value] = true
	}
	for _, value := range right {
		if !seen[value] {
			return false
		}
	}
	return true
}

var _ effect.Adapter = (*Adapter)(nil)
