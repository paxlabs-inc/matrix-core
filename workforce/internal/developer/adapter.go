package developer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/effect"
	"matrix/workforce/internal/processisolation"
	"matrix/workforce/internal/projectbrain"
	"matrix/workforce/internal/skills"
)

const (
	developerAdapterName     = "developer"
	developerOperationOutput = 1 << 20
)

// VerificationCommand is one fixed real verification subprocess available to
// the Developer skill runtime. Agents select its ID but cannot supply a binary,
// arguments, environment, or working directory.
type VerificationCommand struct {
	ID         string
	Bubblewrap string
	Executable string
	Arguments  []string
	Timeout    time.Duration
}

// OperationEnvelope is the typed input accepted by every executable Developer
// skill operation. The generic effect identity must match Grant before any
// operation can read or mutate the workspace.
type OperationEnvelope struct {
	SchemaVersion string                          `json:"schema_version"`
	Grant         Grant                           `json:"grant"`
	Changes       []SourceChange                  `json:"changes,omitempty"`
	Verification  string                          `json:"verification,omitempty"`
	BrainGrant    *projectbrain.CapabilityGrant   `json:"brain_grant,omitempty"`
	ProbeGrant    *projectbrain.CapabilityGrant   `json:"probe_grant,omitempty"`
	Record        *projectbrain.EngineeringRecord `json:"record,omitempty"`
}

// Adapter connects the versioned Developer skill operations to the real
// single-writer effect gateway.
type Adapter struct {
	authority     *Authority
	brain         *projectbrain.Store
	verifications map[string]VerificationCommand
	now           func() time.Time
}

// NewAdapter constructs the executable Developer provider boundary.
func NewAdapter(
	authority *Authority,
	brain *projectbrain.Store,
	verifications []VerificationCommand,
	now func() time.Time,
) (*Adapter, error) {
	if authority == nil || brain == nil || now == nil {
		return nil, fmt.Errorf("developer adapter requires authority, Project Brain, and clock")
	}
	commands := make(map[string]VerificationCommand, len(verifications))
	for _, command := range verifications {
		if err := validateVerificationCommand(command); err != nil {
			return nil, err
		}
		if _, exists := commands[command.ID]; exists {
			return nil, fmt.Errorf("developer verification command %q is duplicated", command.ID)
		}
		command.Arguments = append([]string(nil), command.Arguments...)
		commands[command.ID] = command
	}
	if len(commands) == 0 {
		return nil, fmt.Errorf("developer adapter requires real verification commands")
	}
	return &Adapter{
		authority: authority, brain: brain, verifications: commands, now: now,
	}, nil
}

// Name returns the stable effect-provider identity.
func (adapter *Adapter) Name() string { return developerAdapterName }

// SupportsOperation reports whether the executable adapter owns the declared
// Developer skill operation.
func SupportsOperation(name string) bool {
	switch name {
	case "plan_change", "inspect_source", "apply_scoped_change",
		"restore_source_snapshot", "run_verification", "inspect_handoff",
		"publish_review_handoff", "inspect_project_brain",
		"propose_verified_record":
		return true
	default:
		return false
	}
}

