package wakeruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"matrix/workforce/internal/actorstate"
	"matrix/workforce/internal/audit"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/effect"
	"matrix/workforce/internal/execution"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/lineage"
	"matrix/workforce/internal/policy"
	"matrix/workforce/internal/skills"
	"matrix/workforce/internal/workcompile"
	"matrix/workforce/internal/workorder"
	"matrix/workforce/scheduler"
)

const (
	artifactWorkPacket   = "work_packet"
	artifactSeatOutput   = "seat_output"
	artifactAuditorLease = "auditor_lease"
)

type retryableWakeError struct {
	err error
}

func (value retryableWakeError) Error() string {
	return value.err.Error()
}

func (value retryableWakeError) Unwrap() error {
	return value.err
}

func retryWake(err error) error {
	if err == nil {
		return nil
	}
	return retryableWakeError{err: err}
}

func isRetryableWake(err error) bool {
	var target retryableWakeError
	return errors.As(err, &target)
}

type preparedClaim struct {
	wake         scheduler.WakeEnvelope
	work         workorder.Context
	packet       contracts.WorkPacket
	checkpoint   execution.State
	grant        *lease.Grant
	nodeVersion  uint64
	organization contracts.OrganizationID
	intent       contracts.IntentID
}

// RunClaim resumes from immutable wake artifacts and the durable execution
// checkpoint. Every stage after dispatch reads existing truth before deciding
// whether any external call is still permitted.
func (runner *Runner) RunClaim(
	ctx context.Context,
	claim scheduler.Claim,
) error {
	if err := runner.Validate(); err != nil {
		return err
	}
	prepared, err := runner.prepareClaim(ctx, claim)
	if err != nil {
		return err
	}
	if err := runner.lifecycle(
		ctx, prepared.wake.WakeID, "wake.running", false, "",
		map[string]any{
			"state": "working", "intent_id": prepared.intent,
		},
	); err != nil {
		return err
	}
	runErr := runner.resumePreparedClaim(ctx, &prepared)
	if prepared.grant != nil && !isRetryableWake(runErr) {
		cancelErr := runner.Leases.Cancel(
			context.WithoutCancel(ctx), prepared.organization,
			prepared.grant.ID, prepared.grant.Fence,
			"wake attempt closed",
		)
		if cancelErr != nil && !errors.Is(cancelErr, lease.ErrCancelled) &&
			!errors.Is(cancelErr, lease.ErrExpired) {
			runErr = errors.Join(runErr, cancelErr)
		}
	}
	return runErr
}

func (runner *Runner) parkFailedClaim(
	ctx context.Context,
	claim scheduler.Claim,
	cause error,
) error {
	wake := claim.Envelope
	organizationID := contracts.OrganizationID(wake.OrganizationID)
	intentID := contracts.IntentID(wake.GraphScope)
	var parked []error
	state, err := runner.Execution.Load(
		ctx, organizationID, contracts.WakeID(wake.WakeID),
	)
	if err == nil && state.Stage != execution.StageSleep {
		_, err = advance(
			ctx, runner.Execution, state, execution.DecisionFail,
			"permanent-failure", execution.Usage{}, "", "", "",
		)
		if err != nil && !errors.Is(err, execution.ErrTerminal) {
			parked = append(parked, err)
		}
	} else if err != nil && !errors.Is(err, execution.ErrConflict) {
		parked = append(parked, err)
	}
	work, err := runner.Orders.LoadContext(ctx, organizationID, intentID)
	if err == nil && (work.Node.State == dependency.StateEligible ||
		work.Node.State == dependency.StateLeased) {
		err = runner.Graph.Transition(
			ctx, organizationID, work.Node.ID, work.Node.Version,
			dependency.StateWaiting, "",
		)
	}
	if err != nil {
		parked = append(parked, err)
	}
	if err := runner.Scheduler.Fail(
		ctx, wake.OrganizationID, wake.WakeID, safeFailure(cause),
	); err != nil {
		parked = append(parked, err)
	}
	if err := runner.lifecycle(
		ctx, wake.WakeID, "wake.failed", false, "",
		map[string]any{
			"state": "failed", "intent_id": intentID,
		},
	); err != nil {
		parked = append(parked, err)
	}
	return errors.Join(parked...)
}

