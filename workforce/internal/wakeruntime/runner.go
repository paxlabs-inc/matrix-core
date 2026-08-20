// Package wakeruntime composes the durable Workforce kernel into the real
// claimed-wake execution path owned by workforced.
package wakeruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"centra/workforce/internal/actorstate"
	"centra/workforce/internal/audit"
	"centra/workforce/internal/businessoutcome"
	"centra/workforce/internal/commercialexecution"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/dependency"
	"centra/workforce/internal/developer"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/execution"
	"centra/workforce/internal/lease"
	"centra/workforce/internal/lineage"
	"centra/workforce/internal/modelclient"
	"centra/workforce/internal/policy"
	"centra/workforce/internal/productexecution"
	"centra/workforce/internal/projectbrain"
	"centra/workforce/internal/provider/financial"
	"centra/workforce/internal/skills"
	"centra/workforce/internal/workcompile"
	"centra/workforce/internal/workorder"
	"centra/workforce/scheduler"
)

type Runner struct {
	Scheduler             *scheduler.Store
	Graph                 *dependency.Store
	Orders                *workorder.Store
	Authority             *policy.Store
	Leases                *lease.Store
	Assembler             *actorstate.Assembler
	Seat                  actorstate.Runner
	Model                 *modelclient.Client
	Compiler              *workcompile.Compiler
	Effects               *effect.Gateway
	Execution             *execution.Store
	Lineage               *lineage.Store
	Auditor               audit.Runner
	Audits                *audit.Store
	Catalog               *skills.Catalog
	SkillStore            *skills.Store
	Developer             *developer.Authority
	ProductExecution      *productexecution.Store
	Financial             *financial.Store
	BusinessOutcomes      *businessoutcome.Store
	CommercialExecution   *commercialexecution.Store
	CommercialCoordinator *commercialexecution.Coordinator
	RuntimeKeyID          string
	RuntimeKey            ed25519.PrivateKey
	Runtime               contracts.RuntimeBinding
	TenantID              string
	AuditorSeatID         contracts.SeatID
	LeaseDuration         time.Duration
	WakeBudget            contracts.WakeBudget
	ReplayEvidence        bool
	Now                   func() time.Time
	PublishLifecycle      func(
		context.Context, string, string, bool, contracts.ReceiptID,
		map[string]any,
	) error
}

func (runner *Runner) Validate() error {
	if runner == nil || runner.Scheduler == nil || runner.Graph == nil ||
		runner.Orders == nil || runner.Authority == nil || runner.Leases == nil ||
		runner.Assembler == nil || runner.Model == nil || runner.Compiler == nil ||
		runner.Effects == nil || runner.Execution == nil ||
		runner.Lineage == nil || runner.Audits == nil || runner.Catalog == nil ||
		runner.SkillStore == nil || runner.Financial == nil ||
		runner.BusinessOutcomes == nil || runner.CommercialExecution == nil ||
		runner.CommercialCoordinator == nil ||
		strings.TrimSpace(runner.RuntimeKeyID) == "" ||
		strings.TrimSpace(runner.TenantID) == "" ||
		len(runner.RuntimeKey) != ed25519.PrivateKeySize ||
		runner.AuditorSeatID == "" || runner.Now == nil ||
		runner.LeaseDuration <= 0 || runner.LeaseDuration > 2*time.Hour {
		return fmt.Errorf("wakeruntime: complete durable runtime is required")
	}
	if err := runner.Runtime.Validate(); err != nil {
		return err
	}
	return runner.WakeBudget.Validate()
}

func (runner *Runner) RunDue(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	limit uint32,
) error {
	if err := runner.Validate(); err != nil {
		return err
	}
	claims, err := runner.Scheduler.ClaimDue(
		ctx, string(organizationID), limit,
	)
	if err != nil {
		return err
	}
	var failures []error
	for _, claim := range claims {
		if err := runner.RunClaim(ctx, claim); err != nil {
			if !isRetryableWake(err) {
				_ = runner.parkFailedClaim(
					context.WithoutCancel(ctx), claim, err,
				)
			}
			failures = append(failures, fmt.Errorf(
				"wake %s: %w", claim.Envelope.WakeID, err,
			))
		}
	}
	return errors.Join(failures...)
}

