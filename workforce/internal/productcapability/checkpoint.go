package productcapability

import (
	"fmt"
	"slices"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/projectbrain"
)

// ExecutionPhase is the closed restart-safe product execution lifecycle.
type ExecutionPhase string

const (
	PhaseIntake            ExecutionPhase = "intake"
	PhasePlanned           ExecutionPhase = "planned"
	PhaseImplementing      ExecutionPhase = "implementing"
	PhaseImplemented       ExecutionPhase = "implemented"
	PhaseVerified          ExecutionPhase = "verified"
	PhaseReleaseReady      ExecutionPhase = "release_ready"
	PhaseDeploymentPending ExecutionPhase = "deployment_pending"
	PhaseDeployed          ExecutionPhase = "deployed"
	PhaseObserved          ExecutionPhase = "observed"
	PhaseIncident          ExecutionPhase = "incident"
	PhaseRolledBack        ExecutionPhase = "rolled_back"
	PhaseClosed            ExecutionPhase = "closed"
)

var executionPhases = []ExecutionPhase{
	PhaseIntake, PhasePlanned, PhaseImplementing, PhaseImplemented,
	PhaseVerified, PhaseReleaseReady, PhaseDeploymentPending, PhaseDeployed,
	PhaseObserved, PhaseIncident, PhaseRolledBack, PhaseClosed,
}

// Valid reports whether the phase is recognized by this checkpoint schema.
func (value ExecutionPhase) Valid() bool {
	return slices.Contains(executionPhases, value)
}

// Checkpoint is a durable crash boundary. Committed effect identities remain
// present across restart so a resumed wake observes or reconciles instead of
// repeating a deployment or rollback.
type Checkpoint struct {
	SchemaVersion       string                     `json:"schema_version"`
	ID                  CheckpointID               `json:"checkpoint_id"`
	Version             uint64                     `json:"version"`
	OrganizationID      contracts.OrganizationID   `json:"organization_id"`
	InitiativeID        InitiativeID               `json:"initiative_id"`
	HandoffID           HandoffID                  `json:"handoff_id"`
	ProjectID           contracts.ProjectID        `json:"project_id"`
	WorkspaceID         contracts.WorkspaceID      `json:"workspace_id"`
	Phase               ExecutionPhase             `json:"phase"`
	Source              projectbrain.GraphSnapshot `json:"source"`
	BrainViewDigest     contracts.ContentHash      `json:"project_brain_view_digest"`
	CompletedRecordIDs  []RecordID                 `json:"completed_record_ids"`
	CommittedEffectIDs  []string                   `json:"committed_effect_ids"`
	ReconciledEffectIDs []string                   `json:"reconciled_effect_ids"`
	IdempotencyKey      string                     `json:"idempotency_key"`
	UpdatedAt           time.Time                  `json:"updated_at"`
}

