// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package turnstate

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"matrix/neo/internal/runtime/records"
	"matrix/neo/internal/sessionjournal"
	"matrix/vault"
)

func TestCanonicalStoreWritesOnlyCanonicalRecordsAndTransitionsFrozenMachine(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	defer store.Close(ctx)
	turn := canonicalTurn()
	if err := store.CreateTurnRecord(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionTurn(ctx, turn.LogicalTurnID, records.StatePreparing, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionTurn(ctx, turn.LogicalTurnID, records.StateDelivered, nil); err == nil {
		t.Fatal("illegal Preparing -> Delivered transition was accepted")
	}
	if err := store.SaveCycleRecord(ctx, turn.LogicalTurnID, records.CycleRecord{
		GenerationNumber: 1, ProviderRequest: json.RawMessage(`{"messages":[]}`),
	}); err != nil {
		t.Fatal(err)
	}
	manifest := records.ContextManifest{Entries: []records.ContextManifestEntry{{
		SourceNamespace: "transcript", SourceID: "7", SemanticKind: "transcript_user",
		ContentHash: "abc", Included: true, Reason: "included",
	}}}
	if err := store.SaveContextManifest(ctx, turn.LogicalTurnID, 1, manifest); err != nil {
		t.Fatal(err)
	}
	cycle, err := store.LoadCycleRecord(ctx, turn.LogicalTurnID, 1)
	if err != nil || len(cycle.ContextManifest.Entries) != 1 || string(cycle.ProviderRequest) != `{"messages":[]}` {
		t.Fatalf("persisted manifest lost cycle data: %#v err=%v", cycle, err)
	}
	if err := store.SaveConvergenceRecord(ctx, turn.LogicalTurnID, records.ConvergenceRecord{CumulativeInputTokens: 17}); err != nil {
		t.Fatal(err)
	}
	var legacyTurns, legacyEffects int
	if err := store.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn_state`).Scan(&legacyTurns); err != nil {
		t.Fatal(err)
	}
	if err := store.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM effect_state`).Scan(&legacyEffects); err != nil {
		t.Fatal(err)
	}
	if legacyTurns != 0 || legacyEffects != 0 {
		t.Fatalf("canonical write touched legacy stores: turns=%d effects=%d", legacyTurns, legacyEffects)
	}
	loaded, err := store.LoadTurnRecord(ctx, turn.LogicalTurnID)
	if err != nil || loaded.CurrentState != records.StatePreparing {
		t.Fatalf("LoadTurnRecord() = %+v, %v", loaded, err)
	}
}

func TestLegacyJournalCompatibilityReaderUsesRealEncryptedJournal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	session := bootTestVault(t, dir)
	journal, err := sessionjournal.Open(ctx, filepath.Join(dir, "legacy-journal.db"), session)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	conversationID := "conversation-legacy"
	turnID := "turn-legacy"
	for _, event := range []sessionjournal.Event{
		{ConversationID: conversationID, TurnID: turnID, Kind: sessionjournal.KindUserMessage, DisplayContent: "brief me", Message: &sessionjournal.Message{Role: sessionjournal.RoleUser}},
		{ConversationID: conversationID, TurnID: turnID, Kind: sessionjournal.KindToolCall, ToolCall: &sessionjournal.ToolCall{CallID: "call-1", Name: "web_search", Arguments: []byte(`{"q":"agents"}`)}},
		{ConversationID: conversationID, TurnID: turnID, Kind: sessionjournal.KindToolResult, DisplayContent: "result", ToolResult: &sessionjournal.ToolResult{CallID: "call-1", Name: "web_search", Result: []byte(`{"items":1}`)}},
		{ConversationID: conversationID, TurnID: turnID, Kind: sessionjournal.KindAssistantMessage, DisplayContent: "Here is the briefing.", Message: &sessionjournal.Message{Role: sessionjournal.RoleAssistant}},
	} {
		if _, err := journal.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := ReadLegacyJournal(ctx, journal, conversationID, turnID, "actor-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Turn.CurrentState != records.StateDelivered || len(snapshot.Answers) != 1 || len(snapshot.Effects) != 1 {
		t.Fatalf("legacy snapshot = %+v", snapshot)
	}
}

func TestResurrectionCompatibilityReaderPreservesTurnBudgetsDebtAndEffects(t *testing.T) {
	state := baseTurnState()
	state.Checkpoint = &Checkpoint{
		Step: 4, ProviderAttempts: 3,
		ToolEvents: []json.RawMessage{json.RawMessage(`{"call_id":"call-1"}`)},
		SavedAt:    state.UpdatedAt,
	}
	snapshot, err := ImportResurrection(state, map[string]EffectRecord{
		"effect-1": {
			IdempotencyKey: "effect-1", ToolName: "fs__read_file", RetrySafe: true,
			Status: EffectStarted, StartedAt: state.UpdatedAt,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Turn.CurrentState != records.StateSynthesisOwed ||
		!snapshot.Turn.SynthesisDebt.Owed || snapshot.Turn.CumulativeBudgets.ProviderCalls != 3 ||
		len(snapshot.Effects) != 1 {
		t.Fatalf("resurrection snapshot = %+v", snapshot)
	}
}

func TestKill9CanonicalResumePreservesLogicalTurnBudgetsDebtAndEffects(t *testing.T) {
	if os.Getenv("TURNSTATE_CANONICAL_KILL_HELPER") == "1" {
		runCanonicalKillHelper()
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "turnstate.db")
	command := exec.Command(os.Args[0], "-test.run=TestKill9CanonicalResumePreservesLogicalTurnBudgetsDebtAndEffects")
	command.Env = append(os.Environ(),
		"TURNSTATE_CANONICAL_KILL_HELPER=1",
		"TURNSTATE_CANONICAL_KILL_DIR="+dir,
		"TURNSTATE_CANONICAL_KILL_PATH="+path,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "canonical-committed" {
		_ = command.Process.Kill()
		t.Fatalf("helper ready = %q, %v", line, err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	ctx := context.Background()
	store, err := Open(ctx, path, bootTestVault(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(ctx)
	turn, err := store.LoadTurnRecord(ctx, "canonical-kill-turn")
	if err != nil {
		t.Fatal(err)
	}
	convergence, err := store.LoadConvergenceRecord(ctx, turn.LogicalTurnID)
	if err != nil {
		t.Fatal(err)
	}
	effect, err := store.LoadEffectRecord(ctx, turn.LogicalTurnID, "effect-1")
	if err != nil {
		t.Fatal(err)
	}
	if turn.CurrentState != records.StateSynthesisOwed || !turn.SynthesisDebt.Owed ||
		convergence.CumulativeInputTokens != 991 || effect.EffectState != records.EffectStarted {
		t.Fatalf("resumed turn=%+v convergence=%+v effect=%+v", turn, convergence, effect)
	}
}

func runCanonicalKillHelper() {
	dir := os.Getenv("TURNSTATE_CANONICAL_KILL_DIR")
	path := os.Getenv("TURNSTATE_CANONICAL_KILL_PATH")
	ctx := context.Background()
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: dir, UserDID: testUser,
		KEKHex: hex.EncodeToString(bytesOf(0x42, 32)),
	})
	if err != nil {
		os.Exit(2)
	}
	store, err := Open(ctx, path, session)
	if err != nil {
		os.Exit(3)
	}
	turn := canonicalTurn()
	turn.LogicalTurnID = "canonical-kill-turn"
	if err := store.CreateTurnRecord(ctx, turn); err != nil {
		os.Exit(4)
	}
	for _, next := range []records.TurnState{
		records.StatePreparing, records.StateGenerating, records.StateAwaitingTools,
		records.StateExecutingTools, records.StateSynthesisOwed,
	} {
		var mutate func(*records.TurnRecord) error
		if next == records.StateSynthesisOwed {
			mutate = func(current *records.TurnRecord) error {
				current.SynthesisDebt = records.SynthesisDebt{Owed: true, UnconsumedEvidence: []string{"effect-1:result"}}
				return nil
			}
		}
		if err := store.TransitionTurn(ctx, turn.LogicalTurnID, next, mutate); err != nil {
			os.Exit(4)
		}
	}
	if err := store.SaveConvergenceRecord(ctx, turn.LogicalTurnID, records.ConvergenceRecord{CumulativeInputTokens: 991}); err != nil {
		os.Exit(5)
	}
	if err := store.SaveEffectRecord(ctx, turn.LogicalTurnID, "effect-1", records.EffectRecord{
		Operation: "service_call", NormalizedArguments: json.RawMessage(`{}`),
		SideEffectClass:     records.SideEffectNonIdempotentReconciliable,
		IdempotencyStrategy: "effect-1", ReconciliationStrategy: "authoritative-state",
		EffectState: records.EffectStarted,
	}); err != nil {
		os.Exit(6)
	}
	_, _ = os.Stdout.WriteString("canonical-committed\n")
	select {}
}

func canonicalTurn() records.TurnRecord {
	return records.TurnRecord{
		LogicalTurnID: "turn-canonical", ConversationID: "conversation-canonical",
		RequestIdentity: "request-canonical", Objective: "complete the task",
		LatestGenuineMessageID: "message-canonical", CurrentState: records.StateAccepted,
	}
}
