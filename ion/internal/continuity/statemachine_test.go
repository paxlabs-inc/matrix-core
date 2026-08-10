package continuity

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/paxlabs-inc/ion-agent/internal/work"
)

const (
	stateMachineTraces          = 100
	stateMachineTransitions     = 100
	stateMachineMinTransitions  = 10000
	stateMachineMaxWorkers      = 16
	requiredFailureClassSamples = 10
)

// stateMachineWorkerCount bounds how many seeded traces execute at once. Each
// trace drives its own durable stack whose single SQLite connection blocks the
// trace goroutine for most of a transition, so the pool is sized to the
// available processors and capped to keep open databases bounded.
func stateMachineWorkerCount() int {
	workers := goruntime.GOMAXPROCS(0)
	if workers < 1 {
		return 1
	}
	if workers > stateMachineMaxWorkers {
		return stateMachineMaxWorkers
	}
	return workers
}

type failureClass string

const (
	classToolError          failureClass = "tool_error"
	classMalformedModel     failureClass = "malformed_model_response"
	classRepeatedRoadblock  failureClass = "repeated_roadblock"
	classProviderChange     failureClass = "provider_change"
	classContextTruncation  failureClass = "context_truncation"
	classProcessRestart     failureClass = "process_crash_and_restart"
	classStaleWorker        failureClass = "stale_worker"
	classDuplicateWorker    failureClass = "duplicate_worker"
	classWorkerDeath        failureClass = "worker_death"
	classBudgetExhaustion   failureClass = "budget_exhaustion"
	classTerminalFrameError failureClass = "terminal_frame_failure"
)

var requiredClasses = []failureClass{
	classToolError, classMalformedModel, classRepeatedRoadblock, classProviderChange,
	classContextTruncation, classProcessRestart, classStaleWorker,
	classDuplicateWorker, classWorkerDeath,
}

type model struct {
	harness   *harness
	contract  GoalContract
	lease     WorkerLease
	workers   int
	openItem  string
	frames    []RecoveryFrame
	covered   map[string]bool
	completed map[string]bool
	goalDone  bool
	keys      int
	seed      int64
	step      int
}

// TestGoalBoundStateMachineHoldsEveryInvariant runs the reproducible seeded
// property-based state-machine suite required by acceptance criterion 69.9.
// Each seed owns an independent production stack, so traces execute on a
// bounded worker pool and merge their observed failure classes afterwards.
func TestGoalBoundStateMachineHoldsEveryInvariant(t *testing.T) {
	var mutex sync.Mutex
	counts := map[failureClass]int{}
	transitions := 0
	workers := make(chan struct{}, stateMachineWorkerCount())
	t.Run("traces", func(t *testing.T) {
		for seed := int64(1); seed <= stateMachineTraces; seed++ {
			t.Run(fmt.Sprintf("seed-%03d", seed), func(t *testing.T) {
				t.Parallel()
				workers <- struct{}{}
				defer func() { <-workers }()
				executed, observed := runStateMachineTrace(t, seed)
				mutex.Lock()
				defer mutex.Unlock()
				transitions += executed
				for class, count := range observed {
					counts[class] += count
				}
			})
		}
	})
	if transitions < stateMachineMinTransitions {
		t.Fatalf(
			"state machine executed %d transitions, want at least %d",
			transitions, stateMachineMinTransitions,
		)
	}
	for _, class := range requiredClasses {
		if counts[class] < requiredFailureClassSamples {
			t.Fatalf(
				"failure class %s appeared in %d transitions, want at least %d: %+v",
				class, counts[class], requiredFailureClassSamples, counts,
			)
		}
	}
}

// runStateMachineTrace executes one reproducible seeded trace and asserts every
// criterion 69.9 invariant after every transition.
func runStateMachineTrace(t *testing.T, seed int64) (int, map[failureClass]int) {
	t.Helper()
	counts := map[failureClass]int{}
	trace := newModel(t, seed)
	random := rand.New(rand.NewSource(seed))
	transitions := 0
	for step := 0; step < stateMachineTransitions; step++ {
		trace.step = step
		class := trace.transition(t, random)
		transitions++
		if class != "" {
			counts[class]++
		}
		trace.assertInvariants(t)
	}
	trace.harness.close(t)
	return transitions, counts
}

func newModel(t *testing.T, seed int64) *model {
	t.Helper()
	instance := openDetachedHarness(t)
	trace := &model{
		harness: instance, covered: map[string]bool{}, completed: map[string]bool{},
		seed: seed,
	}
	trace.contract = instance.approve(t, standardProposal(uuid.New(), fmt.Sprintf(
		"Prove goal-bound continuity for trace %d", seed,
	)))
	trace.lease = instance.acquire(t, "worker-0")
	return trace
}