func (runner *Runner) auditorLease(
	ctx context.Context,
	packet contracts.WorkPacket,
	seat contracts.Seat,
	policies []contracts.PolicyRef,
	expiresAt time.Time,
) (contracts.WakeLease, lease.Grant, error) {
	now := runner.Now()
	id := contracts.LeaseID("lease:audit:" + string(packet.Lease.WakeID))
	wakeID := contracts.WakeID("audit:" + string(packet.Lease.WakeID))
	grant, err := runner.Leases.Acquire(ctx, lease.Request{
		ID: id, WakeID: wakeID, OrganizationID: packet.Lease.OrganizationID,
		SeatID: seat.ID, NodeID: dependency.NodeID(packet.Intent.ID),
		MandateID: seat.MandateID, MandateVersion: seat.MandateVersion,
		Policies: policies, IssuedAt: now, ExpiresAt: expiresAt,
	})
	if err != nil {
		return contracts.WakeLease{}, lease.Grant{}, err
	}
	value := contracts.WakeLease{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            id, WakeID: wakeID, OrganizationID: packet.Lease.OrganizationID,
		SeatID: seat.ID, SeatDID: seat.DID, Reason: "independent verification",
		MandateID: seat.MandateID, MandateVersion: seat.MandateVersion,
		Policies:   append([]contracts.PolicyRef(nil), policies...),
		GraphScope: []contracts.IntentID{packet.Intent.ID},
		Model:      packet.Lease.Model, MGS: packet.Lease.MGS,
		Runtime:            packet.Lease.Runtime,
		SkillCatalogDigest: packet.Lease.SkillCatalogDigest,
		Budget:             boundedBudget(packet.Lease.Budget, now, expiresAt),
		IssuedAt:           now, ExpiresAt: expiresAt,
		Fence: grant.Fence,
	}
	if err := policy.SignWakeLease(
		&value, runner.RuntimeKeyID, runner.RuntimeKey,
	); err != nil {
		return contracts.WakeLease{}, lease.Grant{}, err
	}
	if err := runner.Authority.RegisterLease(ctx, value); err != nil {
		return contracts.WakeLease{}, lease.Grant{}, err
	}
	return value, grant, nil
}

func (runner *Runner) sourceState(
	ctx context.Context,
	organizationID contracts.OrganizationID,
) (contracts.SourceState, error) {
	snapshot, err := runner.Graph.Snapshot(ctx, organizationID)
	if err != nil {
		return contracts.SourceState{}, err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return contracts.SourceState{}, err
	}
	sum := sha256.Sum256(encoded)
	var generation uint64 = 1
	for _, node := range snapshot.Nodes {
		generation += node.Version
	}
	return contracts.SourceState{
		RootDigest: contracts.ContentHash{
			Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
		},
		GraphGeneration: generation,
	}, nil
}

func (runner *Runner) finalIntent(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	goalID contracts.GoalID,
	current dependency.NodeID,
) (bool, error) {
	snapshot, err := runner.Graph.Snapshot(ctx, organizationID)
	if err != nil {
		return false, err
	}
	scope := goalScope(snapshot.Nodes, snapshot.Edges, dependency.NodeID(goalID))
	for _, node := range snapshot.Nodes {
		if scope[node.ID] && node.Kind == dependency.NodeIntent && node.ID != current &&
			node.State != dependency.StateCompleted {
			return false, nil
		}
	}
	return true, nil
}