// Validate enforces bounded durable identities, current source, and exact
// reconciliation membership for every externally committed effect.
func (value Checkpoint) Validate() error {
	if value.SchemaVersion != CheckpointSchemaVersion || value.Version == 0 ||
		!value.Phase.Valid() {
		return fmt.Errorf("product capability: checkpoint schema, version, or phase is invalid")
	}
	for name, tokenValue := range map[string]string{
		"checkpoint_id": string(value.ID), "organization_id": string(value.OrganizationID),
		"initiative_id": string(value.InitiativeID), "handoff_id": string(value.HandoffID),
		"project_id": string(value.ProjectID), "workspace_id": string(value.WorkspaceID),
		"idempotency_key": value.IdempotencyKey,
	} {
		if err := validateToken(name, tokenValue); err != nil {
			return err
		}
	}
	if err := value.Source.Validate(); err != nil {
		return fmt.Errorf("product capability: checkpoint source: %w", err)
	}
	if err := value.BrainViewDigest.Validate(); err != nil {
		return fmt.Errorf("product capability: checkpoint Project Brain digest: %w", err)
	}
	if len(value.CompletedRecordIDs) > 256 || len(value.CommittedEffectIDs) > 128 ||
		len(value.ReconciledEffectIDs) > 128 {
		return fmt.Errorf("product capability: checkpoint lineage exceeds bounds")
	}
	recordIDs := make([]string, len(value.CompletedRecordIDs))
	for index, id := range value.CompletedRecordIDs {
		recordIDs[index] = string(id)
	}
	if err := validateTokenSet("completed record id", recordIDs, 0, 256); err != nil {
		return err
	}
	if err := validateTokenSet("committed effect id", value.CommittedEffectIDs, 0, 128); err != nil {
		return err
	}
	if err := validateTokenSet("reconciled effect id", value.ReconciledEffectIDs, 0, 128); err != nil {
		return err
	}
	committed := make(map[string]bool, len(value.CommittedEffectIDs))
	for _, id := range value.CommittedEffectIDs {
		committed[id] = true
	}
	for _, id := range value.ReconciledEffectIDs {
		if !committed[id] {
			return fmt.Errorf("product capability: reconciled effect was never committed")
		}
	}
	if !validUTC(value.UpdatedAt) || value.UpdatedAt.Before(value.Source.CapturedAt) {
		return fmt.Errorf("product capability: checkpoint updated_at is invalid")
	}
	if value.Phase == PhaseObserved || value.Phase == PhaseClosed || value.Phase == PhaseRolledBack {
		if len(value.CommittedEffectIDs) != len(value.ReconciledEffectIDs) {
			return fmt.Errorf("product capability: terminal checkpoint has ambiguous effects")
		}
	}
	if len(value.CommittedEffectIDs) > 0 &&
		(value.Phase == PhaseIntake || value.Phase == PhasePlanned ||
			value.Phase == PhaseImplementing || value.Phase == PhaseImplemented ||
			value.Phase == PhaseVerified || value.Phase == PhaseReleaseReady) {
		return fmt.Errorf("product capability: pre-deployment checkpoint contains committed effects")
	}
	return nil
}

// ValidateResume compares a durable checkpoint with current CodeGraph and
// Project Brain state before any resumed work or effect may be authorized.
func ValidateResume(
	checkpoint Checkpoint,
	currentSource projectbrain.GraphSnapshot,
	brain projectbrain.View,
	now time.Time,
) error {
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	if err := currentSource.Validate(); err != nil {
		return err
	}
	if err := brain.Validate(); err != nil {
		return err
	}
	if !validUTC(now) || !currentSource.Fresh || !brain.Source.Fresh ||
		!brain.ExpiresAt.After(now) || brain.OrganizationID != checkpoint.OrganizationID ||
		brain.ProjectID != checkpoint.ProjectID || brain.WorkspaceID != checkpoint.WorkspaceID {
		return fmt.Errorf("product capability: resume authority or Project Brain is stale")
	}
	if currentSource.RootDigest != checkpoint.Source.RootDigest ||
		currentSource.GraphDigest != checkpoint.Source.GraphDigest ||
		currentSource.Generation != checkpoint.Source.Generation ||
		brain.Source.RootDigest != currentSource.RootDigest ||
		brain.Source.GraphDigest != currentSource.GraphDigest ||
		brain.Source.Generation != currentSource.Generation ||
		brain.Digest != checkpoint.BrainViewDigest {
		return fmt.Errorf("product capability: resume source or Project Brain drifted")
	}
	return nil
}

func validCheckpointAdvance(from, to ExecutionPhase) bool {
	if from == to {
		return true
	}
	switch from {
	case PhaseIntake:
		return to == PhasePlanned
	case PhasePlanned:
		return to == PhaseImplementing
	case PhaseImplementing:
		return to == PhaseImplemented || to == PhaseIncident
	case PhaseImplemented:
		return to == PhaseVerified || to == PhaseIncident
	case PhaseVerified:
		return to == PhaseReleaseReady || to == PhaseIncident
	case PhaseReleaseReady:
		return to == PhaseDeploymentPending || to == PhaseIncident
	case PhaseDeploymentPending:
		return to == PhaseDeployed || to == PhaseIncident || to == PhaseRolledBack
	case PhaseDeployed:
		return to == PhaseObserved || to == PhaseIncident || to == PhaseRolledBack
	case PhaseIncident:
		return to == PhaseObserved || to == PhaseRolledBack
	case PhaseObserved:
		return to == PhaseClosed || to == PhaseIncident
	case PhaseRolledBack:
		return to == PhaseClosed
	default:
		return false
	}
}
