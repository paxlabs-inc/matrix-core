// Package developer implements the credential-free local Developer provider.
// It exposes only authoritative read operations in this release; source writes
// remain unavailable until a separately scoped writer is configured.
package developer

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"centra/workforce/internal/effect"
	"centra/workforce/internal/lease"
	"centra/workforce/internal/skills"
)

type Adapter struct {
	repository string
	cg         string
	now        func() time.Time
}

type inputEnvelope struct {
	SchemaVersion string      `json:"schema_version"`
	Grant         lease.Grant `json:"grant"`
	Query         string      `json:"query"`
}

type observation struct {
	SchemaVersion string    `json:"schema_version"`
	Operation     string    `json:"operation"`
	Repository    string    `json:"repository"`
	Output        string    `json:"output"`
	ObservedAt    time.Time `json:"observed_at"`
}

func New(repository, codegraph string, now func() time.Time) (*Adapter, error) {
	root, err := filepath.Abs(strings.TrimSpace(repository))
	if err != nil || root == "" || strings.TrimSpace(codegraph) == "" || now == nil {
		return nil, fmt.Errorf(
			"developer provider: repository, CodeGraph executable, and time source are required",
		)
	}
	return &Adapter{repository: root, cg: codegraph, now: now}, nil
}

func (*Adapter) Name() string { return "developer" }

func (adapter *Adapter) Dispatch(
	ctx context.Context,
	operation effect.Operation,
) (effect.DispatchResult, error) {
	input, err := adapter.authorize(operation)
	if err != nil {
		return effect.DispatchResult{}, err
	}
	switch operation.Name {
	case "plan_change", "inspect_source", "run_verification",
		"inspect_handoff", "inspect_project_brain":
	default:
		return effect.DispatchResult{}, fmt.Errorf(
			"developer provider: operation is not an authorized read",
		)
	}
	args := []string{"stats", "--repo", filepath.Base(adapter.repository)}
	if strings.TrimSpace(input.Query) != "" {
		args = []string{
			"query", strings.TrimSpace(input.Query),
			"--repo", filepath.Base(adapter.repository),
		}
	}
	command := exec.CommandContext(ctx, adapter.cg, args...)
	command.Dir = adapter.repository
	output, err := command.Output()
	if err != nil {
		return effect.DispatchResult{}, fmt.Errorf(
			"developer provider: CodeGraph read failed: %w", err,
		)
	}
	now := adapter.now()
	if now.IsZero() || now.Location() != time.UTC {
		return effect.DispatchResult{}, fmt.Errorf(
			"developer provider: time source must return UTC",
		)
	}
	encoded, err := json.Marshal(observation{
		SchemaVersion: "workforce.v1",
		Operation:     operation.Name,
		Repository:    adapter.repository,
		Output:        string(output),
		ObservedAt:    now,
	})
	if err != nil {
		return effect.DispatchResult{}, err
	}
	return effect.DispatchResult{
		Started: true, ExternalID: operation.IdempotencyKey,
		Observation: encoded, ObservedAt: now,
	}, nil
}

func (adapter *Adapter) Probe(
	ctx context.Context,
	operation effect.Operation,
) (effect.ProbeResult, error) {
	result, err := adapter.Dispatch(ctx, operation)
	if err != nil {
		return effect.ProbeResult{
			Outcome: skills.ProbeUnknown,
			Reason:  "authoritative read unavailable",
		}, err
	}
	return effect.ProbeResult{
		Outcome:  skills.ProbeCompletedOutOfBand,
		Dispatch: result,
		Reason:   "idempotent authoritative read reproduced",
	}, nil
}

func (adapter *Adapter) authorize(
	operation effect.Operation,
) (inputEnvelope, error) {
	var input inputEnvelope
	if err := json.Unmarshal(operation.Input, &input); err != nil ||
		input.SchemaVersion != "workforce.v1" {
		return inputEnvelope{}, fmt.Errorf(
			"developer provider: canonical input is invalid",
		)
	}
	if input.Grant.OrganizationID != operation.OrganizationID ||
		input.Grant.SeatID != operation.SeatID ||
		input.Grant.ID != operation.LeaseID ||
		input.Grant.Fence != operation.Fence ||
		input.Grant.State != lease.StateActive {
		return inputEnvelope{}, fmt.Errorf(
			"developer provider: runtime grant does not match operation",
		)
	}
	return input, nil
}