func (runner *Runner) nextEnvelope(
	ctx context.Context,
	order workorder.ExecutionOrder,
	node dependency.Node,
) (scheduler.WakeEnvelope, error) {
	if node.OwnerSeatID == nil {
		return scheduler.WakeEnvelope{}, fmt.Errorf(
			"wakeruntime: eligible node has no owner",
		)
	}
	seat, err := runner.Authority.LoadCurrentSeat(ctx, *node.OwnerSeatID)
	if err != nil {
		return scheduler.WakeEnvelope{}, err
	}
	sum := sha256.Sum256([]byte(node.ID))
	suffix := hex.EncodeToString(sum[:8])
	domain := ""
	if order.Domain != "owner" {
		domain = order.Domain + ":"
	}
	value := scheduler.WakeEnvelope{
		SchemaVersion: "workforce.wake.v1",
		WakeID:        "wake:" + domain + order.ID + ":" + suffix,
		ScheduleID:    "schedule:" + domain + order.ID,
		TenantID:      runner.TenantID, OrganizationID: string(order.OrganizationID),
		SeatID: string(seat.ID), MandateID: string(seat.MandateID),
		MandateVersion: seat.MandateVersion,
		Trigger:        scheduler.TriggerDependency,
		Reason:         "receipt-backed dependency completed",
		ScheduledAt:    runner.Now(),
		Budget: scheduler.Budget{
			MaxTasks:           order.Budget.MaxTasks,
			MaxSpendMicrounits: order.Budget.MaxSpendMicrounits,
		},
		Model: scheduler.ModelBinding{
			Provider: order.ModelProvider, ModelID: order.ModelID,
		},
		MGS: scheduler.MGSBinding{
			Reference: order.MGSReference, Digest: order.MGSDigest,
		},
		IdempotencyKey: "next:" + domain + order.ID + ":" + string(node.ID),
		CoalesceKey:    domain + "work-order:" + order.ID,
		GraphScope:     string(node.ID),
	}
	return value, nil
}

func requiredOutputs(criteria []string) []contracts.RequiredOutput {
	result := make([]contracts.RequiredOutput, len(criteria))
	for index, criterion := range criteria {
		result[index] = contracts.RequiredOutput{
			Kind:             "criterion_" + fmt.Sprintf("%02d", index+1),
			SuccessPredicate: criterion,
		}
	}
	return result
}

func boundedBudget(
	value contracts.WakeBudget,
	issuedAt, expiresAt time.Time,
) contracts.WakeBudget {
	remaining := uint64(expiresAt.Sub(issuedAt) / time.Millisecond)
	if value.MaxDurationMillis > remaining {
		value.MaxDurationMillis = remaining
	}
	return value
}

func proposalSkill(
	values []contracts.SkillRef,
	id contracts.SkillID,
) (contracts.SkillRef, error) {
	for _, value := range values {
		if value.ID == id {
			return value, nil
		}
	}
	return contracts.SkillRef{}, fmt.Errorf(
		"wakeruntime: proposed skill is outside the packet",
	)
}

func bindGrant(input json.RawMessage, grant lease.Grant) ([]byte, error) {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(input, &value); err != nil || value == nil {
		return nil, fmt.Errorf("wakeruntime: proposal input must be an object")
	}
	encodedGrant, err := json.Marshal(grant)
	if err != nil {
		return nil, err
	}
	value["schema_version"] = json.RawMessage(
		`"` + contracts.SchemaVersionV1 + `"`,
	)
	value["grant"] = encodedGrant
	return json.Marshal(value)
}

func (runner *Runner) bindRuntimeGrant(
	ctx context.Context,
	prepared *preparedClaim,
	provider string,
	input json.RawMessage,
	grant lease.Grant,
) ([]byte, error) {
	if provider != "developer" {
		return bindGrant(input, grant)
	}
	if runner.Developer == nil {
		return nil, fmt.Errorf(
			"wakeruntime: Developer project authority is unavailable",
		)
	}
	root, err := filepath.EvalSymlinks(prepared.work.Execute.Scope)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	seat := prepared.packet.Seat
	capabilityIDHash := sha256.Sum256([]byte(
		prepared.wake.WakeID + "|" + string(prepared.intent),
	))
	capability := projectbrain.CapabilityGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		ID: "capability:" + hex.EncodeToString(
			capabilityIDHash[:16],
		),
		TenantID:                runner.TenantID,
		OrganizationID:          prepared.organization,
		ProjectID:               prepared.work.Execute.ProjectID,
		WorkspaceID:             prepared.work.Execute.WorkspaceID,
		WorkspaceRoot:           root,
		Operation:               projectbrain.CapabilityChangeScope,
		RequesterSeatID:         seat.ID,
		RequesterSeatVersion:    seat.Version,
		RequesterSeatDID:        seat.DID,
		RequesterBindingID:      seat.BindingID,
		RequesterBindingVersion: seat.BindingVersion,
		Purpose: "developer_change_scope:" +
			string(prepared.work.Node.ID),
		IssuedAt:  grant.IssuedAt,
		ExpiresAt: grant.ExpiresAt,
	}
	if err := projectbrain.SignCapabilityGrant(
		&capability, runner.RuntimeKeyID, runner.RuntimeKey,
	); err != nil {
		return nil, err
	}
	developerGrant, err := runner.Developer.Bind(
		ctx, grant, developer.ScopeRequest{
			SchemaVersion: contracts.SchemaVersionV1,
			ProjectID:     prepared.work.Execute.ProjectID,
			WorkspaceID:   prepared.work.Execute.WorkspaceID,
			TaskNodeID:    prepared.work.Node.ID,
			WorkspaceRoot: root,
			Files: append(
				[]string(nil), prepared.work.Execute.ScopeFiles...,
			),
			Symbols: append(
				[]string(nil), prepared.work.Execute.ScopeSymbols...,
			),
			Capability: capability,
		},
	)
	if err != nil {
		return nil, err
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(input, &value); err != nil || value == nil {
		return nil, fmt.Errorf(
			"wakeruntime: proposal input must be an object",
		)
	}
	encodedGrant, err := json.Marshal(developerGrant)
	if err != nil {
		return nil, err
	}
	value["schema_version"] = json.RawMessage(
		`"` + contracts.SchemaVersionV1 + `"`,
	)
	value["grant"] = encodedGrant
	return json.Marshal(value)
}