// Dispatch executes one allowlisted Developer operation after exact scope
// authority is revalidated.
func (adapter *Adapter) Dispatch(
	ctx context.Context,
	operation effect.Operation,
) (effect.DispatchResult, error) {
	envelope, err := decodeOperationEnvelope(operation.Input)
	if err != nil {
		return effect.DispatchResult{}, err
	}
	if err := matchEffectGrant(operation, envelope.Grant); err != nil {
		return effect.DispatchResult{}, err
	}
	if !SupportsOperation(operation.Name) {
		return effect.DispatchResult{}, fmt.Errorf("developer operation is not registered")
	}
	now := adapter.now()
	if now.IsZero() || now.Location() != time.UTC {
		return effect.DispatchResult{}, fmt.Errorf("developer adapter clock is invalid")
	}
	switch operation.Name {
	case "plan_change":
		if err := adapter.authority.Authorize(ctx, envelope.Grant); err != nil {
			return effect.DispatchResult{}, err
		}
		return developerResult(operation, now, envelope.Grant.Scope)
	case "inspect_source":
		if err := adapter.authority.Authorize(ctx, envelope.Grant); err != nil {
			return effect.DispatchResult{}, err
		}
		files, err := inspectScopedSource(envelope.Grant.Scope)
		if err != nil {
			return effect.DispatchResult{}, err
		}
		return developerResult(operation, now, files)
	case "apply_scoped_change", "restore_source_snapshot":
		evidence, err := adapter.authority.ApplyScopedChanges(
			ctx, envelope.Grant, operation.Name, envelope.Changes,
		)
		if err != nil {
			return effect.DispatchResult{Started: true, ObservedAt: now}, err
		}
		return developerResult(operation, now, evidence)
	case "run_verification":
		if err := adapter.authority.Authorize(ctx, envelope.Grant); err != nil {
			return effect.DispatchResult{}, err
		}
		result, err := adapter.runVerification(
			ctx, envelope.Grant.Scope.WorkspaceRoot, envelope.Verification,
		)
		if err != nil {
			return effect.DispatchResult{Started: true, ObservedAt: now}, err
		}
		return developerResult(operation, now, result)
	case "inspect_handoff", "inspect_project_brain":
		if envelope.BrainGrant == nil {
			return effect.DispatchResult{}, fmt.Errorf("developer Project Brain read grant is required")
		}
		view, err := adapter.brain.View(ctx, *envelope.BrainGrant)
		if err != nil {
			return effect.DispatchResult{}, err
		}
		if operation.Name == "inspect_handoff" {
			view.Records = recordsOfKind(view.Records, projectbrain.KindHandoff)
		}
		return developerResult(operation, now, view)
	case "publish_review_handoff", "propose_verified_record":
		if envelope.BrainGrant == nil || envelope.Record == nil {
			return effect.DispatchResult{}, fmt.Errorf("developer verified record and write grant are required")
		}
		if operation.Name == "publish_review_handoff" &&
			envelope.Record.Proposal.Kind != projectbrain.KindHandoff {
			return effect.DispatchResult{}, fmt.Errorf("developer review handoff requires a handoff record")
		}
		if err := adapter.authority.Authorize(ctx, envelope.Grant); err != nil {
			return effect.DispatchResult{}, err
		}
		existing, err := adapter.brain.Commit(ctx, *envelope.Record, *envelope.BrainGrant)
		if err != nil {
			return effect.DispatchResult{Started: true, ObservedAt: now}, err
		}
		return developerResult(operation, now, struct {
			RecordID projectbrain.RecordID `json:"record_id"`
			Existing bool                  `json:"existing"`
		}{RecordID: envelope.Record.Proposal.ID, Existing: existing})
	}
	return effect.DispatchResult{}, fmt.Errorf("developer operation is not executable")
}