func (trace *model) fail(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Fatalf(
		"seed %d step %d: %s\nmodel: open=%q frames=%d goalDone=%v covered=%v\ndurable: %s",
		trace.seed, trace.step, fmt.Sprintf(format, args...), trace.openItem,
		len(trace.frames), trace.goalDone, trace.covered, trace.durableSummary(t),
	)
}

func (trace *model) durableSummary(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	portfolio, err := trace.harness.work.Get(ctx, trace.harness.actor)
	if err != nil {
		return "portfolio unavailable: " + err.Error()
	}
	summary := ""
	for _, item := range portfolio.WorkItems {
		summary += fmt.Sprintf("%s=%s ", item.ID, item.Status)
	}
	frames, err := trace.harness.runtime.Frames(ctx, trace.harness.actor)
	if err != nil {
		return summary + "| frames unavailable: " + err.Error()
	}
	for _, frame := range frames {
		summary += fmt.Sprintf("| frame %d %s return=%s ", frame.Depth, frame.Status, frame.ReturnTo)
	}
	return summary
}

func (trace *model) nextKey(prefix string) string {
	trace.keys++
	return fmt.Sprintf("%s-%d-%d", prefix, trace.seed, trace.keys)
}

func (trace *model) transition(t *testing.T, random *rand.Rand) failureClass {
	t.Helper()
	ctx := context.Background()
	trace.reconcileOpenItem(t, ctx)
	if random.Intn(24) == 0 {
		return trace.reviseGoal(t, ctx)
	}
	switch random.Intn(16) {
	case 0, 1:
		return trace.progressWork(t, ctx)
	case 2:
		return trace.injectToolError(t, ctx)
	case 3:
		return trace.injectMalformedModelResponse(t, ctx, random)
	case 4:
		return trace.injectRepeatedRoadblock(t, ctx)
	case 5:
		return trace.continueUnder(t, ctx, CauseProviderChange, classProviderChange)
	case 6:
		return trace.continueUnder(t, ctx, CauseContextCompression, classContextTruncation)
	case 7:
		return trace.crashAndRestart(t, ctx)
	case 8:
		return trace.continueUnder(t, ctx, CauseWorkerDeath, classWorkerDeath)
	case 9:
		return trace.continueUnder(t, ctx, CauseWorkerReplacement, "")
	case 10:
		return trace.staleWorker(t, ctx)
	case 11:
		return trace.duplicateWorker(t, ctx)
	case 12:
		return trace.recordEvidence(t, ctx, random)
	case 13:
		return trace.closeFrame(t, ctx, random)
	case 14:
		return trace.completeWork(t, ctx)
	default:
		return trace.progressWork(t, ctx)
	}
}

// reconcileOpenItem mirrors durable work-item transitions the production work
// service performs when verified criterion evidence lands or a recovery
// disposition blocks the parent.
func (trace *model) reconcileOpenItem(t *testing.T, ctx context.Context) {
	t.Helper()
	if trace.openItem == "" || len(trace.frames) > 0 {
		return
	}
	portfolio, err := trace.harness.work.Get(ctx, trace.harness.actor)
	if err != nil {
		trace.fail(t, "portfolio: %v", err)
	}
	item, found := findWorkItem(portfolio, trace.contract.ContractID, trace.openItem)
	if !found {
		trace.openItem = ""
		return
	}
	switch item.Status {
	case work.WorkItemCompleted:
		covered := true
		for _, criterion := range item.Criteria {
			if !trace.covered[criterion] {
				covered = false
				break
			}
		}
		if covered {
			if _, err := trace.harness.runtime.CompleteWorkItem(
				ctx, trace.harness.actor, trace.openItem,
			); err != nil {
				trace.fail(t, "complete verified item: %v", err)
			}
		}
		trace.completed[trace.openItem] = true
		trace.openItem = ""
	case work.WorkItemBlocked:
		trace.openItem = ""
	}
}

// reviseGoal exercises the only authoritative revision path: a versioned
// proposal plus explicit approval at a strictly increased version.
func (trace *model) reviseGoal(t *testing.T, ctx context.Context) failureClass {
	t.Helper()
	if len(trace.frames) > 0 || trace.goalDone {
		return trace.staleWorker(t, ctx)
	}
	input := standardProposal(trace.contract.ContractID, fmt.Sprintf(
		"Prove goal-bound continuity for trace %d revision %d", trace.seed,
		trace.contract.Version+1,
	))
	proposal, err := trace.harness.runtime.ProposeGoalRevision(ctx, trace.harness.actor, input)
	if err != nil {
		trace.fail(t, "propose revision: %v", err)
	}
	before, err := trace.harness.runtime.ApprovedGoal(ctx, trace.harness.actor)
	if err != nil {
		trace.fail(t, "approved goal: %v", err)
	}
	if before.Hash != trace.contract.Hash || before.Version != trace.contract.Version {
		trace.fail(t, "unapproved proposal changed the authorization root: %+v", before)
	}
	revised, err := trace.harness.runtime.ApproveGoalRevision(
		ctx, trace.harness.actor, proposal.ID, "operator",
	)
	if err != nil {
		trace.fail(t, "approve revision: %v", err)
	}
	if revised.Version != trace.contract.Version+1 || revised.Hash == trace.contract.Hash {
		trace.fail(t, "revision did not strictly increase the approved version: %+v", revised)
	}
	trace.contract = revised
	trace.covered = map[string]bool{}
	snapshot, err := trace.harness.runtime.Snapshot(ctx, trace.harness.actor)
	if err != nil {
		trace.fail(t, "snapshot after revision: %v", err)
	}
	trace.openItem = snapshot.OpenWorkItemID
	return ""
}