func (runner *Runner) prepareClaim(
	ctx context.Context,
	claim scheduler.Claim,
) (preparedClaim, error) {
	wake := claim.Envelope
	organizationID := contracts.OrganizationID(wake.OrganizationID)
	intentID := contracts.IntentID(wake.GraphScope)
	if wake.TenantID != runner.TenantID || organizationID == "" ||
		intentID == "" {
		return preparedClaim{}, fmt.Errorf(
			"wakeruntime: claimed envelope is incomplete or outside tenant",
		)
	}
	work, err := runner.Orders.LoadContext(ctx, organizationID, intentID)
	if err != nil {
		return preparedClaim{}, err
	}
	if work.Node.OwnerSeatID == nil ||
		*work.Node.OwnerSeatID != contracts.SeatID(wake.SeatID) {
		return preparedClaim{}, fmt.Errorf(
			"wakeruntime: claimed intent owner does not match scheduler",
		)
	}
	packet, packetFound, err := runner.openPacket(
		ctx, organizationID, contracts.WakeID(wake.WakeID),
	)
	if err != nil {
		return preparedClaim{}, err
	}
	var grant *lease.Grant
	if !packetFound {
		if work.Node.State != dependency.StateEligible {
			return preparedClaim{}, fmt.Errorf(
				"wakeruntime: missing WorkPacket for non-eligible intent",
			)
		}
		packet, grant, err = runner.assembleInitialPacket(
			ctx, wake, work, organizationID, intentID,
		)
		if err != nil {
			return preparedClaim{}, err
		}
		if err := runner.putCanonicalArtifact(
			ctx, organizationID, packet.Lease.WakeID,
			artifactWorkPacket, &packet,
		); err != nil {
			return preparedClaim{}, err
		}
	}
	if err := validateRecoveredPacket(
		runner, wake, work, organizationID, intentID, packet,
	); err != nil {
		return preparedClaim{}, err
	}
	checkpoint, err := runner.Execution.Load(
		ctx, organizationID, packet.Lease.WakeID,
	)
	if errors.Is(err, execution.ErrConflict) {
		if grant == nil {
			recovered, recoverErr := runner.Leases.Recover(
				ctx, organizationID, packet.Lease.ID,
				packet.Lease.WakeID, packet.Seat.ID,
				dependency.NodeID(packet.Intent.ID),
			)
			if recoverErr != nil {
				return preparedClaim{}, recoverErr
			}
			grant = &recovered
		}
		checkpoint, err = runner.Execution.Start(ctx, packet)
	}
	if err != nil {
		return preparedClaim{}, err
	}
	if grant == nil && stageBefore(checkpoint.Stage, execution.StageVerify) {
		recovered, recoverErr := runner.Leases.Recover(
			ctx, organizationID, packet.Lease.ID,
			packet.Lease.WakeID, packet.Seat.ID,
			dependency.NodeID(packet.Intent.ID),
		)
		if recoverErr != nil {
			return preparedClaim{}, recoverErr
		}
		grant = &recovered
	}
	nodeVersion := work.Node.Version
	switch work.Node.State {
	case dependency.StateEligible:
		if err := runner.Graph.Transition(
			ctx, organizationID, work.Node.ID, work.Node.Version,
			dependency.StateLeased, "",
		); err != nil {
			return preparedClaim{}, err
		}
		nodeVersion++
	case dependency.StateLeased:
	case dependency.StateCompleted:
		if stageBefore(checkpoint.Stage, execution.StageVerify) ||
			work.Node.TerminalRecordID == nil {
			return preparedClaim{}, fmt.Errorf(
				"wakeruntime: intent completed before verified recovery boundary",
			)
		}
	case dependency.StateWaiting:
		if checkpoint.Stage != execution.StageVerify &&
			(checkpoint.Stage != execution.StageSleep ||
				checkpoint.Disposition != contracts.DispositionBlocked) {
			return preparedClaim{}, fmt.Errorf(
				"wakeruntime: waiting intent does not match terminal checkpoint",
			)
		}
	default:
		return preparedClaim{}, fmt.Errorf(
			"wakeruntime: intent state %q cannot execute", work.Node.State,
		)
	}
	return preparedClaim{
		wake: wake, work: work, packet: packet, checkpoint: checkpoint,
		grant: grant, nodeVersion: nodeVersion,
		organization: organizationID, intent: intentID,
	}, nil
}

func (runner *Runner) assembleInitialPacket(
	ctx context.Context,
	wake scheduler.WakeEnvelope,
	work workorder.Context,
	organizationID contracts.OrganizationID,
	intentID contracts.IntentID,
) (contracts.WorkPacket, *lease.Grant, error) {
	policies, err := runner.Authority.LoadCurrentPolicyRefs(ctx)
	if err != nil {
		return contracts.WorkPacket{}, nil, err
	}
	seat, err := runner.Authority.LoadCurrentSeat(
		ctx, contracts.SeatID(wake.SeatID),
	)
	if err != nil {
		return contracts.WorkPacket{}, nil, err
	}
	if seat.MandateID != contracts.MandateID(wake.MandateID) ||
		seat.MandateVersion != wake.MandateVersion {
		return contracts.WorkPacket{}, nil, fmt.Errorf(
			"wakeruntime: scheduler authority is stale",
		)
	}
	now := runner.Now()
	if now.IsZero() || now.Location() != time.UTC {
		return contracts.WorkPacket{}, nil, fmt.Errorf(
			"wakeruntime: time source must return UTC",
		)
	}
	expiresAt := now.Add(runner.LeaseDuration)
	if expiresAt.After(work.Order.Deadline) {
		expiresAt = work.Order.Deadline
	}
	if !expiresAt.After(now) {
		return contracts.WorkPacket{}, nil, fmt.Errorf(
			"wakeruntime: work order deadline has elapsed",
		)
	}
	leaseID := contracts.LeaseID("lease:" + wake.WakeID)
	grant, err := runner.Leases.Recover(
		ctx, organizationID, leaseID, contracts.WakeID(wake.WakeID),
		seat.ID, dependency.NodeID(intentID),
	)
	if errors.Is(err, lease.ErrStaleFence) {
		grant, err = runner.Leases.Acquire(ctx, lease.Request{
			ID: leaseID, WakeID: contracts.WakeID(wake.WakeID),
			OrganizationID: organizationID, SeatID: seat.ID,
			NodeID: dependency.NodeID(intentID), MandateID: seat.MandateID,
			MandateVersion: seat.MandateVersion, Policies: policies,
			IssuedAt: now, ExpiresAt: expiresAt,
		})
	}
	if err != nil {
		return contracts.WorkPacket{}, nil, err
	}
	authorityLease := contracts.WakeLease{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            leaseID, WakeID: contracts.WakeID(wake.WakeID),
		OrganizationID: organizationID, SeatID: seat.ID, SeatDID: seat.DID,
		Reason: wake.Reason, MandateID: seat.MandateID,
		MandateVersion: seat.MandateVersion,
		Policies:       append([]contracts.PolicyRef(nil), grant.Policies...),
		GraphScope:     []contracts.IntentID{intentID},
		Model:          runner.Model.Binding("model:" + wake.WakeID),
		MGS: contracts.MGSGenomeRef{
			Reference: wake.MGS.Reference,
			Digest: contracts.ContentHash{
				Algorithm: "sha256", Digest: wake.MGS.Digest,
			},
		},
		Runtime: runner.Runtime, SkillCatalogDigest: runner.Catalog.Digest(),
		Budget: boundedBudget(
			runner.WakeBudget, grant.IssuedAt, grant.ExpiresAt,
		),
		IssuedAt: grant.IssuedAt, ExpiresAt: grant.ExpiresAt,
		Fence: grant.Fence,
	}
	if authorityLease.Model.Provider != wake.Model.Provider ||
		authorityLease.Model.ModelID != wake.Model.ModelID {
		return contracts.WorkPacket{}, nil, fmt.Errorf(
			"wakeruntime: scheduled model is not configured",
		)
	}
	if err := policy.SignWakeLease(
		&authorityLease, runner.RuntimeKeyID, runner.RuntimeKey,
	); err != nil {
		return contracts.WorkPacket{}, nil, err
	}
	if err := runner.Authority.RegisterLease(ctx, authorityLease); err != nil {
		return contracts.WorkPacket{}, nil, err
	}
	predecessorArtifacts, predecessorEvidence, err := runner.predecessorReceipts(
		ctx, organizationID, work.Node.ID,
	)
	if err != nil {
		return contracts.WorkPacket{}, nil, err
	}
	packet, err := runner.Assembler.Assemble(
		ctx, actorstate.AssemblyRequest{
			LeaseID: authorityLease.ID, Goal: work.Goal,
			Intent:         work.Intent,
			Artifacts:      predecessorArtifacts,
			Evidence:       predecessorEvidence,
			RequiredOutput: requiredOutputs(work.Order.AcceptanceCriteria),
			InboxLimit:     100,
		},
	)
	if err != nil {
		return contracts.WorkPacket{}, nil, err
	}
	return packet, &grant, nil
}

