package continuity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Sidiora-Labs/centra-llm-agents/ion/internal/work"
	"github.com/google/uuid"
)

// Validates: Requirements 69.1
func TestApprovedGoalIsTheOnlyAuthorizationRoot(t *testing.T) {
	instance := openHarness(t)
	ctx := context.Background()
	contractID := uuid.New()
	first := instance.approve(t, standardProposal(contractID, "Prove drift-resistant execution"))
	if first.Version != 1 || first.Hash != GoalHash(first) {
		t.Fatalf("approved contract = %+v", first)
	}
	lease := instance.acquire(t, "worker-1")
	instance.open(t, lease, "analyze")

	revision := standardProposal(contractID, "Silently replace the delegated work")
	proposal, err := instance.runtime.ProposeGoalRevision(ctx, instance.actor, revision)
	if err != nil {
		t.Fatal(err)
	}
	current, err := instance.runtime.ApprovedGoal(ctx, instance.actor)
	if err != nil {
		t.Fatal(err)
	}
	if current.Hash != first.Hash || current.Version != 1 {
		t.Fatalf("unapproved proposal changed authority: %+v", current)
	}
	unapproved := GoalContract{
		ContractID: contractID, Goal: proposal.Goal, Constraints: proposal.Constraints,
		DoneCriteria: proposal.DoneCriteria, Plan: proposal.Plan,
	}
	envelope := instance.envelope(first, lease, "analyze", []string{"analysis"}, "unapproved")
	envelope.GoalHash = GoalHash(unapproved)
	if _, err := instance.runtime.Authorize(ctx, instance.actor, envelope); !errors.Is(err, ErrGoalMismatch) {
		t.Fatalf("proposal-derived authorization error = %v", err)
	}

	second, err := instance.runtime.ApproveGoalRevision(ctx, instance.actor, proposal.ID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != 2 || second.Hash != GoalHash(second) || second.Hash == first.Hash {
		t.Fatalf("second approved version = %+v", second)
	}
	versions, err := instance.runtime.ApprovedVersions(ctx, instance.actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Hash != first.Hash || versions[0].Goal != first.Goal {
		t.Fatalf("approved ledger mutated earlier version: %+v", versions)
	}
	stale := instance.envelope(first, lease, "analyze", []string{"analysis"}, "stale-goal")
	if _, err := instance.runtime.Authorize(ctx, instance.actor, stale); !errors.Is(err, ErrGoalMismatch) {
		t.Fatalf("superseded goal authorization error = %v", err)
	}
}

// Validates: Requirements 69.3
func TestActionAuthorizationFailsClosedWithoutCompleteCorrelation(t *testing.T) {
	instance := openHarness(t)
	ctx := context.Background()
	contract := instance.approve(t, standardProposal(uuid.New(), "Authorize only correlated actions"))
	lease := instance.acquire(t, "worker-1")
	instance.open(t, lease, "analyze")
	valid := instance.envelope(contract, lease, "analyze", []string{"analysis"}, "accepted")

	rejections := map[string]ActionEnvelope{}
	missingHash := valid
	missingHash.GoalHash, missingHash.IdempotencyKey = "", "missing-hash"
	rejections["absent goal hash"] = missingHash
	staleVersion := valid
	staleVersion.GoalVersion, staleVersion.IdempotencyKey = 99, "stale-version"
	rejections["stale goal version"] = staleVersion
	noParent := valid
	noParent.WorkItemID, noParent.IdempotencyKey = "", "no-parent"
	rejections["absent parent"] = noParent
	twoParents := valid
	twoParents.FrameID, twoParents.IdempotencyKey = uuid.New(), "two-parents"
	rejections["two parents"] = twoParents
	otherItem := valid
	otherItem.WorkItemID, otherItem.IdempotencyKey = "verify", "other-item"
	rejections["parent that is not open"] = otherItem
	noDelta := valid
	noDelta.Expected, noDelta.IdempotencyKey = EvidenceDelta{}, "no-delta"
	rejections["absent evidence delta"] = noDelta
	foreignCriterion := valid
	foreignCriterion.Expected.Criteria = []string{"checks"}
	foreignCriterion.IdempotencyKey = "foreign-criterion"
	rejections["uncorrelated evidence delta"] = foreignCriterion
	staleWorker := valid
	staleWorker.FencingToken, staleWorker.IdempotencyKey = lease.FencingToken+1, "stale-token"
	rejections["stale fencing token"] = staleWorker
	otherWorker := valid
	otherWorker.WorkerID, otherWorker.IdempotencyKey = "worker-2", "duplicate-worker"
	rejections["duplicate worker identity"] = otherWorker

	for name, envelope := range rejections {
		if _, err := instance.runtime.Authorize(ctx, instance.actor, envelope); err == nil {
			t.Fatalf("%s was authorized", name)
		}
	}
	actions, err := instance.runtime.Actions(ctx, instance.actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("rejected envelopes produced %d authorizations", len(actions))
	}

	authorization, err := instance.runtime.Authorize(ctx, instance.actor, valid)
	if err != nil {
		t.Fatal(err)
	}
	if authorization.GoalHash != contract.Hash || authorization.WorkItemID != "analyze" ||
		authorization.FrameID != nil || authorization.FencingToken != lease.FencingToken {
		t.Fatalf("authorization = %+v", authorization)
	}
	duplicate := valid
	if _, err := instance.runtime.Authorize(ctx, instance.actor, duplicate); !errors.Is(
		err, ErrDuplicateAction,
	) {
		t.Fatalf("duplicate idempotency key error = %v", err)
	}
	instance.clock.advance(2 * time.Minute)
	expired := valid
	expired.IdempotencyKey = "after-expiry"
	if _, err := instance.runtime.Authorize(ctx, instance.actor, expired); !errors.Is(
		err, ErrStaleWorker,
	) {
		t.Fatalf("expired lease error = %v", err)
	}
}

// Validates: Requirements 69.2, 69.4, 69.5
func TestRecoveryFramesNestOrderedWithBoundedParentDebits(t *testing.T) {
	instance := openHarness(t)
	ctx := context.Background()
	contract := instance.approve(t, standardProposal(uuid.New(), "Bound recovery"))
	lease := instance.acquire(t, "worker-1")
	instance.open(t, lease, "analyze")

	outer, err := instance.runtime.OpenRecoveryFrame(ctx, instance.actor, instance.frameInput(
		contract, lease, "analyze", []string{"analysis"},
		FrameBudget{ToolInvocations: 6, ModelTokens: 4096, ElapsedSeconds: 600,
			SubsequentErrors: 4, RetriesPerOperation: 2},
	))
	if err != nil {
		t.Fatal(err)
	}
	if outer.Depth != 1 || outer.ReturnTo != "analyze" || outer.ParentFrameID != nil ||
		outer.Status != FrameActive {
		t.Fatalf("outer frame = %+v", outer)
	}
	if outer.Budget.ToolInvocations != 6 || outer.Budget.RetriesPerOperation != 2 {
		t.Fatalf("frame budget was not honored: %+v", outer.Budget)
	}

	nonAllowlisted := instance.frameEnvelope(
		contract, lease, outer, StrategySwitchProvider, "not-allowlisted",
		ActionCost{ToolInvocations: 1},
	)
	if _, err := instance.runtime.Authorize(ctx, instance.actor, nonAllowlisted); !errors.Is(
		err, ErrStrategy,
	) {
		t.Fatalf("non-allowlisted strategy error = %v", err)
	}
	workItemParent := instance.envelope(contract, lease, "analyze", []string{"analysis"}, "bypass-frame")
	if _, err := instance.runtime.Authorize(ctx, instance.actor, workItemParent); !errors.Is(
		err, ErrParent,
	) {
		t.Fatalf("work-item parent during recovery error = %v", err)
	}
	revision := standardProposal(contract.ContractID, "Replace the goal during recovery")
	if _, err := instance.runtime.ProposeGoalRevision(ctx, instance.actor, revision); !errors.Is(
		err, ErrFrameActive,
	) {
		t.Fatalf("frame-originated revision error = %v", err)
	}

	authorized, err := instance.runtime.Authorize(ctx, instance.actor, instance.frameEnvelope(
		contract, lease, outer, StrategyRetryWithBackoff, "outer-1",
		ActionCost{ToolInvocations: 2, ModelTokens: 1024},
	))
	if err != nil {
		t.Fatal(err)
	}
	if authorized.FrameID == nil || *authorized.FrameID != outer.ID {
		t.Fatalf("authorization parent = %+v", authorized)
	}

	middle, err := instance.runtime.OpenRecoveryFrame(ctx, instance.actor, instance.frameInput(
		contract, lease, "analyze", []string{"analysis"},
		FrameBudget{ToolInvocations: 32, ModelTokens: MaxFrameModelTokens,
			ElapsedSeconds: 1800, SubsequentErrors: 8, RetriesPerOperation: 3},
	))
	if err != nil {
		t.Fatal(err)
	}
	if middle.Depth != 2 || middle.ParentFrameID == nil || *middle.ParentFrameID != outer.ID ||
		middle.ReturnTo != "analyze" || middle.OriginalWorkItemID != "analyze" {
		t.Fatalf("nested frame = %+v", middle)
	}
	if middle.Budget.ToolInvocations != 4 || middle.Budget.ModelTokens != 3072 ||
		middle.Budget.SubsequentErrors != 4 || middle.Budget.RetriesPerOperation != 2 {
		t.Fatalf("nested budget was not clamped to remaining parent budget: %+v", middle.Budget)
	}

	if _, err := instance.runtime.Authorize(ctx, instance.actor, instance.frameEnvelope(
		contract, lease, middle, StrategyAlternateTool, "middle-1",
		ActionCost{ToolInvocations: 2, ModelTokens: 512},
	)); err != nil {
		t.Fatal(err)
	}
	frames, err := instance.runtime.Frames(ctx, instance.actor)
	if err != nil {
		t.Fatal(err)
	}
	if frames[0].Consumed.ToolInvocations != 4 || frames[0].Consumed.ModelTokens != 1536 {
		t.Fatalf("parent was not atomically debited: %+v", frames[0].Consumed)
	}
	if frames[1].Consumed.ToolInvocations != 2 || frames[1].Consumed.ModelTokens != 512 {
		t.Fatalf("child debit = %+v", frames[1].Consumed)
	}

	inner, err := instance.runtime.OpenRecoveryFrame(ctx, instance.actor, instance.frameInput(
		contract, lease, "analyze", []string{"analysis"}, FrameBudget{},
	))
	if err != nil {
		t.Fatal(err)
	}
	if inner.Depth != 3 || inner.ParentFrameID == nil || *inner.ParentFrameID != middle.ID {
		t.Fatalf("depth three frame = %+v", inner)
	}
	if _, err := instance.runtime.OpenRecoveryFrame(ctx, instance.actor, instance.frameInput(
		contract, lease, "analyze", []string{"analysis"}, FrameBudget{},
	)); !errors.Is(err, ErrFrameDepth) {
		t.Fatalf("fourth simultaneous frame error = %v", err)
	}
}

// Validates: Requirements 69.6
func TestBudgetExhaustionClosesFrameAndAppliesDeclaredDisposition(t *testing.T) {
	instance := openHarness(t)
	ctx := context.Background()
	contract := instance.approve(t, standardProposal(uuid.New(), "Exhaust honestly"))
	lease := instance.acquire(t, "worker-1")
	instance.open(t, lease, "analyze")
	input := instance.frameInput(contract, lease, "analyze", []string{"analysis"},
		FrameBudget{ToolInvocations: 1, ModelTokens: 1024, ElapsedSeconds: 600,
			SubsequentErrors: 2, RetriesPerOperation: 1})
	input.Disposition = DispositionEscalated
	frame, err := instance.runtime.OpenRecoveryFrame(ctx, instance.actor, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.runtime.Authorize(ctx, instance.actor, instance.frameEnvelope(
		contract, lease, frame, StrategyRetryWithBackoff, "spend-1",
		ActionCost{ToolInvocations: 1},
	)); err != nil {
		t.Fatal(err)
	}
	before, err := instance.runtime.Actions(ctx, instance.actor)
	if err != nil {
		t.Fatal(err)
	}
	exhaustErr := func() error {
		_, err := instance.runtime.Authorize(ctx, instance.actor, instance.frameEnvelope(
			contract, lease, frame, StrategyRetryWithBackoff, "spend-2",
			ActionCost{ToolInvocations: 1},
		))
		return err
	}()
	var exhaustion *ExhaustionError
	if !errors.As(exhaustErr, &exhaustion) || !errors.Is(exhaustErr, ErrBudgetExhausted) {
		t.Fatalf("exhaustion error = %v", exhaustErr)
	}
	if len(exhaustion.Closures) != 1 || exhaustion.Closures[0].Status != FrameClosedExhaust ||
		exhaustion.Closures[0].Disposition != DispositionEscalated ||
		exhaustion.Closures[0].OriginalWorkItemID != "analyze" ||
		exhaustion.Closures[0].UnmetExit == "" ||
		exhaustion.Closures[0].Consumed.ToolInvocations != 1 {
		t.Fatalf("closure = %+v", exhaustion.Closures)
	}
	after, err := instance.runtime.Actions(ctx, instance.actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("exhausted authorization produced an effect: %d -> %d", len(before), len(after))
	}
	if _, err := instance.runtime.Authorize(ctx, instance.actor, instance.frameEnvelope(
		contract, lease, frame, StrategyRetryWithBackoff, "after-close",
		ActionCost{ToolInvocations: 1},
	)); !errors.Is(err, ErrParent) {
		t.Fatalf("action under closed frame error = %v", err)
	}
	portfolio, err := instance.work.Get(ctx, instance.actor)
	if err != nil {
		t.Fatal(err)
	}
	item, found := findWorkItem(portfolio, contract.ContractID, "analyze")
	if !found || item.Status != work.WorkItemBlocked ||
		item.BlockingNote == "" || !containsSubstring(item.BlockingNote, "escalated") {
		t.Fatalf("parent disposition = %+v", item)
	}
	dispositions, err := instance.runtime.Dispositions(ctx, instance.actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispositions) != 1 || dispositions[0].Disposition != DispositionEscalated ||
		dispositions[0].WorkItemID != "analyze" || dispositions[0].UnmetExit == "" {
		t.Fatalf("disposition ledger = %+v", dispositions)
	}
	frames, err := instance.runtime.Frames(ctx, instance.actor)
	if err != nil {
		t.Fatal(err)
	}
	for _, recorded := range frames {
		if recorded.Status == FrameActive {
			t.Fatalf("frame remained active after exhaustion: %+v", recorded)
		}
	}
}

// Validates: Requirements 69.11
func TestVerifiedFrameExitPopsOneFrameAndResumesParent(t *testing.T) {
	instance := openHarness(t)
	ctx := context.Background()
	contract := instance.approve(t, standardProposal(uuid.New(), "Return to the original work"))
	lease := instance.acquire(t, "worker-1")
	instance.open(t, lease, "analyze")
	outer, err := instance.runtime.OpenRecoveryFrame(ctx, instance.actor, instance.frameInput(
		contract, lease, "analyze", []string{"analysis"}, FrameBudget{},
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.runtime.Authorize(ctx, instance.actor, instance.frameEnvelope(
		contract, lease, outer, StrategyRetryWithBackoff, "outer-work",
		ActionCost{ToolInvocations: 3, ModelTokens: 900},
	)); err != nil {
		t.Fatal(err)
	}
	inner, err := instance.runtime.OpenRecoveryFrame(ctx, instance.actor, instance.frameInput(
		contract, lease, "analyze", []string{"analysis"}, FrameBudget{},
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.runtime.VerifyFrameExit(ctx, instance.actor, inner.ID); !errors.Is(
		err, ErrExitUnverified,
	) {
		t.Fatalf("unverified exit error = %v", err)
	}
	instance.evidence(t, lease, ScopeCriterion, []string{"analysis"}, "analysis.txt")
	innerClosure, err := instance.runtime.VerifyFrameExit(ctx, instance.actor, inner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if innerClosure.ResumedFrameID == nil || *innerClosure.ResumedFrameID != outer.ID ||
		innerClosure.Status != FrameClosedVerified {
		t.Fatalf("inner closure = %+v", innerClosure)
	}
	outerClosure, err := instance.runtime.VerifyFrameExit(ctx, instance.actor, outer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if outerClosure.ResumedWorkItemID != "analyze" || outerClosure.ResumedFrameID != nil ||
		outerClosure.Consumed.ToolInvocations != 3 || outerClosure.Consumed.ModelTokens != 900 {
		t.Fatalf("outer closure = %+v", outerClosure)
	}
	snapshot, err := instance.runtime.Snapshot(ctx, instance.actor)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.OpenWorkItemID != "analyze" || len(activeOf(snapshot.Frames)) != 0 ||
		snapshot.Contract.Hash != contract.Hash || snapshot.Contract.Version != contract.Version {
		t.Fatalf("snapshot after frame return = %+v", snapshot)
	}
	// The returned-to original work item now carries server-verified evidence for
	// its only criterion, so the next eligible item is the following plan node
	// while the open work item stays the exact return_to identifier.
	if snapshot.NextWorkItemID != "verify" {
		t.Fatalf("next eligible item after frame return = %q", snapshot.NextWorkItemID)
	}
	dispositions, err := instance.runtime.Dispositions(ctx, instance.actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispositions) != 0 {
		t.Fatalf("successful frames applied a failure disposition: %+v", dispositions)
	}
	completed, err := instance.runtime.CompleteWorkItem(ctx, instance.actor, "analyze")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != work.WorkItemCompleted {
		t.Fatalf("completed item = %+v", completed)
	}
}

// Validates: Requirements 69.8
func TestCompletionRequiresCurrentGoalCorrelatedEvidence(t *testing.T) {
	instance := openHarness(t)
	ctx := context.Background()
	contractID := uuid.New()
	contract := instance.approve(t, standardProposal(contractID, "Complete only from evidence"))
	if contract.Version != 1 {
		t.Fatalf("first approved version = %d", contract.Version)
	}
	lease := instance.acquire(t, "worker-1")
	instance.open(t, lease, "analyze")
	if _, err := instance.runtime.CompleteWorkItem(ctx, instance.actor, "analyze"); !errors.Is(
		err, ErrEvidenceMissing,
	) {
		t.Fatalf("completion without evidence error = %v", err)
	}
	reference := instance.writeFile(t, "strategy.txt", "the alternate tool ran")
	if _, err := instance.runtime.VerifyEvidence(ctx, instance.actor, EvidenceInput{
		WorkerID: lease.WorkerID, FencingToken: lease.FencingToken,
		Scope: ScopeRecoveryStrategy, Kind: "log", Title: "strategy log",
		Reference: reference, Criteria: []string{"analysis"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.runtime.CompleteWorkItem(ctx, instance.actor, "analyze"); !errors.Is(
		err, ErrEvidenceMissing,
	) {
		t.Fatalf("strategy-scoped completion error = %v", err)
	}
	instance.evidence(t, lease, ScopeCriterion, []string{"analysis"}, "analysis.txt")
	if _, err := instance.runtime.CompleteWorkItem(ctx, instance.actor, "analyze"); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.runtime.CompleteGoal(ctx, instance.actor); !errors.Is(
		err, ErrEvidenceMissing,
	) {
		t.Fatalf("partial goal completion error = %v", err)
	}
	instance.open(t, lease, "verify")
	instance.evidence(t, lease, ScopeCriterion, []string{"checks"}, "checks.txt")

	superseded := standardProposal(contractID, "Prove drift resistance with an added criterion")
	superseded.DoneCriteria = append(superseded.DoneCriteria, Criterion{
		ID: "restart", Description: "restart evidence exists",
	})
	superseded.Plan = append(superseded.Plan, PlanNode{
		ID: "restart", Title: "Prove restart", Criteria: []string{"restart"},
	})
	revised := instance.approve(t, superseded)
	if revised.Version != 2 {
		t.Fatalf("revised version = %d", revised.Version)
	}
	if _, err := instance.runtime.CompleteGoal(ctx, instance.actor); !errors.Is(
		err, ErrEvidenceMissing,
	) {
		t.Fatalf("superseded-version evidence completion error = %v", err)
	}
	current := instance.acquire(t, "worker-2")
	for criterion, name := range map[string]string{
		"analysis": "analysis-v2.txt", "checks": "checks-v2.txt", "restart": "restart-v2.txt",
	} {
		instance.evidence(t, current, ScopeCriterion, []string{criterion}, name)
	}
	completed, err := instance.runtime.CompleteGoal(ctx, instance.actor)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != work.StatusCompleted || completed.CompletedAt == nil {
		t.Fatalf("completed contract = %+v", completed)
	}
}

func containsSubstring(value, wanted string) bool {
	return len(value) >= len(wanted) && (value == wanted ||
		len(wanted) == 0 || indexOf(value, wanted) >= 0)
}

func indexOf(value, wanted string) int {
	for index := 0; index+len(wanted) <= len(value); index++ {
		if value[index:index+len(wanted)] == wanted {
			return index
		}
	}
	return -1
}