func (trace *model) ensureOpenItem(t *testing.T, ctx context.Context) string {
	t.Helper()
	if trace.openItem != "" || trace.goalDone {
		return trace.openItem
	}
	snapshot, err := trace.harness.runtime.Snapshot(ctx, trace.harness.actor)
	if err != nil {
		trace.fail(t, "snapshot: %v", err)
	}
	if snapshot.NextWorkItemID == "" {
		return ""
	}
	item, err := trace.harness.runtime.OpenWorkItem(
		ctx, trace.harness.actor, trace.lease.WorkerID, trace.lease.FencingToken,
		snapshot.NextWorkItemID,
	)
	if err != nil {
		trace.fail(t, "open %s: %v", snapshot.NextWorkItemID, err)
	}
	trace.openItem = item.ID
	return trace.openItem
}

func (trace *model) parentCriteria(t *testing.T, ctx context.Context) []string {
	t.Helper()
	if len(trace.frames) > 0 {
		return trace.frames[len(trace.frames)-1].Exit.Criteria
	}
	portfolio, err := trace.harness.work.Get(ctx, trace.harness.actor)
	if err != nil {
		trace.fail(t, "portfolio: %v", err)
	}
	item, found := findWorkItem(portfolio, trace.contract.ContractID, trace.openItem)
	if !found {
		trace.fail(t, "open item %q is missing from the plan", trace.openItem)
	}
	return item.Criteria
}

func (trace *model) authorize(
	t *testing.T, ctx context.Context, envelope ActionEnvelope,
) (Authorization, bool) {
	t.Helper()
	authorization, err := trace.harness.runtime.Authorize(ctx, trace.harness.actor, envelope)
	if err == nil {
		return authorization, true
	}
	var exhaustion *ExhaustionError
	if errors.As(err, &exhaustion) {
		trace.applyClosures(exhaustion.Closures)
		return Authorization{}, false
	}
	trace.fail(t, "authorize: %v", err)
	return Authorization{}, false
}

func (trace *model) applyClosures(closures []FrameClosure) {
	if len(closures) == 0 {
		return
	}
	lowest := closures[len(closures)-1].Depth
	kept := make([]RecoveryFrame, 0, len(trace.frames))
	for _, frame := range trace.frames {
		if frame.Depth < lowest {
			kept = append(kept, frame)
		}
	}
	trace.frames = kept
	if len(trace.frames) == 0 {
		trace.openItem = ""
	}
}

func (trace *model) progressWork(t *testing.T, ctx context.Context) failureClass {
	t.Helper()
	if trace.goalDone {
		return trace.continueUnder(t, ctx, CauseWorkerReplacement, "")
	}
	if trace.ensureOpenItem(t, ctx) == "" {
		trace.completeGoalWhenCovered(t, ctx)
		return ""
	}
	criteria := trace.parentCriteria(t, ctx)
	envelope := ActionEnvelope{
		GoalHash: trace.contract.Hash, GoalVersion: trace.contract.Version,
		Expected: EvidenceDelta{Description: "bounded progress", Criteria: criteria},
		WorkerID: trace.lease.WorkerID, FencingToken: trace.lease.FencingToken,
		Kind: ActionTool, OperationID: trace.nextKey("operation"),
		IdempotencyKey: trace.nextKey("progress"), Cost: ActionCost{ToolInvocations: 1},
	}
	if len(trace.frames) > 0 {
		frame := trace.frames[len(trace.frames)-1]
		envelope.FrameID, envelope.Strategy = frame.ID, StrategyRetryWithBackoff
	} else {
		envelope.WorkItemID = trace.openItem
	}
	authorization, ok := trace.authorize(t, ctx, envelope)
	if !ok {
		return classBudgetExhaustion
	}
	settlement, err := trace.harness.runtime.SettleAction(
		ctx, trace.harness.actor, authorization.ID, ActionOutcome{Succeeded: true},
	)
	if err != nil {
		trace.fail(t, "settle success: %v", err)
	}
	if !settlement.Record.Succeeded {
		trace.fail(t, "settlement did not record success: %+v", settlement.Record)
	}
	return ""
}