func (runner *Runner) predecessorReceipts(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	intentID dependency.NodeID,
) ([]contracts.ArtifactRef, []contracts.EvidenceRef, error) {
	snapshot, err := runner.Graph.Snapshot(ctx, organizationID)
	if err != nil {
		return nil, nil, err
	}
	nodes := make(map[dependency.NodeID]dependency.Node, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		nodes[node.ID] = node
	}
	var artifacts []contracts.ArtifactRef
	var evidence []contracts.EvidenceRef
	for _, edge := range snapshot.Edges {
		if edge.Dependent != intentID {
			continue
		}
		predecessor, ok := nodes[edge.Prerequisite]
		if !ok || predecessor.Kind != dependency.NodeIntent ||
			predecessor.State != dependency.StateCompleted ||
			predecessor.TerminalRecordID == nil {
			return nil, nil, fmt.Errorf(
				"wakeruntime: predecessor receipt is not verified current truth",
			)
		}
		receipt, openErr := runner.Lineage.OpenReceipt(
			ctx, organizationID,
			contracts.ReceiptID(*predecessor.TerminalRecordID),
		)
		if openErr != nil {
			return nil, nil, fmt.Errorf("wakeruntime: open predecessor receipt: %w", openErr)
		}
		if receipt.ParentIntentID != contracts.IntentID(predecessor.ID) {
			return nil, nil, fmt.Errorf("wakeruntime: predecessor receipt intent mismatch")
		}
		encoded, encodeErr := contracts.EncodeCanonical(&receipt)
		if encodeErr != nil {
			return nil, nil, encodeErr
		}
		suffix := receipt.ContentHash.Digest[:32]
		artifacts = append(artifacts, contracts.ArtifactRef{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            contracts.ArtifactID("artifact:receipt:" + suffix),
			Hash:          receipt.ContentHash,
			MediaType:     "application/vnd.matrix.workforce-receipt+json",
			SizeBytes:     uint64(len(encoded)),
		})
		evidence = append(evidence, contracts.EvidenceRef{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            contracts.EvidenceID("evidence:receipt:" + suffix),
			Hash:          receipt.ContentHash,
			Kind:          "verified_receipt",
			ObservedAt:    receipt.CreatedAt,
		})
	}
	return artifacts, evidence, nil
}

func (runner *Runner) resumePreparedClaim(
	ctx context.Context,
	prepared *preparedClaim,
) error {
	for {
		state := prepared.checkpoint
		switch state.Stage {
		case execution.StageLease, execution.StageOrient,
			execution.StageSelect:
			next, err := advanceTo(
				ctx, runner.Execution, state, execution.StagePropose,
			)
			if err != nil {
				return err
			}
			prepared.checkpoint = next
		case execution.StageReconcile:
			if state.ResumeStage == "" {
				next, err := advanceTo(
					ctx, runner.Execution, state, execution.StagePropose,
				)
				if err != nil {
					return err
				}
				prepared.checkpoint = next
				continue
			}
			if err := runner.reconcileEffect(ctx, prepared); err != nil {
				return err
			}
		case execution.StagePropose:
			if err := runner.propose(ctx, prepared); err != nil {
				return err
			}
		case execution.StageCompile:
			if err := runner.compile(ctx, prepared); err != nil {
				return err
			}
		case execution.StagePreflight:
			next, err := advance(
				ctx, runner.Execution, state, execution.DecisionAdvance,
				"preflight", execution.Usage{}, "", "", "",
			)
			if err != nil {
				return err
			}
			prepared.checkpoint = next
		case execution.StageExecute:
			if err := runner.executeEffect(ctx, prepared); err != nil {
				return err
			}
		case execution.StageObserve:
			if err := runner.observeEffect(ctx, prepared); err != nil {
				return err
			}
		case execution.StageVerify:
			if err := runner.verifyAndCommit(ctx, prepared); err != nil {
				return err
			}
		case execution.StageCommit:
			receiptID := contracts.ReceiptID(
				"receipt:" + prepared.wake.WakeID,
			)
			if _, err := runner.Lineage.OpenReceipt(
				ctx, prepared.organization, receiptID,
			); err != nil {
				return err
			}
			next, err := advance(
				ctx, runner.Execution, state, execution.DecisionAdvance,
				"receipt-commit", execution.Usage{}, "", receiptID, "",
			)
			if err != nil {
				return err
			}
			prepared.checkpoint = next
		case execution.StageYield:
			if err := runner.yield(ctx, prepared); err != nil {
				return err
			}
		case execution.StageSleep:
			return runner.finishScheduledWake(ctx, prepared)
		default:
			return fmt.Errorf(
				"wakeruntime: unsupported checkpoint stage %q", state.Stage,
			)
		}
	}
}

