package continuity

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Sidiora-Labs/centra-llm-agents/ion/internal/security/vault"
	"github.com/Sidiora-Labs/centra-llm-agents/ion/internal/session"
	"github.com/Sidiora-Labs/centra-llm-agents/ion/internal/work"
	"github.com/google/uuid"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
}

func (clock *testClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *testClock) advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

type harness struct {
	runtime   *Runtime
	work      *work.Service
	store     *session.Store
	clock     *testClock
	workspace string
	database  string
	actor     uuid.UUID
	closed    bool
}

func openHarness(t *testing.T) *harness {
	t.Helper()
	directory := t.TempDir()
	clock := newTestClock()
	database := filepath.Join(directory, "sessions.db")
	store, workService, runtime := openStack(t, database, directory, clock)
	instance := &harness{
		runtime: runtime, work: workService, store: store, clock: clock,
		workspace: directory, database: database, actor: uuid.New(),
	}
	t.Cleanup(func() {
		if !instance.closed {
			_ = instance.store.Close(context.Background())
			instance.closed = true
		}
	})
	return instance
}

// openDetachedHarness opens one production stack whose lifetime a caller closes
// explicitly, while cleanup still releases it if a trace fails early.
func openDetachedHarness(t *testing.T) *harness {
	t.Helper()
	return openHarness(t)
}

func (instance *harness) close(t *testing.T) {
	t.Helper()
	if instance.closed {
		return
	}
	if err := instance.store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	instance.closed = true
}

func unmarshalState(raw json.RawMessage, state *State) error {
	return json.Unmarshal(raw, state)
}

func openStack(
	t *testing.T, database, workspace string, clock *testClock,
) (*session.Store, *work.Service, *Runtime) {
	t.Helper()
	cipher, err := vault.New(bytes.Repeat([]byte{0x51}, vault.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.Open(context.Background(), database, cipher, clock, 8192)
	if err != nil {
		t.Fatal(err)
	}
	workService, err := work.NewService(store, clock, workspace)
	if err != nil {
		_ = store.Close(context.Background())
		t.Fatal(err)
	}
	runtime, err := New(store, clock, workService)
	if err != nil {
		_ = store.Close(context.Background())
		t.Fatal(err)
	}
	return store, workService, runtime
}

// restart closes the durable store and rebuilds the production composition
// over the same encrypted database file.
func (instance *harness) restart(t *testing.T) {
	t.Helper()
	if err := instance.store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	store, workService, runtime := openStack(t, instance.database, instance.workspace, instance.clock)
	instance.store, instance.work, instance.runtime = store, workService, runtime
}

func standardProposal(contractID uuid.UUID, goal string) ProposalInput {
	return ProposalInput{
		ContractID: contractID, Origin: "user", Rationale: "explicit delegation",
		Goal: goal, Deliverable: "verified report and passing checks",
		Constraints:  []string{"no destructive operations"},
		Verification: []string{"targeted test", "server digest"},
		DoneCriteria: []Criterion{
			{ID: "analysis", Description: "analysis artifact exists"},
			{ID: "checks", Description: "checks pass"},
		},
		Plan: []PlanNode{
			{ID: "analyze", Title: "Analyze the system", Criteria: []string{"analysis"}},
			{
				ID: "verify", Title: "Verify the change", Criteria: []string{"checks"},
				DependsOn: []string{"analyze"},
			},
		},
		NextAction: "analyze the system",
	}
}

func (instance *harness) approve(t *testing.T, input ProposalInput) GoalContract {
	t.Helper()
	proposal, err := instance.runtime.ProposeGoalRevision(context.Background(), instance.actor, input)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := instance.runtime.ApproveGoalRevision(
		context.Background(), instance.actor, proposal.ID, "operator",
	)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func (instance *harness) acquire(t *testing.T, workerID string) WorkerLease {
	t.Helper()
	lease, err := instance.runtime.AcquireWorker(
		context.Background(), instance.actor, workerID, time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func (instance *harness) open(t *testing.T, lease WorkerLease, itemID string) {
	t.Helper()
	if _, err := instance.runtime.OpenWorkItem(
		context.Background(), instance.actor, lease.WorkerID, lease.FencingToken, itemID,
	); err != nil {
		t.Fatal(err)
	}
}

func (instance *harness) writeFile(t *testing.T, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(instance.workspace, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func (instance *harness) evidence(
	t *testing.T, lease WorkerLease, scope EvidenceScope, criteria []string, name string,
) EvidenceRecord {
	t.Helper()
	reference := instance.writeFile(t, name, "verified "+name)
	record, err := instance.runtime.VerifyEvidence(context.Background(), instance.actor, EvidenceInput{
		WorkerID: lease.WorkerID, FencingToken: lease.FencingToken, Scope: scope,
		Kind: "report", Title: "evidence " + name, Reference: reference, Criteria: criteria,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (instance *harness) envelope(
	contract GoalContract, lease WorkerLease, itemID string, criteria []string, key string,
) ActionEnvelope {
	return ActionEnvelope{
		GoalHash: contract.Hash, GoalVersion: contract.Version, WorkItemID: itemID,
		Expected: EvidenceDelta{
			Description: "observable change for " + itemID, Criteria: criteria,
		},
		WorkerID: lease.WorkerID, FencingToken: lease.FencingToken, Kind: ActionTool,
		OperationID: "operation-" + key, IdempotencyKey: key,
		Cost: ActionCost{ToolInvocations: 1},
	}
}

func (instance *harness) frameEnvelope(
	contract GoalContract,
	lease WorkerLease,
	frame RecoveryFrame,
	strategy Strategy,
	key string,
	cost ActionCost,
) ActionEnvelope {
	return ActionEnvelope{
		GoalHash: contract.Hash, GoalVersion: contract.Version, FrameID: frame.ID,
		Expected: EvidenceDelta{
			Description: "recovery evidence delta", Criteria: frame.Exit.Criteria,
		},
		WorkerID: lease.WorkerID, FencingToken: lease.FencingToken, Kind: ActionTool,
		Strategy: strategy, OperationID: "operation-" + key, IdempotencyKey: key, Cost: cost,
	}
}

func (instance *harness) frameInput(
	contract GoalContract, lease WorkerLease, itemID string, criteria []string, budget FrameBudget,
) OpenFrameInput {
	return OpenFrameInput{
		WorkerID: lease.WorkerID, FencingToken: lease.FencingToken,
		GoalHash: contract.Hash, GoalVersion: contract.Version, WorkItemID: itemID,
		Cause: "tool error interrupted the open work item",
		Exit: ExitCondition{
			Description: "criterion evidence exists again", Criteria: criteria,
		},
		Disposition: DispositionBlocked,
		Allowlist:   []Strategy{StrategyRetryWithBackoff, StrategyAlternateTool},
		Budget:      budget,
	}
}

func (instance *harness) rawDurableState(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := instance.store.LoadLivingState(
		context.Background(), stateKind, instance.actor.String(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