func (trace *model) injectToolError(t *testing.T, ctx context.Context) failureClass {
	t.Helper()
	if trace.goalDone || trace.ensureOpenItem(t, ctx) == "" {
		return trace.staleWorker(t, ctx)
	}
	criteria := trace.parentCriteria(t, ctx)
	envelope := ActionEnvelope{
		GoalHash: trace.contract.Hash, GoalVersion: trace.contract.Version,
		Expected: EvidenceDelta{Description: "tool output", Criteria: criteria},
		WorkerID: trace.lease.WorkerID, FencingToken: trace.lease.FencingToken,
		Kind: ActionTool, OperationID: trace.nextKey("operation"),
		IdempotencyKey: trace.nextKey("tool-error"), Cost: ActionCost{ToolInvocations: 1},
	}
	if len(trace.frames) > 0 {
		frame := trace.frames[len(trace.frames)-1]
		envelope.FrameID, envelope.Strategy = frame.ID, StrategyAlternateTool
	} else {
		envelope.WorkItemID = trace.openItem
	}
	authorization, ok := trace.authorize(t, ctx, envelope)
	if !ok {
		return classBudgetExhaustion
	}
	settlement, err := trace.harness.runtime.SettleAction(
		ctx, trace.harness.actor, authorization.ID,
		ActionOutcome{Error: "tool error: exit status 1"},
	)
	if err != nil {
		trace.fail(t, "settle tool error: %v", err)
	}
	trace.applyClosures(settlement.ClosedFrames)
	if len(settlement.ClosedFrames) > 0 {
		return classBudgetExhaustion
	}
	trace.openFrame(t, ctx, "tool error interrupted the open work item", FrameBudget{
		ToolInvocations: 4, ModelTokens: 4096, ElapsedSeconds: 600,
		SubsequentErrors: 3, RetriesPerOperation: 2,
	})
	return classToolError
}

func (trace *model) injectMalformedModelResponse(
	t *testing.T, ctx context.Context, random *rand.Rand,
) failureClass {
	t.Helper()
	if trace.goalDone || trace.ensureOpenItem(t, ctx) == "" {
		return trace.duplicateWorker(t, ctx)
	}
	if len(trace.frames) == 0 {
		trace.openFrame(t, ctx, "malformed model response interrupted the work item", FrameBudget{
			ToolInvocations: 4, ModelTokens: 8192, ElapsedSeconds: 900,
			SubsequentErrors: 3, RetriesPerOperation: 2,
		})
	}
	frame := trace.frames[len(trace.frames)-1]
	envelope := ActionEnvelope{
		GoalHash: trace.contract.Hash, GoalVersion: trace.contract.Version,
		FrameID: frame.ID,
		Expected: EvidenceDelta{
			Description: "reparsed model output", Criteria: frame.Exit.Criteria,
		},
		WorkerID: trace.lease.WorkerID, FencingToken: trace.lease.FencingToken,
		Kind: ActionModel, Strategy: StrategyReReadInputs,
		OperationID: trace.nextKey("operation"), IdempotencyKey: trace.nextKey("malformed"),
		Cost: ActionCost{ModelTokens: int64(256 + random.Intn(512))},
	}
	authorization, ok := trace.authorize(t, ctx, envelope)
	if !ok {
		return classBudgetExhaustion
	}
	settlement, err := trace.harness.runtime.SettleAction(
		ctx, trace.harness.actor, authorization.ID,
		ActionOutcome{Error: "malformed model response: unterminated tool call JSON"},
	)
	if err != nil {
		trace.fail(t, "settle malformed model response: %v", err)
	}
	trace.applyClosures(settlement.ClosedFrames)
	return classMalformedModel
}