func (runner *Runner) propose(
	ctx context.Context,
	prepared *preparedClaim,
) error {
	_, modelOutput, err := runner.Lineage.OpenModelOutput(
		ctx, prepared.organization,
		contracts.EvidenceID("model:"+prepared.wake.WakeID),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		signedSkills := make(
			[]skills.SignedContract, len(prepared.packet.Skills),
		)
		for index, reference := range prepared.packet.Skills {
			signedSkills[index], err = runner.SkillStore.LoadAccepted(
				ctx, reference,
			)
			if err != nil {
				return err
			}
		}
		exchange, completeErr := runner.Model.Complete(
			ctx, prepared.packet, signedSkills,
		)
		if completeErr != nil {
			return retryWake(completeErr)
		}
		if _, err := runner.Lineage.PutModelEvidence(
			ctx, lineage.ModelExchange{
				ID: contracts.EvidenceID(
					"model:" + prepared.wake.WakeID,
				),
				OrganizationID: prepared.organization,
				WakeID:         prepared.packet.Lease.WakeID,
				Model:          prepared.packet.Lease.Model,
				MGS:            prepared.packet.Lease.MGS,
				Runtime:        prepared.packet.Lease.Runtime,
				Request:        exchange.Request,
				Response:       exchange.Response,
				Output:         exchange.Output,
				ReplayRequired: runner.ReplayEvidence,
			},
		); err != nil {
			return err
		}
		modelOutput = exchange.Output
	} else if err != nil {
		return err
	}
	output, found, err := runner.openSeatOutput(
		ctx, prepared.organization, prepared.packet.Lease.WakeID,
	)
	if err != nil {
		return err
	}
	if !found {
		output, err = runner.Seat.RunModel(
			ctx, prepared.packet, modelOutput,
		)
		if err != nil {
			return err
		}
		if err := runner.putCanonicalArtifact(
			ctx, prepared.organization, prepared.packet.Lease.WakeID,
			artifactSeatOutput, &output,
		); err != nil {
			return err
		}
	}
	if output.Disposition == contracts.DispositionBlocked {
		if err := runner.Graph.Transition(
			ctx, prepared.organization, prepared.work.Node.ID,
			prepared.nodeVersion, dependency.StateWaiting, "",
		); err != nil {
			return err
		}
		next, err := advance(
			ctx, runner.Execution, prepared.checkpoint,
			execution.DecisionBlock, "seat-blocked", execution.Usage{},
			"", "", "",
		)
		if err != nil {
			return err
		}
		prepared.checkpoint = next
		return nil
	}
	if output.Disposition != contracts.DispositionProgressed ||
		output.Proposal == nil {
		return fmt.Errorf(
			"wakeruntime: progressed seat output requires a proposal",
		)
	}
	next, err := advance(
		ctx, runner.Execution, prepared.checkpoint,
		execution.DecisionAdvance, "model-response",
		execution.Usage{ModelCalls: 1}, "", "", "",
	)
	if err != nil {
		return err
	}
	prepared.checkpoint = next
	return nil
}