// Probe performs a read-only authoritative observation for an unresolved
// Developer operation.
func (adapter *Adapter) Probe(
	ctx context.Context,
	operation effect.Operation,
) (effect.ProbeResult, error) {
	envelope, err := decodeOperationEnvelope(operation.Input)
	if err != nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	if err := matchEffectGrant(operation, envelope.Grant); err != nil {
		return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
	}
	switch operation.Name {
	case "apply_scoped_change", "restore_source_snapshot":
		outcome, evidence, err := probeSourceChanges(
			envelope.Grant.Scope.WorkspaceRoot, envelope.Changes,
		)
		return effect.ProbeResult{
			Outcome: outcome,
			Dispatch: effect.DispatchResult{
				Started:     outcome == skills.ProbeCompletedOutOfBand,
				ExternalID:  operationExternalID(operation),
				Observation: evidence, ObservedAt: adapter.now(),
			},
			Reason: "current_source",
		}, err
	case "publish_review_handoff", "propose_verified_record":
		if envelope.ProbeGrant == nil || envelope.Record == nil {
			return effect.ProbeResult{
				Outcome: skills.ProbeUnknown, Reason: "probe_grant_missing",
			}, nil
		}
		view, err := adapter.brain.View(ctx, *envelope.ProbeGrant)
		if err != nil {
			return effect.ProbeResult{
				Outcome: skills.ProbeUnknown, Reason: "project_brain_unavailable",
			}, err
		}
		for _, record := range view.Records {
			if record.Proposal.ID == envelope.Record.Proposal.ID {
				result, resultErr := developerResult(operation, adapter.now(), record)
				return effect.ProbeResult{
					Outcome:  skills.ProbeCompletedOutOfBand,
					Dispatch: result, Reason: "verified_record_present",
				}, resultErr
			}
		}
		return effect.ProbeResult{
			Outcome: skills.ProbeUnchanged, Reason: "verified_record_absent",
		}, nil
	default:
		if err := adapter.authority.Authorize(ctx, envelope.Grant); err != nil {
			return effect.ProbeResult{Outcome: skills.ProbeUnknown}, err
		}
		return effect.ProbeResult{
			Outcome: skills.ProbeCompletedOutOfBand,
			Dispatch: effect.DispatchResult{
				Started: true, ExternalID: operationExternalID(operation),
				Observation: []byte(`{"authorized":true}`), ObservedAt: adapter.now(),
			},
			Reason: "scope_authorized",
		}, nil
	}
}

type verificationResult struct {
	ID       string                `json:"id"`
	Output   string                `json:"output"`
	Evidence contracts.ContentHash `json:"evidence"`
}

func (adapter *Adapter) runVerification(
	ctx context.Context,
	root, id string,
) (verificationResult, error) {
	commandSpec, exists := adapter.verifications[id]
	if !exists {
		return verificationResult{}, fmt.Errorf("developer verification is not registered")
	}
	runContext, cancel := context.WithTimeout(ctx, commandSpec.Timeout)
	defer cancel()
	command, err := processisolation.VerificationCommand(
		runContext,
		processisolation.VerificationSpec{
			Bubblewrap: commandSpec.Bubblewrap, Executable: commandSpec.Executable,
			Arguments: commandSpec.Arguments, Workspace: root,
		},
	)
	if err != nil {
		return verificationResult{}, err
	}
	defer command.Close()
	var output boundedDeveloperOutput
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return verificationResult{}, fmt.Errorf("developer verification failed")
	}
	if output.exceeded {
		return verificationResult{}, fmt.Errorf("developer verification output exceeded limit")
	}
	return verificationResult{
		ID: id, Output: output.String(), Evidence: hashBytes(output.Bytes()),
	}, nil
}

func validateVerificationCommand(command VerificationCommand) error {
	if err := token("verification_id", command.ID); err != nil {
		return err
	}
	if !filepath.IsAbs(command.Bubblewrap) || !filepath.IsAbs(command.Executable) ||
		len(command.Arguments) > 32 ||
		command.Timeout <= 0 || command.Timeout > 30*time.Minute {
		return fmt.Errorf("developer verification command is outside bounds")
	}
	for _, value := range command.Arguments {
		if len(value) > 4096 || strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("developer verification command contains invalid data")
		}
	}
	return nil
}