func (trace *model) injectRepeatedRoadblock(t *testing.T, ctx context.Context) failureClass {
	t.Helper()
	if trace.goalDone || trace.ensureOpenItem(t, ctx) == "" {
		return trace.staleWorker(t, ctx)
	}
	if len(trace.frames) == 0 {
		trace.openFrame(t, ctx, "repeated roadblock on the same operation", FrameBudget{
			ToolInvocations: 6, ModelTokens: 4096, ElapsedSeconds: 600,
			SubsequentErrors: 4, RetriesPerOperation: 2,
		})
	}
	frame := trace.frames[len(trace.frames)-1]
	operation := fmt.Sprintf("roadblock-%d-%d", trace.seed, frame.Depth)
	blocked := false
	for attempt := 0; attempt < 3; attempt++ {
		if len(trace.frames) == 0 {
			break
		}
		frame = trace.frames[len(trace.frames)-1]
		envelope := ActionEnvelope{
			GoalHash: trace.contract.Hash, GoalVersion: trace.contract.Version,
			FrameID: frame.ID,
			Expected: EvidenceDelta{
				Description: "retry of the blocked operation", Criteria: frame.Exit.Criteria,
			},
			WorkerID: trace.lease.WorkerID, FencingToken: trace.lease.FencingToken,
			Kind: ActionTool, Strategy: StrategyRetryWithBackoff, OperationID: operation,
			IdempotencyKey: trace.nextKey("roadblock"), Retry: true,
			Cost: ActionCost{ToolInvocations: 1},
		}
		authorization, ok := trace.authorize(t, ctx, envelope)
		if !ok {
			blocked = true
			break
		}
		settlement, err := trace.harness.runtime.SettleAction(
			ctx, trace.harness.actor, authorization.ID,
			ActionOutcome{Error: "roadblock persists: dependency unavailable"},
		)
		if err != nil {
			trace.fail(t, "settle roadblock: %v", err)
		}
		trace.applyClosures(settlement.ClosedFrames)
		if len(settlement.ClosedFrames) > 0 {
			blocked = true
			break
		}
	}
	if blocked {
		return classRepeatedRoadblock
	}
	if len(trace.frames) > 0 && len(trace.frames) < MaxActiveFrames {
		trace.openFrame(t, ctx, "nested recovery after a repeated roadblock", FrameBudget{})
	}
	return classRepeatedRoadblock
}

func (trace *model) openFrame(
	t *testing.T, ctx context.Context, cause string, budget FrameBudget,
) {
	t.Helper()
	if len(trace.frames) >= MaxActiveFrames || trace.openItem == "" {
		return
	}
	input := OpenFrameInput{
		WorkerID: trace.lease.WorkerID, FencingToken: trace.lease.FencingToken,
		GoalHash: trace.contract.Hash, GoalVersion: trace.contract.Version,
		WorkItemID: trace.openItem, Cause: cause,
		Exit: ExitCondition{
			Description: "the interrupted criterion has fresh verified evidence",
			Criteria:    trace.itemCriteria(t, ctx),
		},
		Disposition: DispositionBlocked,
		Allowlist: []Strategy{
			StrategyRetryWithBackoff, StrategyAlternateTool, StrategyReReadInputs,
			StrategySmallerBatch,
		},
		Budget: budget,
	}
	frame, err := trace.harness.runtime.OpenRecoveryFrame(ctx, trace.harness.actor, input)
	if err != nil {
		var exhaustion *ExhaustionError
		if errors.As(err, &exhaustion) {
			return
		}
		trace.fail(t, "open recovery frame: %v", err)
	}
	trace.frames = append(trace.frames, frame)
}

func (trace *model) itemCriteria(t *testing.T, ctx context.Context) []string {
	t.Helper()
	portfolio, err := trace.harness.work.Get(ctx, trace.harness.actor)
	if err != nil {
		trace.fail(t, "portfolio: %v", err)
	}
	item, found := findWorkItem(portfolio, trace.contract.ContractID, trace.openItem)
	if !found {
		trace.fail(t, "open item %q is missing", trace.openItem)
	}
	return item.Criteria
}

func (trace *model) continueUnder(
	t *testing.T, ctx context.Context, cause Cause, class failureClass,
) failureClass {
	t.Helper()
	trace.workers++
	previous := trace.lease
	before, err := trace.harness.runtime.Snapshot(ctx, trace.harness.actor)
	if err != nil {
		trace.fail(t, "snapshot before %s: %v", cause, err)
	}
	continuation, err := trace.harness.runtime.Continue(ctx, trace.harness.actor, ContinueInput{
		Cause: cause, WorkerID: fmt.Sprintf("worker-%d", trace.workers),
		Provider: "provider-" + string(cause), LeaseTTL: 5 * time.Minute,
	})
	if err != nil {
		trace.fail(t, "continue %s: %v", cause, err)
	}
	if continuation.Lease.FencingToken <= previous.FencingToken {
		trace.fail(t, "continuation did not invalidate prior worker authority: %+v", continuation.Lease)
	}
	restored := continuation.Snapshot
	if restored.Contract.Hash != trace.contract.Hash ||
		restored.Contract.Version != trace.contract.Version ||
		restored.Contract.Hash != before.Contract.Hash {
		trace.fail(t, "continuation changed the approved goal: %+v", restored.Contract)
	}
	if restored.OpenWorkItemID != before.OpenWorkItemID ||
		restored.PlanNodeID != before.PlanNodeID ||
		restored.NextWorkItemID != before.NextWorkItemID ||
		restored.ReturnTo != before.ReturnTo ||
		!reflect.DeepEqual(restored.Cursor, before.Cursor) {
		trace.fail(t, "continuation lost authoritative fields:\n%+v\n%+v", before, restored)
	}
	if !reflect.DeepEqual(restored.Frames, before.Frames) {
		trace.fail(t, "continuation changed the RecoveryFrame stack:\n%+v\n%+v",
			before.Frames, restored.Frames)
	}
	if len(activeOf(restored.Frames)) != len(trace.frames) {
		trace.fail(
			t, "continuation restored %d active frames, want %d",
			len(activeOf(restored.Frames)), len(trace.frames),
		)
	}
	for index, frame := range activeOf(restored.Frames) {
		if frame.ID != trace.frames[index].ID || frame.ReturnTo != trace.frames[index].ReturnTo {
			trace.fail(t, "continuation reordered the frame stack: %+v", frame)
		}
	}
	trace.lease = continuation.Lease
	stale := trace.staleAuthorizationDenied(t, ctx, previous)
	if !stale {
		trace.fail(t, "prior worker retained authority after %s", cause)
	}
	return class
}