func advanceTo(
	ctx context.Context,
	store *execution.Store,
	state execution.State,
	target execution.Stage,
) (execution.State, error) {
	var err error
	for state.Stage != target {
		state, err = advance(
			ctx, store, state, execution.DecisionAdvance,
			"stage:"+string(state.Stage), execution.Usage{}, "", "", "",
		)
		if err != nil {
			return execution.State{}, err
		}
	}
	return state, nil
}

func advance(
	ctx context.Context,
	store *execution.Store,
	state execution.State,
	decision execution.Decision,
	key string,
	usage execution.Usage,
	effectID string,
	receiptID contracts.ReceiptID,
	final string,
) (execution.State, error) {
	return store.Advance(ctx, execution.AdvanceRequest{
		OrganizationID: state.OrganizationID, WakeID: state.WakeID,
		ExpectedVersion: state.Version, Decision: decision,
		IdempotencyKey: key + ":" + fmt.Sprintf("%d", state.Version),
		Usage:          usage, EffectID: effectID, ReceiptID: receiptID,
		FinalDisposition: contracts.WakeDisposition(final),
	})
}

func eligibleIntent(projection dependency.Projection, goalID contracts.GoalID) *dependency.Node {
	scope := goalScope(projection.Nodes, projection.Edges, dependency.NodeID(goalID))
	values := make([]dependency.Node, 0)
	for _, node := range projection.Eligible {
		if scope[node.ID] && node.Kind == dependency.NodeIntent {
			values = append(values, node)
		}
	}
	if len(values) == 0 {
		return nil
	}
	sort.Slice(values, func(left, right int) bool {
		return values[left].ID < values[right].ID
	})
	return &values[0]
}

func eligibleGoal(projection dependency.Projection, goalID contracts.GoalID) *dependency.Node {
	for index := range projection.Eligible {
		if projection.Eligible[index].Kind == dependency.NodeGoal &&
			projection.Eligible[index].ID == dependency.NodeID(goalID) {
			return &projection.Eligible[index]
		}
	}
	return nil
}

func goalScope(
	nodes []dependency.Node,
	edges []dependency.Edge,
	goalID dependency.NodeID,
) map[dependency.NodeID]bool {
	exists := make(map[dependency.NodeID]bool, len(nodes))
	for _, node := range nodes {
		exists[node.ID] = true
	}
	scope := map[dependency.NodeID]bool{goalID: exists[goalID]}
	for changed := true; changed; {
		changed = false
		for _, edge := range edges {
			if exists[edge.Prerequisite] && scope[edge.Dependent] && !scope[edge.Prerequisite] {
				scope[edge.Prerequisite] = true
				changed = true
			}
		}
	}
	return scope
}

func safeFailure(err error) string {
	if err == nil {
		return "unknown wake failure"
	}
	value := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}

func (runner *Runner) lifecycle(
	ctx context.Context,
	resourceID, kind string,
	verified bool,
	receiptID contracts.ReceiptID,
	fields map[string]any,
) error {
	if runner.PublishLifecycle == nil {
		return nil
	}
	return runner.PublishLifecycle(
		ctx, resourceID, kind, verified, receiptID, fields,
	)
}