func decodeOperationEnvelope(input []byte) (OperationEnvelope, error) {
	if len(input) == 0 || len(input) > 256<<10 {
		return OperationEnvelope{}, fmt.Errorf("developer operation input is outside bounds")
	}
	var envelope OperationEnvelope
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return OperationEnvelope{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF ||
		envelope.SchemaVersion != contracts.SchemaVersionV1 {
		return OperationEnvelope{}, fmt.Errorf("developer operation envelope is invalid")
	}
	return envelope, nil
}

func matchEffectGrant(operation effect.Operation, grant Grant) error {
	if operation.OrganizationID != grant.Lease.OrganizationID ||
		operation.SeatID != grant.Lease.SeatID ||
		operation.LeaseID != grant.Lease.ID ||
		operation.Fence != grant.Lease.Fence {
		return fmt.Errorf("developer effect identity does not match scope grant")
	}
	return nil
}

func inspectScopedSource(scope ResolvedScope) ([]ChangedFile, error) {
	result := make([]ChangedFile, 0, len(scope.Files))
	for _, file := range scope.Files {
		current, err := hashScopedFile(scope.WorkspaceRoot, file.Path)
		if err != nil || current != file.Hash {
			return nil, ErrSourceDrift
		}
		result = append(result, ChangedFile{
			Path: file.Path, BeforeHash: file.Hash, AfterHash: current,
		})
	}
	return result, nil
}

func probeSourceChanges(
	root string,
	changes []SourceChange,
) (skills.ProbeOutcome, []byte, error) {
	if len(changes) == 0 {
		return skills.ProbeUnknown, nil, fmt.Errorf("developer probe has no source changes")
	}
	before := 0
	after := 0
	evidence := make([]ChangedFile, 0, len(changes))
	for _, change := range changes {
		if err := change.Validate(); err != nil {
			return skills.ProbeUnknown, nil, err
		}
		current, err := hashScopedFile(root, change.Path)
		if err != nil {
			return skills.ProbeUnknown, nil, err
		}
		expectedAfter := hashBytes(change.Content)
		switch current {
		case change.BeforeHash:
			before++
		case expectedAfter:
			after++
		default:
			return skills.ProbeConflicted, nil, nil
		}
		evidence = append(evidence, ChangedFile{
			Path: change.Path, BeforeHash: change.BeforeHash, AfterHash: current,
		})
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return skills.ProbeUnknown, nil, err
	}
	switch {
	case after == len(changes):
		return skills.ProbeCompletedOutOfBand, encoded, nil
	case before == len(changes):
		return skills.ProbeUnchanged, encoded, nil
	default:
		return skills.ProbeConflicted, encoded, nil
	}
}

func developerResult(
	operation effect.Operation,
	now time.Time,
	value any,
) (effect.DispatchResult, error) {
	observation, err := json.Marshal(value)
	if err != nil {
		return effect.DispatchResult{}, err
	}
	envelope := struct {
		Outcome      string                `json:"outcome"`
		EvidenceHash contracts.ContentHash `json:"evidence_hash"`
		Observation  json.RawMessage       `json:"observation"`
	}{
		Outcome: "completed", EvidenceHash: hashBytes(observation),
		Observation: observation,
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return effect.DispatchResult{}, err
	}
	if len(encoded) > developerOperationOutput {
		return effect.DispatchResult{}, fmt.Errorf("developer operation output exceeded limit")
	}
	return effect.DispatchResult{
		Started: true, ExternalID: operationExternalID(operation),
		Observation: encoded, ObservedAt: now,
	}, nil
}

func operationExternalID(operation effect.Operation) string {
	value := hashBytes([]byte(
		developerAdapterName + "|" + operation.IdempotencyKey,
	))
	return "developer:" + value.Digest[:32]
}

func recordsOfKind(
	records []projectbrain.EngineeringRecord,
	kind projectbrain.Kind,
) []projectbrain.EngineeringRecord {
	result := make([]projectbrain.EngineeringRecord, 0, len(records))
	for _, record := range records {
		if record.Proposal.Kind == kind {
			result = append(result, record)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Proposal.ID < result[right].Proposal.ID
	})
	return result
}

type boundedDeveloperOutput struct {
	value    bytes.Buffer
	exceeded bool
}

func (output *boundedDeveloperOutput) Write(value []byte) (int, error) {
	accepted := len(value)
	remaining := developerOperationOutput - output.value.Len()
	if remaining <= 0 {
		output.exceeded = true
		return accepted, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		output.exceeded = true
	}
	_, _ = output.value.Write(value)
	return accepted, nil
}

func (output *boundedDeveloperOutput) Bytes() []byte  { return output.value.Bytes() }
func (output *boundedDeveloperOutput) String() string { return output.value.String() }

var _ effect.Adapter = (*Adapter)(nil)