func (trace *model) crashAndRestart(t *testing.T, ctx context.Context) failureClass {
	t.Helper()
	before, err := trace.harness.runtime.Snapshot(ctx, trace.harness.actor)
	if err != nil {
		trace.fail(t, "snapshot before restart: %v", err)
	}
	trace.harness.restart(t)
	after, err := trace.harness.runtime.Snapshot(ctx, trace.harness.actor)
	if err != nil {
		trace.fail(t, "snapshot after restart: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		trace.fail(t, "restart changed authoritative state:\n%+v\n%+v", before, after)
	}
	return trace.continueUnder(t, ctx, CauseProcessRestart, classProcessRestart)
}

func (trace *model) staleAuthorizationDenied(
	t *testing.T, ctx context.Context, stale WorkerLease,
) bool {
	t.Helper()
	before, err := trace.harness.runtime.Actions(ctx, trace.harness.actor)
	if err != nil {
		trace.fail(t, "actions: %v", err)
	}
	envelope := ActionEnvelope{
		GoalHash: trace.contract.Hash, GoalVersion: trace.contract.Version,
		Expected: EvidenceDelta{
			Description: "stale worker attempt", Criteria: []string{"analysis"},
		},
		WorkerID: stale.WorkerID, FencingToken: stale.FencingToken, Kind: ActionTool,
		OperationID: trace.nextKey("operation"), IdempotencyKey: trace.nextKey("stale"),
		Cost: ActionCost{ToolInvocations: 1},
	}
	if len(trace.frames) > 0 {
		envelope.FrameID = trace.frames[len(trace.frames)-1].ID
		envelope.Strategy = StrategyRetryWithBackoff
		envelope.Expected.Criteria = trace.frames[len(trace.frames)-1].Exit.Criteria
	} else {
		envelope.WorkItemID = trace.openItem
	}
	_, err = trace.harness.runtime.Authorize(ctx, trace.harness.actor, envelope)
	if !errors.Is(err, ErrStaleWorker) {
		return false
	}
	after, err := trace.harness.runtime.Actions(ctx, trace.harness.actor)
	if err != nil {
		trace.fail(t, "actions: %v", err)
	}
	return len(before) == len(after)
}

func (trace *model) staleWorker(t *testing.T, ctx context.Context) failureClass {
	t.Helper()
	stale := WorkerLease{
		WorkerID: trace.lease.WorkerID, FencingToken: trace.lease.FencingToken - 1,
		ExpiresAt: trace.lease.ExpiresAt,
	}
	if trace.lease.FencingToken == 1 {
		stale.FencingToken = trace.lease.FencingToken + 1
	}
	if !trace.staleAuthorizationDenied(t, ctx, stale) {
		trace.fail(t, "stale fencing token was authorized")
	}
	return classStaleWorker
}

func (trace *model) duplicateWorker(t *testing.T, ctx context.Context) failureClass {
	t.Helper()
	duplicate := WorkerLease{
		WorkerID: trace.lease.WorkerID + "-duplicate", FencingToken: trace.lease.FencingToken,
		ExpiresAt: trace.lease.ExpiresAt,
	}
	if !trace.staleAuthorizationDenied(t, ctx, duplicate) {
		trace.fail(t, "duplicate worker identity was authorized")
	}
	return classDuplicateWorker
}

func (trace *model) recordEvidence(
	t *testing.T, ctx context.Context, random *rand.Rand,
) failureClass {
	t.Helper()
	if trace.goalDone || trace.ensureOpenItem(t, ctx) == "" {
		return trace.staleWorker(t, ctx)
	}
	criteria := trace.itemCriteria(t, ctx)
	if len(criteria) == 0 {
		return ""
	}
	criterion := criteria[random.Intn(len(criteria))]
	scope := ScopeCriterion
	if random.Intn(5) == 0 {
		scope = ScopeRecoveryStrategy
	}
	trace.recordCriterionEvidence(t, ctx, criterion, scope)
	return ""
}

func (trace *model) closeFrame(
	t *testing.T, ctx context.Context, random *rand.Rand,
) failureClass {
	t.Helper()
	if len(trace.frames) == 0 {
		return trace.injectToolError(t, ctx)
	}
	frame := trace.frames[len(trace.frames)-1]
	verified := true
	for _, criterion := range frame.Exit.Criteria {
		if !trace.covered[criterion] {
			verified = false
			break
		}
	}
	if verified && random.Intn(4) != 0 {
		closure, err := trace.harness.runtime.VerifyFrameExit(ctx, trace.harness.actor, frame.ID)
		if err != nil {
			trace.fail(t, "verify frame exit: %v", err)
		}
		if closure.ReturnTo != trace.openItem {
			trace.fail(t, "closure return_to = %q, want %q", closure.ReturnTo, trace.openItem)
		}
		if len(trace.frames) > 1 {
			if closure.ResumedFrameID == nil ||
				*closure.ResumedFrameID != trace.frames[len(trace.frames)-2].ID {
				trace.fail(t, "closure did not resume the parent frame: %+v", closure)
			}
		} else if closure.ResumedWorkItemID != trace.openItem {
			trace.fail(t, "closure did not return to the original work item: %+v", closure)
		}
		trace.frames = trace.frames[:len(trace.frames)-1]
		return ""
	}
	closure, err := trace.harness.runtime.FailRecoveryFrame(
		ctx, trace.harness.actor, frame.ID, "declared terminal failure condition reached",
	)
	if err != nil {
		trace.fail(t, "fail recovery frame: %v", err)
	}
	if closure.Disposition != DispositionBlocked || closure.ReturnTo != trace.openItem {
		trace.fail(t, "terminal closure = %+v", closure)
	}
	trace.applyClosures([]FrameClosure{closure})
	return classTerminalFrameError
}

func (trace *model) completeWork(t *testing.T, ctx context.Context) failureClass {
	t.Helper()
	if trace.goalDone {
		return trace.continueUnder(t, ctx, CauseWorkerReplacement, "")
	}
	if len(trace.frames) > 0 {
		return ""
	}
	if trace.openItem != "" {
		missing := ""
		for _, criterion := range trace.itemCriteria(t, ctx) {
			if !trace.covered[criterion] {
				missing = criterion
				break
			}
		}
		if missing != "" {
			trace.recordCriterionEvidence(t, ctx, missing, ScopeCriterion)
			return ""
		}
		if _, err := trace.harness.runtime.CompleteWorkItem(
			ctx, trace.harness.actor, trace.openItem,
		); err != nil {
			trace.fail(t, "complete work item %s: %v", trace.openItem, err)
		}
		trace.completed[trace.openItem] = true
		trace.openItem = ""
		return ""
	}
	trace.completeGoalWhenCovered(t, ctx)
	return ""
}

func (trace *model) completeGoalWhenCovered(t *testing.T, ctx context.Context) {
	t.Helper()
	if trace.goalDone || len(trace.frames) > 0 || trace.openItem != "" {
		return
	}
	for _, criterion := range trace.contract.DoneCriteria {
		if !trace.covered[criterion.ID] {
			return
		}
	}
	contract, err := trace.harness.runtime.CompleteGoal(ctx, trace.harness.actor)
	if err != nil {
		trace.fail(t, "complete goal: %v", err)
	}
	if contract.Status != work.StatusCompleted {
		trace.fail(t, "completed contract = %+v", contract)
	}
	trace.goalDone = true
}

func (trace *model) recordCriterionEvidence(
	t *testing.T, ctx context.Context, criterion string, scope EvidenceScope,
) EvidenceRecord {
	t.Helper()
	trace.keys++
	name := fmt.Sprintf("evidence-%d-%d.txt", trace.seed, trace.keys)
	reference := trace.harness.writeFile(t, name, "verified "+name)
	record, err := trace.harness.runtime.VerifyEvidence(ctx, trace.harness.actor, EvidenceInput{
		WorkerID: trace.lease.WorkerID, FencingToken: trace.lease.FencingToken, Scope: scope,
		Kind: "report", Title: "evidence " + name, Reference: reference,
		Criteria: []string{criterion},
	})
	if err != nil {
		trace.fail(t, "verify evidence: %v", err)
	}
	if record.Verification != "server_sha256" || len(record.SHA256) != 64 {
		trace.fail(t, "evidence was not server verified: %+v", record)
	}
	if scope == ScopeCriterion {
		trace.covered[criterion] = true
	}
	return record
}

// assertInvariants checks every criterion 69.9 invariant after a transition.
func (trace *model) assertInvariants(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	actor := trace.harness.actor
	snapshot, err := trace.harness.runtime.Snapshot(ctx, actor)
	if err != nil {
		trace.fail(t, "snapshot: %v", err)
	}
	versions, err := trace.harness.runtime.ApprovedVersions(ctx, actor)
	if err != nil {
		trace.fail(t, "approved versions: %v", err)
	}
	latest := versions[len(versions)-1]
	if snapshot.Contract.Hash != latest.Hash || snapshot.Contract.Version != latest.Version ||
		latest.Hash != GoalHash(latest) {
		trace.fail(t, "active goal is not the latest explicit approval: %+v", snapshot.Contract)
	}
	frames, err := trace.harness.runtime.Frames(ctx, actor)
	if err != nil {
		trace.fail(t, "frames: %v", err)
	}
	active := activeOf(frames)
	if len(active) > MaxActiveFrames {
		trace.fail(t, "%d frames are simultaneously active", len(active))
	}
	for index, frame := range active {
		if frame.Depth != index+1 {
			trace.fail(t, "active frames are not an ordered stack: %+v", active)
		}
		if frame.ReturnTo != frame.OriginalWorkItemID ||
			frame.ReturnTo != snapshot.OpenWorkItemID {
			trace.fail(t, "frame return_to drifted from the original work item: %+v", frame)
		}
		if frame.Consumed.ToolInvocations > frame.Budget.ToolInvocations ||
			frame.Consumed.ModelTokens > frame.Budget.ModelTokens ||
			frame.Consumed.SubsequentErrors > frame.Budget.SubsequentErrors {
			trace.fail(t, "frame exceeded its budget: %+v", frame)
		}
	}
	actions, err := trace.harness.runtime.Actions(ctx, actor)
	if err != nil {
		trace.fail(t, "actions: %v", err)
	}
	known := map[uuid.UUID]struct{}{}
	for _, frame := range frames {
		known[frame.ID] = struct{}{}
	}
	for _, action := range actions {
		if (action.WorkItemID == "") == (action.FrameID == nil) {
			trace.fail(t, "action does not have exactly one parent: %+v", action)
		}
		if action.FrameID != nil {
			if _, ok := known[*action.FrameID]; !ok {
				trace.fail(t, "action parent frame is unknown: %+v", action)
			}
		}
		if !versionKnown(versions, action.GoalHash, action.GoalVersion) {
			trace.fail(t, "action is not bound to an approved goal version: %+v", action)
		}
	}
	portfolio, err := trace.harness.work.Get(ctx, actor)
	if err != nil {
		trace.fail(t, "portfolio: %v", err)
	}
	for _, contract := range portfolio.Contracts {
		if contract.ID != latest.ContractID || contract.Status != work.StatusCompleted {
			continue
		}
		covered := verifiedCriteria(t, trace, latest)
		for _, criterion := range latest.DoneCriteria {
			if !covered[criterion.ID] {
				trace.fail(
					t, "completed contract lacks verified evidence for %q", criterion.ID,
				)
			}
		}
	}
	replayRuntime, err := New(trace.harness.store, trace.harness.clock, trace.harness.work)
	if err != nil {
		trace.fail(t, "replay runtime: %v", err)
	}
	replayed, err := replayRuntime.Snapshot(ctx, actor)
	if err != nil {
		trace.fail(t, "replay snapshot: %v", err)
	}
	if !reflect.DeepEqual(snapshot, replayed) {
		trace.fail(t, "replay diverged:\n%+v\n%+v", snapshot, replayed)
	}
	if replayed.NextWorkItemID != snapshot.NextWorkItemID {
		trace.fail(
			t, "replay next work item = %q, want %q",
			replayed.NextWorkItemID, snapshot.NextWorkItemID,
		)
	}
}

func verifiedCriteria(t *testing.T, trace *model, contract GoalContract) map[string]bool {
	t.Helper()
	raw, err := trace.harness.store.LoadLivingState(
		context.Background(), stateKind, trace.harness.actor.String(),
	)
	if err != nil {
		trace.fail(t, "load durable state: %v", err)
	}
	var state State
	if err := unmarshalState(raw, &state); err != nil {
		trace.fail(t, "decode durable state: %v", err)
	}
	covered := map[string]bool{}
	for criterion := range coverage(state, contract) {
		covered[criterion] = true
	}
	return covered
}

func activeOf(frames []RecoveryFrame) []RecoveryFrame {
	active := make([]RecoveryFrame, 0, MaxActiveFrames)
	for _, frame := range frames {
		if frame.Status == FrameActive {
			active = append(active, frame)
		}
	}
	return active
}

func versionKnown(versions []GoalContract, hash string, version uint64) bool {
	for _, contract := range versions {
		if contract.Hash == hash && contract.Version == version {
			return true
		}
	}
	return false
}