func (runner *Runner) compile(
	ctx context.Context,
	prepared *preparedClaim,
) error {
	plan, err := runner.Compiler.Load(
		ctx, prepared.organization, "proposal:"+prepared.wake.WakeID,
	)
	if errors.Is(err, workcompile.ErrPlanNotFound) {
		if prepared.grant == nil {
			return fmt.Errorf(
				"wakeruntime: live execution grant is required to compile",
			)
		}
		output, found, outputErr := runner.openSeatOutput(
			ctx, prepared.organization, prepared.packet.Lease.WakeID,
		)
		if outputErr != nil {
			return outputErr
		}
		if !found || output.Proposal == nil {
			return fmt.Errorf(
				"wakeruntime: durable seat proposal is unavailable",
			)
		}
		skill, skillErr := proposalSkill(
			prepared.packet.Skills, output.Proposal.SkillID,
		)
		if skillErr != nil {
			return skillErr
		}
		if inputErr := validateDepartmentInput(
			prepared.packet, skill.ID, output.Proposal.Provider,
			output.Proposal.Input,
		); inputErr != nil {
			return inputErr
		}
		input, inputErr := runner.bindRuntimeGrant(
			ctx, prepared, output.Proposal.Provider,
			output.Proposal.Input, *prepared.grant,
		)
		if inputErr != nil {
			return inputErr
		}
		source, sourceErr := runner.sourceState(
			ctx, prepared.organization,
		)
		if sourceErr != nil {
			return sourceErr
		}
		plan, err = runner.Compiler.Compile(
			ctx, prepared.packet, workcompile.Proposal{
				SchemaVersion:  contracts.SchemaVersionV1,
				ID:             "proposal:" + prepared.wake.WakeID,
				OrganizationID: prepared.organization,
				WakeID:         prepared.packet.Lease.WakeID,
				IntentID:       prepared.intent,
				SeatID:         prepared.packet.Seat.ID,
				Skill:          skill,
				Operation:      output.Proposal.Operation,
				Provider:       output.Proposal.Provider,
				IdempotencyKey: "effect:" + prepared.wake.WakeID,
				Input:          input,
				Deadline:       prepared.packet.Lease.ExpiresAt,
			}, source,
		)
	}
	if err != nil {
		return err
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	next, err := advance(
		ctx, runner.Execution, prepared.checkpoint,
		execution.DecisionAdvance, "compiled", execution.Usage{},
		"", "", "",
	)
	if err != nil {
		return err
	}
	prepared.checkpoint = next
	return nil
}

func validateDepartmentInput(
	packet contracts.WorkPacket,
	skillID contracts.SkillID,
	provider string,
	input json.RawMessage,
) error {
	if provider == "developer" {
		return nil
	}
	var value struct {
		OrganizationID contracts.OrganizationID `json:"organization_id"`
		Department     contracts.DepartmentKind `json:"department"`
		SeatID         contracts.SeatID         `json:"seat_id"`
		IntentID       contracts.IntentID       `json:"intent_id"`
		SkillID        contracts.SkillID        `json:"skill_id"`
		Evidence       json.RawMessage          `json:"evidence"`
		SourceDigest   contracts.ContentHash    `json:"source_digest"`
	}
	if err := json.Unmarshal(input, &value); err != nil ||
		value.OrganizationID != packet.Lease.OrganizationID ||
		value.Department != packet.Mandate.DepartmentKind ||
		value.SeatID != packet.Seat.ID || value.IntentID != packet.Intent.ID ||
		value.SkillID != skillID || value.SourceDigest.Validate() != nil {
		return fmt.Errorf("wakeruntime: department proposal identity is outside WorkPacket")
	}
	allowed := make(map[contracts.EvidenceID]contracts.EvidenceRef, len(packet.Evidence))
	sourceAllowed := false
	for _, reference := range packet.Evidence {
		allowed[reference.ID] = reference
		if reference.Hash == value.SourceDigest {
			sourceAllowed = true
		}
	}
	if !sourceAllowed {
		return fmt.Errorf("wakeruntime: department source digest is outside WorkPacket")
	}
	var references []contracts.EvidenceRef
	if provider == "knowledge" {
		if err := json.Unmarshal(value.Evidence, &references); err != nil {
			return fmt.Errorf("wakeruntime: knowledge evidence is invalid")
		}
	} else {
		var expiring []struct {
			Reference contracts.EvidenceRef `json:"reference"`
		}
		if err := json.Unmarshal(value.Evidence, &expiring); err != nil {
			return fmt.Errorf("wakeruntime: department evidence is invalid")
		}
		for _, item := range expiring {
			references = append(references, item.Reference)
		}
	}
	if len(references) == 0 {
		return fmt.Errorf("wakeruntime: department evidence is required")
	}
	for _, reference := range references {
		if current, ok := allowed[reference.ID]; !ok || current != reference {
			return fmt.Errorf("wakeruntime: department evidence is outside WorkPacket")
		}
	}
	return nil
}

func (runner *Runner) executeEffect(
	ctx context.Context,
	prepared *preparedClaim,
) error {
	plan, err := runner.loadPlan(ctx, prepared)
	if err != nil {
		return err
	}
	if prepared.checkpoint.PendingEffectID == "" {
		next, dispatchErr := advance(
			ctx, runner.Execution, prepared.checkpoint,
			execution.DecisionDispatch, "dispatch", execution.Usage{},
			plan.Operation.ID, "", "",
		)
		if dispatchErr != nil {
			return dispatchErr
		}
		prepared.checkpoint = next
	}
	result, err := runner.Effects.Execute(ctx, plan.Operation)
	if errors.Is(err, effect.ErrAmbiguous) {
		next, resumeErr := runner.Execution.Resume(
			ctx, prepared.organization, prepared.packet.Lease.WakeID,
			"effect-ambiguous:"+fmt.Sprintf(
				"%d", prepared.checkpoint.Version,
			),
		)
		if resumeErr != nil {
			return resumeErr
		}
		prepared.checkpoint = next
		return retryWake(err)
	}
	if err != nil {
		return err
	}
	if result.State != effect.StateSucceeded {
		return fmt.Errorf(
			"wakeruntime: effect did not produce authoritative success",
		)
	}
	next, err := advance(
		ctx, runner.Execution, prepared.checkpoint,
		execution.DecisionObserved, "observed",
		execution.Usage{ToolCalls: 1}, plan.Operation.ID, "", "",
	)
	if err != nil {
		return err
	}
	prepared.checkpoint = next
	return nil
}

func (runner *Runner) reconcileEffect(
	ctx context.Context,
	prepared *preparedClaim,
) error {
	plan, err := runner.loadPlan(ctx, prepared)
	if err != nil {
		return err
	}
	result, err := runner.Effects.Reconcile(ctx, plan.Operation)
	if err != nil {
		return retryWake(err)
	}
	if result.State != effect.StateSucceeded {
		return retryWake(fmt.Errorf(
			"wakeruntime: reconciliation has no authoritative success",
		))
	}
	next, err := advance(
		ctx, runner.Execution, prepared.checkpoint,
		execution.DecisionReconcileCompleted, "reconciled",
		execution.Usage{ToolCalls: 1}, plan.Operation.ID, "", "",
	)
	if err != nil {
		return err
	}
	prepared.checkpoint = next
	return nil
}

func (runner *Runner) observeEffect(
	ctx context.Context,
	prepared *preparedClaim,
) error {
	plan, err := runner.loadPlan(ctx, prepared)
	if err != nil {
		return err
	}
	result, err := runner.Effects.LoadResult(ctx, plan.Operation)
	if err != nil {
		return err
	}
	if result.State != effect.StateSucceeded ||
		result.EvidenceHash.Validate() != nil ||
		result.ObservedAt.IsZero() {
		return fmt.Errorf(
			"wakeruntime: durable provider observation is incomplete",
		)
	}
	next, err := advance(
		ctx, runner.Execution, prepared.checkpoint,
		execution.DecisionAdvance, "verify", execution.Usage{},
		"", "", "",
	)
	if err != nil {
		return err
	}
	prepared.checkpoint = next
	return nil
}

func (runner *Runner) verifyAndCommit(
	ctx context.Context,
	prepared *preparedClaim,
) error {
	plan, err := runner.loadPlan(ctx, prepared)
	if err != nil {
		return err
	}
	result, err := runner.Effects.LoadResult(ctx, plan.Operation)
	if err != nil {
		return err
	}
	observation := contracts.EvidenceRef{
		SchemaVersion: contracts.SchemaVersionV1,
		ID: contracts.EvidenceID(
			"effect:" + prepared.wake.WakeID,
		),
		Hash:       result.EvidenceHash,
		Kind:       "provider_observation",
		ObservedAt: result.ObservedAt,
	}
	if err := observation.Validate(); err != nil {
		return err
	}
	if prepared.grant != nil {
		cancelErr := runner.Leases.Cancel(
			ctx, prepared.organization, prepared.grant.ID,
			prepared.grant.Fence,
			"execution complete; independent audit begins",
		)
		if cancelErr != nil && !errors.Is(cancelErr, lease.ErrCancelled) &&
			!errors.Is(cancelErr, lease.ErrExpired) {
			return cancelErr
		}
		prepared.grant = nil
	}
	verdictID := contracts.VerdictID(
		"verdict:" + prepared.wake.WakeID,
	)
	verdict, err := runner.Audits.LoadVerdict(
		ctx, prepared.organization, verdictID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		verdict, err = runner.runIndependentAudit(
			ctx, prepared, plan, observation, verdictID,
		)
	}
	if err != nil {
		return err
	}
	if verdict.Outcome == contracts.VerdictRequiresHuman {
		return runner.waitForHumanVerification(ctx, prepared, verdict.ID)
	}
	if verdict.Outcome != contracts.VerdictPass {
		return fmt.Errorf(
			"wakeruntime: independent audit did not pass",
		)
	}
	modelEvidence, _, _, err := runner.Lineage.OpenModelEvidence(
		ctx, prepared.organization,
		contracts.EvidenceID("model:"+prepared.wake.WakeID),
	)
	if err != nil {
		return err
	}
	receiptID := contracts.ReceiptID(
		"receipt:" + prepared.wake.WakeID,
	)
	receipt, err := runner.Lineage.OpenReceipt(
		ctx, prepared.organization, receiptID,
	)
	if errors.Is(err, lineage.ErrConflict) {
		finalIntent, finalErr := runner.finalIntent(
			ctx, prepared.organization, prepared.work.Node.ID,
		)
		if finalErr != nil {
			return finalErr
		}
		disposition := contracts.DispositionProgressed
		if finalIntent {
			disposition = contracts.DispositionGoalCompleted
		}
		receipt, err = runner.Lineage.BuildReceipt(
			lineage.ReceiptInput{
				ID: receiptID, Packet: prepared.packet, Plan: plan,
				ModelEvidence: modelEvidence,
				Constraints: []string{
					"owner-signed work order, current mandate, policy, live fence, fresh seat, and independent audit",
				},
				Evidence:         []contracts.EvidenceRef{observation},
				VerdictID:        &verdict.ID,
				LatencyMillis:    uint64(runner.Now().Sub(prepared.checkpoint.StartedAt) / time.Millisecond),
				Disposition:      disposition,
				OperationOutcome: "succeeded",
			},
		)
		if err == nil {
			err = runner.Lineage.PublishReceipt(ctx, receipt)
		}
	}
	if err != nil {
		return err
	}
	snapshot, err := runner.Graph.Snapshot(ctx, prepared.organization)
	if err != nil {
		return err
	}
	node, found := snapshotNode(snapshot, prepared.work.Node.ID)
	if !found {
		return dependency.ErrNotFound
	}
	if node.State != dependency.StateCompleted {
		if err := runner.Graph.FinishWithReceipt(
			ctx, prepared.organization, node.ID, node.Version,
			dependency.StateCompleted, receipt.ID,
		); err != nil {
			return err
		}
	} else if node.TerminalRecordID == nil ||
		*node.TerminalRecordID != contracts.RecordID(receipt.ID) {
		return fmt.Errorf(
			"wakeruntime: completed node carries a different receipt",
		)
	}
	next, err := advance(
		ctx, runner.Execution, prepared.checkpoint,
		execution.DecisionAdvance, "verified", execution.Usage{},
		"", "", "",
	)
	if err != nil {
		return err
	}
	prepared.checkpoint = next
	return nil
}

func (runner *Runner) runIndependentAudit(
	ctx context.Context,
	prepared *preparedClaim,
	plan workcompile.Plan,
	observation contracts.EvidenceRef,
	verdictID contracts.VerdictID,
) (contracts.Verdict, error) {
	auditorSeat, err := runner.selectIndependentAuditor(
		ctx, prepared.packet.Seat.DepartmentID,
	)
	if err != nil {
		return contracts.Verdict{}, err
	}
	auditorLease, auditorGrant, found, err := runner.openAuditorLease(
		ctx, prepared.organization, prepared.packet.Lease.WakeID,
	)
	if err != nil {
		return contracts.Verdict{}, err
	}
	if found {
		recovered, recoverErr := runner.Leases.Recover(
			ctx, prepared.organization, auditorLease.ID,
			auditorLease.WakeID, auditorLease.SeatID,
			dependency.NodeID(prepared.packet.Intent.ID),
		)
		if recoverErr != nil {
			return contracts.Verdict{}, recoverErr
		}
		auditorGrant = recovered
	} else {
		expiresAt := runner.Now().Add(runner.LeaseDuration)
		if expiresAt.After(prepared.work.Order.Deadline) {
			expiresAt = prepared.work.Order.Deadline
		}
		auditorLease, auditorGrant, err = runner.auditorLease(
			ctx, prepared.packet, auditorSeat,
			prepared.packet.Policies, expiresAt,
		)
		if err != nil {
			return contracts.Verdict{}, err
		}
		if err := runner.putCanonicalArtifact(
			ctx, prepared.organization, prepared.packet.Lease.WakeID,
			artifactAuditorLease, &auditorLease,
		); err != nil {
			return contracts.Verdict{}, err
		}
	}
	defer func() {
		_ = runner.Leases.Cancel(
			context.WithoutCancel(ctx), prepared.organization,
			auditorLease.ID, auditorGrant.Fence, "audit wake closed",
		)
	}()
	packet, err := auditPacket(
		prepared.packet, plan, observation, auditorSeat,
	)
	if err != nil {
		return contracts.Verdict{}, err
	}
	verified, err := runner.Auditor.RunVerified(ctx, packet)
	if err != nil {
		return contracts.Verdict{}, err
	}
	return runner.Audits.Commit(ctx, audit.CommitRequest{
		ID: verdictID, Packet: packet, AuditorLease: auditorLease,
		Decision: verified,
	})
}

func (runner *Runner) selectIndependentAuditor(
	ctx context.Context,
	executingDepartment contracts.DepartmentID,
) (contracts.Seat, error) {
	candidates := []contracts.SeatID{runner.AuditorSeatID}
	for _, department := range contracts.AllDepartmentKinds() {
		candidates = append(candidates, contracts.SeatID(
			"seat-"+string(department)+"-"+string(contracts.SeatAuditor),
		))
	}
	seen := make(map[contracts.SeatID]bool, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		seat, err := runner.Authority.LoadCurrentSeat(ctx, candidate)
		if err == nil && seat.Role == contracts.SeatAuditor &&
			seat.DepartmentID != executingDepartment {
			return seat, nil
		}
	}
	return contracts.Seat{}, fmt.Errorf("wakeruntime: no current independent Auditor is available")
}

func (runner *Runner) waitForHumanVerification(
	ctx context.Context,
	prepared *preparedClaim,
	verdictID contracts.VerdictID,
) error {
	snapshot, err := runner.Graph.Snapshot(ctx, prepared.organization)
	if err != nil {
		return err
	}
	node, found := snapshotNode(snapshot, prepared.work.Node.ID)
	if !found {
		return dependency.ErrNotFound
	}
	switch node.State {
	case dependency.StateLeased:
		if err := runner.Graph.Transition(
			ctx, prepared.organization, node.ID, node.Version,
			dependency.StateWaiting, "",
		); err != nil {
			return err
		}
	case dependency.StateWaiting:
	default:
		return fmt.Errorf(
			"wakeruntime: human verification cannot park intent in state %q",
			node.State,
		)
	}
	switch {
	case prepared.checkpoint.Stage == execution.StageVerify:
		next, err := advance(
			ctx, runner.Execution, prepared.checkpoint,
			execution.DecisionBlock, "human-verification-required",
			execution.Usage{}, "", "", "",
		)
		if err != nil {
			return err
		}
		prepared.checkpoint = next
	case prepared.checkpoint.Stage == execution.StageSleep &&
		prepared.checkpoint.Disposition == contracts.DispositionBlocked:
	default:
		return fmt.Errorf(
			"wakeruntime: human verification conflicts with checkpoint",
		)
	}
	return runner.lifecycle(
		ctx, prepared.wake.WakeID, "wake.verification_required", false, "",
		map[string]any{
			"state": "waiting", "intent_id": prepared.intent,
			"verdict_id": verdictID,
		},
	)
}

func (runner *Runner) yield(
	ctx context.Context,
	prepared *preparedClaim,
) error {
	projection, err := runner.Graph.Resolve(ctx, prepared.organization)
	if err != nil {
		return err
	}
	nextIntent := eligibleIntent(projection)
	disposition := contracts.DispositionProgressed
	if nextIntent == nil {
		goal := eligibleGoal(projection)
		if goal != nil {
			if _, err := runner.Graph.FinishGoalFromReceipts(
				ctx, prepared.organization, goal.ID, goal.Version+1,
			); err != nil {
				return err
			}
		} else {
			snapshot, snapshotErr := runner.Graph.Snapshot(
				ctx, prepared.organization,
			)
			if snapshotErr != nil {
				return snapshotErr
			}
			if !completedGoalExists(snapshot) {
				return fmt.Errorf(
					"wakeruntime: no next intent and root goal is not complete",
				)
			}
		}
		disposition = contracts.DispositionGoalCompleted
	}
	next, err := advance(
		ctx, runner.Execution, prepared.checkpoint,
		execution.DecisionAdvance, "yield", execution.Usage{},
		"", "", string(disposition),
	)
	if err != nil {
		return err
	}
	prepared.checkpoint = next
	return nil
}

func (runner *Runner) finishScheduledWake(
	ctx context.Context,
	prepared *preparedClaim,
) error {
	var nextWakeQueued bool
	switch prepared.checkpoint.Disposition {
	case contracts.DispositionProgressed:
		projection, err := runner.Graph.Resolve(
			ctx, prepared.organization,
		)
		if err != nil {
			return err
		}
		next := eligibleIntent(projection)
		if next == nil {
			return fmt.Errorf(
				"wakeruntime: progressed wake has no eligible successor",
			)
		}
		nextWake, err := runner.nextEnvelope(
			ctx, prepared.work.Order, *next,
		)
		if err != nil {
			return err
		}
		if _, err := runner.Scheduler.CompleteAndEnqueue(
			ctx, prepared.wake.OrganizationID,
			prepared.wake.WakeID, 0, nextWake,
		); err != nil {
			return err
		}
		nextWakeQueued = true
	case contracts.DispositionGoalCompleted:
		if err := runner.Scheduler.Complete(
			ctx, prepared.wake.OrganizationID,
			prepared.wake.WakeID, 0,
		); err != nil {
			return err
		}
	case contracts.DispositionBlocked:
		if err := runner.Scheduler.Complete(
			ctx, prepared.wake.OrganizationID,
			prepared.wake.WakeID, 0,
		); err != nil {
			return err
		}
		return runner.lifecycle(
			ctx, prepared.wake.WakeID, "wake.waiting", false, "",
			map[string]any{
				"state": "waiting", "intent_id": prepared.intent,
				"reason": prepared.checkpoint.ReasonCode,
			},
		)
	default:
		return fmt.Errorf(
			"wakeruntime: unsupported terminal disposition %q",
			prepared.checkpoint.Disposition,
		)
	}
	return runner.lifecycle(
		ctx, prepared.wake.WakeID, "wake.receipt_committed", true,
		prepared.checkpoint.ReceiptID,
		map[string]any{
			"state": "verified", "intent_id": prepared.intent,
			"next_wake_queued": nextWakeQueued,
		},
	)
}

func (runner *Runner) loadPlan(
	ctx context.Context,
	prepared *preparedClaim,
) (workcompile.Plan, error) {
	return runner.Compiler.Load(
		ctx, prepared.organization, "proposal:"+prepared.wake.WakeID,
	)
}

func (runner *Runner) openPacket(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	wakeID contracts.WakeID,
) (contracts.WorkPacket, bool, error) {
	content, _, err := runner.Execution.OpenArtifact(
		ctx, organizationID, wakeID, artifactWorkPacket,
	)
	if errors.Is(err, execution.ErrConflict) {
		return contracts.WorkPacket{}, false, nil
	}
	if err != nil {
		return contracts.WorkPacket{}, false, err
	}
	value, err := contracts.DecodeCanonical[
		contracts.WorkPacket, *contracts.WorkPacket,
	](content)
	if err != nil {
		return contracts.WorkPacket{}, false, err
	}
	return value, true, nil
}

func (runner *Runner) openSeatOutput(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	wakeID contracts.WakeID,
) (actorstate.SeatOutput, bool, error) {
	content, _, err := runner.Execution.OpenArtifact(
		ctx, organizationID, wakeID, artifactSeatOutput,
	)
	if errors.Is(err, execution.ErrConflict) {
		return actorstate.SeatOutput{}, false, nil
	}
	if err != nil {
		return actorstate.SeatOutput{}, false, err
	}
	value, err := contracts.DecodeCanonical[
		actorstate.SeatOutput, *actorstate.SeatOutput,
	](content)
	if err != nil {
		return actorstate.SeatOutput{}, false, err
	}
	return value, true, nil
}

func (runner *Runner) openAuditorLease(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	wakeID contracts.WakeID,
) (contracts.WakeLease, lease.Grant, bool, error) {
	content, _, err := runner.Execution.OpenArtifact(
		ctx, organizationID, wakeID, artifactAuditorLease,
	)
	if errors.Is(err, execution.ErrConflict) {
		return contracts.WakeLease{}, lease.Grant{}, false, nil
	}
	if err != nil {
		return contracts.WakeLease{}, lease.Grant{}, false, err
	}
	value, err := contracts.DecodeCanonical[
		contracts.WakeLease, *contracts.WakeLease,
	](content)
	if err != nil {
		return contracts.WakeLease{}, lease.Grant{}, false, err
	}
	return value, lease.Grant{}, true, nil
}

func (runner *Runner) putCanonicalArtifact(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	wakeID contracts.WakeID,
	kind string,
	value contracts.Validatable,
) error {
	content, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	_, err = runner.Execution.PutArtifact(
		ctx, organizationID, wakeID, kind, content,
	)
	return err
}

func validateRecoveredPacket(
	runner *Runner,
	wake scheduler.WakeEnvelope,
	work workorder.Context,
	organizationID contracts.OrganizationID,
	intentID contracts.IntentID,
	packet contracts.WorkPacket,
) error {
	if err := packet.Validate(); err != nil {
		return err
	}
	if packet.Lease.OrganizationID != organizationID ||
		packet.Lease.WakeID != contracts.WakeID(wake.WakeID) ||
		packet.Intent.ID != intentID ||
		packet.Seat.ID != contracts.SeatID(wake.SeatID) ||
		packet.Goal.ID != work.Goal.ID ||
		packet.Lease.Model.Provider != wake.Model.Provider ||
		packet.Lease.Model.ModelID != wake.Model.ModelID ||
		packet.Lease.MGS.Reference != wake.MGS.Reference ||
		packet.Lease.MGS.Digest.Digest != wake.MGS.Digest ||
		packet.Lease.Runtime != runner.Runtime {
		return fmt.Errorf(
			"wakeruntime: durable WorkPacket conflicts with claimed wake",
		)
	}
	return nil
}

func stageBefore(left, right execution.Stage) bool {
	order := map[execution.Stage]int{
		execution.StageLease: 0, execution.StageReconcile: 1,
		execution.StageOrient: 2, execution.StageSelect: 3,
		execution.StagePropose: 4, execution.StageCompile: 5,
		execution.StagePreflight: 6, execution.StageExecute: 7,
		execution.StageObserve: 8, execution.StageVerify: 9,
		execution.StageCommit: 10, execution.StageYield: 11,
		execution.StageSleep: 12,
	}
	return order[left] < order[right]
}

func auditPacket(
	packet contracts.WorkPacket,
	plan workcompile.Plan,
	observation contracts.EvidenceRef,
	auditor contracts.Seat,
) (contracts.VerdictPacket, error) {
	predicates := []contracts.VerificationPredicate{{
		ID: "provider-evidence", Kind: contracts.PredicateEvidenceHash,
		SubjectID:    string(observation.ID),
		ExpectedHash: &observation.Hash,
		Description:  "The fenced provider observation matches its durable content hash.",
	}}
	acceptance, err := acceptancePredicates(
		packet.RequiredOutputs, observation,
	)
	if err != nil {
		return contracts.VerdictPacket{}, err
	}
	predicates = append(predicates, acceptance...)
	result := contracts.VerdictPacket{
		SchemaVersion:   contracts.SchemaVersionV1,
		OrganizationID:  packet.Lease.OrganizationID,
		Intent:          packet.Intent,
		ExecutingSeatID: packet.Seat.ID,
		AuditorSeatID:   auditor.ID,
		Procedure: contracts.VerificationProcedureRef{
			ID:      "verify:" + string(plan.Skill.ID),
			Version: plan.Skill.Version, Digest: plan.VerifierDigest,
		},
		Predicates: predicates,
		Skill:      plan.Skill, VerifierDigest: plan.VerifierDigest,
		Artifacts:    packet.Artifacts,
		Observations: []contracts.EvidenceRef{observation},
		Model:        plan.Model, MGS: plan.MGS, Runtime: plan.Runtime,
		Source: plan.Source,
	}
	if err := result.Validate(); err != nil {
		return contracts.VerdictPacket{}, err
	}
	return result, nil
}

func acceptancePredicates(
	outputs []contracts.RequiredOutput,
	observation contracts.EvidenceRef,
) ([]contracts.VerificationPredicate, error) {
	predicates := make(
		[]contracts.VerificationPredicate, 0, len(outputs),
	)
	for index, output := range outputs {
		criterion, err := workorder.ParseAcceptanceCriterion(
			index, output.SuccessPredicate,
		)
		if err != nil {
			return nil, err
		}
		predicate := contracts.VerificationPredicate{
			ID: criterion.ID, Description: criterion.Description,
		}
		switch criterion.Kind {
		case workorder.AcceptanceEvidenceHash:
			predicate.Kind = contracts.PredicateEvidenceHash
			predicate.SubjectID = string(observation.ID)
			predicate.ExpectedHash = &observation.Hash
		case workorder.AcceptanceSemantic:
			predicate.Kind = contracts.PredicateSemantic
		default:
			return nil, fmt.Errorf(
				"wakeruntime: unsupported acceptance criterion kind %q",
				criterion.Kind,
			)
		}
		predicates = append(predicates, predicate)
	}
	return predicates, nil
}

func snapshotNode(
	snapshot dependency.Snapshot,
	id dependency.NodeID,
) (dependency.Node, bool) {
	for _, node := range snapshot.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return dependency.Node{}, false
}

func completedGoalExists(snapshot dependency.Snapshot) bool {
	for _, node := range snapshot.Nodes {
		if node.Kind == dependency.NodeGoal &&
			node.State == dependency.StateCompleted &&
			node.TerminalRecordID != nil {
			return true
		}
	}
	return false
}
