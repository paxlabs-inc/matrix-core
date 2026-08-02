// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package sessionjournal

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"matrix/cortex"
	cortexstore "matrix/cortex/store"
	"matrix/vault"
)

const (
	testConversation = "conv-journal-1"
	testUser         = "did:matrix:session-journal-test"
	testKEKByte      = 0x63
)

func TestStoreAppendReadEveryEventType(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	defer store.Close()
	events := everyEventType()
	for index := range events {
		appended, err := store.Append(ctx, events[index])
		if err != nil {
			t.Fatalf("Append[%d]: %v", index, err)
		}
		if appended.Sequence != int64(index+1) || appended.CreatedAt.IsZero() {
			t.Fatalf("Append[%d] envelope = %+v", index, appended)
		}
	}
	got, err := store.Read(ctx, testConversation, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(events) {
		t.Fatalf("Read len = %d, want %d", len(got), len(events))
	}
	for index := range got {
		if got[index].Kind != events[index].Kind {
			t.Fatalf("event[%d] kind = %q, want %q", index, got[index].Kind, events[index].Kind)
		}
		if got[index].DisplayContent != events[index].DisplayContent || !bytes.Equal(got[index].APIContent, events[index].APIContent) {
			t.Fatalf("event[%d] display/API content drifted", index)
		}
		if err := got[index].Validate(); err != nil {
			t.Fatalf("event[%d] invalid after read: %v", index, err)
		}
	}
	bounded, err := store.Read(ctx, testConversation, 3, 2)
	if err != nil || len(bounded) != 2 || bounded[0].Sequence != 3 || bounded[1].Sequence != 4 {
		t.Fatalf("bounded read = %+v, %v", bounded, err)
	}
}

func TestStoreSealsEventPayload(t *testing.T) {
	ctx := context.Background()
	store, path := openTestStore(t)
	secret := "provider-visible-secret-sentinel"
	if _, err := store.Append(ctx, Event{
		ConversationID: testConversation,
		Kind:           KindUserMessage,
		DisplayContent: "clean",
		APIContent:     []byte(secret),
		Message:        &Message{Role: RoleUser},
	}); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := store.db.QueryRowContext(ctx,
		`SELECT event FROM journal_event WHERE conversation_id = ? AND sequence = 1`,
		testConversation,
	).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !vault.IsVault(payload) || bytes.Contains(payload, []byte(secret)) {
		t.Fatalf("journal payload was not sealed: %q", payload)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %v, %v", info.Mode().Perm(), err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path, bootTestVault(t, filepath.Dir(path)))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	events, err := reopened.Read(ctx, testConversation, 1, 0)
	if err != nil || len(events) != 1 || string(events[0].APIContent) != secret {
		t.Fatalf("sealed reopen = %+v, %v", events, err)
	}
}

func TestStoreFinalizesAbnormalTails(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	defer store.Close()
	call, err := store.Append(ctx, Event{
		ConversationID: testConversation, TurnID: "turn-1",
		Kind:     KindToolCall,
		ToolCall: &ToolCall{CallID: "call-pending", Name: "write_file", StateChanging: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	tail, err := store.Tail(ctx, testConversation)
	if err != nil || tail.Condition != TailPendingToolCall || tail.Event.Sequence != call.Sequence {
		t.Fatalf("pending tail = %+v, %v", tail, err)
	}
	finalized, changed, err := store.FinalizeTail(ctx, testConversation, "resume after interrupted effect")
	if err != nil || !changed || finalized.Kind != KindRecovery || finalized.Recovery.FinalizesSequence != call.Sequence || finalized.Recovery.PendingCallID != "call-pending" {
		t.Fatalf("finalization = %+v, changed=%v err=%v", finalized, changed, err)
	}
	tail, err = store.Tail(ctx, testConversation)
	if err != nil || tail.Abnormal() {
		t.Fatalf("tail after finalization = %+v, %v", tail, err)
	}

	partial, err := store.Append(ctx, Event{
		ConversationID: testConversation, TurnID: "turn-2",
		Kind: KindAssistantMessage, DisplayContent: "partial",
		Message: &Message{Role: RoleAssistant, Partial: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	tail, err = store.Tail(ctx, testConversation)
	if err != nil || tail.Condition != TailPartialAssistant || tail.Event.Sequence != partial.Sequence {
		t.Fatalf("partial tail = %+v, %v", tail, err)
	}
	if _, changed, err = store.FinalizeTail(ctx, testConversation, "resume partial response"); err != nil || !changed {
		t.Fatalf("partial finalization changed=%v err=%v", changed, err)
	}
}

func TestStoreProcessKillRollsBackMidAppend(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "session-journal.db")
	store, err := Open(ctx, path, bootTestVault(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, Event{
		ConversationID: testConversation, Kind: KindUserMessage,
		DisplayContent: "committed", Message: &Message{Role: RoleUser},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(dir, "child-ready")
	command := exec.Command(os.Args[0], "-test.run=^TestJournalCrashChild$")
	command.Env = append(os.Environ(),
		"SESSION_JOURNAL_CRASH_CHILD=1",
		"SESSION_JOURNAL_CRASH_DB="+path,
		"SESSION_JOURNAL_CRASH_DIR="+dir,
		"SESSION_JOURNAL_CRASH_READY="+ready,
		"SESSION_JOURNAL_CRASH_KEK="+hex.EncodeToString(bytes.Repeat([]byte{testKEKByte}, 32)),
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatal("crash child did not reach uncommitted append")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	reopened, err := Open(ctx, path, bootTestVault(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	events, err := reopened.Read(ctx, testConversation, 1, 0)
	if err != nil || len(events) != 1 || events[0].DisplayContent != "committed" {
		t.Fatalf("post-kill journal = %+v, %v", events, err)
	}
	next, err := reopened.Append(ctx, Event{
		ConversationID: testConversation, Kind: KindAssistantMessage,
		DisplayContent: "after-recovery", Message: &Message{Role: RoleAssistant},
	})
	if err != nil || next.Sequence != 2 {
		t.Fatalf("post-kill append = %+v, %v", next, err)
	}
}

func TestJournalCrashChild(t *testing.T) {
	if os.Getenv("SESSION_JOURNAL_CRASH_CHILD") != "1" {
		return
	}
	ctx := context.Background()
	kek, err := hex.DecodeString(os.Getenv("SESSION_JOURNAL_CRASH_KEK"))
	if err != nil || len(kek) != 32 {
		os.Exit(2)
	}
	session, err := vault.Boot(ctx, vault.Config{
		Required: true,
		DataDir:  os.Getenv("SESSION_JOURNAL_CRASH_DIR"),
		UserDID:  testUser,
		KEKHex:   hex.EncodeToString(kek),
	})
	if err != nil {
		os.Exit(3)
	}
	store, err := Open(ctx, os.Getenv("SESSION_JOURNAL_CRASH_DB"), session)
	if err != nil {
		os.Exit(4)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		os.Exit(5)
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO journal_sequence(conversation_id, next_sequence)
		 VALUES (?, 2)
		 ON CONFLICT(conversation_id) DO UPDATE
		 SET next_sequence = journal_sequence.next_sequence + 1
		 RETURNING next_sequence - 1`, testConversation,
	).Scan(&sequence); err != nil {
		os.Exit(6)
	}
	event := Event{
		ConversationID: testConversation, Sequence: sequence,
		Kind: KindAssistantMessage, CreatedAt: time.Now().UTC(),
		DisplayContent: "must-not-commit", Message: &Message{Role: RoleAssistant},
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		os.Exit(7)
	}
	sealed, err := store.seal(testConversation, sequence, encoded)
	if err != nil {
		os.Exit(8)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO journal_event(conversation_id, sequence, turn_id, kind, created_at, event)
		 VALUES (?, ?, '', ?, ?, ?)`, testConversation, sequence,
		string(event.Kind), event.CreatedAt.UnixMicro(), sealed,
	); err != nil {
		os.Exit(9)
	}
	if err := os.WriteFile(os.Getenv("SESSION_JOURNAL_CRASH_READY"), []byte("ready"), 0o600); err != nil {
		os.Exit(10)
	}
	select {}
}

func TestStoreDoesNotCoupleJournalAndCortexWrites(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cortexDisk, err := cortexstore.Open(filepath.Join(root, "cortex"), "journal-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cortexDisk.Close()
	memory := cortex.New(cortexDisk)
	journal, err := Open(ctx, filepath.Join(root, "journal", "events.db"), bootTestVault(t, filepath.Join(root, "vault")))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := memory.AppendMessage(cortex.Message{
		ConversationID: testConversation, Role: cortex.RoleUser, Content: "before journal",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(ctx, Event{
		ConversationID: testConversation, Kind: KindUserMessage,
		DisplayContent: "journal event", Message: &Message{Role: RoleUser},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.AppendMessage(cortex.Message{
		ConversationID: testConversation, Role: cortex.RoleAssistant, Content: "after journal",
	}); err != nil {
		t.Fatal(err)
	}
	transcript, err := memory.Transcript(testConversation, 0, 0)
	if err != nil || len(transcript) != 2 || transcript[0].Content != "before journal" || transcript[1].Content != "after journal" {
		t.Fatalf("cortex transcript = %+v, %v", transcript, err)
	}
	events, err := journal.Read(ctx, testConversation, 1, 0)
	if err != nil || len(events) != 1 || events[0].DisplayContent != "journal event" {
		t.Fatalf("journal events = %+v, %v", events, err)
	}
}

func everyEventType() []Event {
	base := func(kind Kind, display string) Event {
		return Event{
			ConversationID: testConversation, TurnID: "turn-1", Attempt: 1,
			Kind: kind, DisplayContent: display,
			APIContent: []byte("api:" + display),
		}
	}
	events := []Event{
		base(KindUserMessage, "user"),
		base(KindAssistantMessage, "assistant"),
		base(KindToolCall, "call"),
		base(KindToolResult, "result"),
		base(KindReasoning, "reasoning"),
		base(KindProviderReplay, "replay"),
		base(KindApproval, "approval"),
		base(KindArtifact, "artifact"),
		base(KindUncertainEffect, "effect"),
		base(KindSupervisor, "supervisor"),
		base(KindRecovery, "recovery"),
	}
	events[0].Message = &Message{Role: RoleUser, ProviderMetadata: []byte(`{"source":"voice"}`)}
	events[1].Message = &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{CallID: "call-inline", Name: "search"}}}
	events[2].ToolCall = &ToolCall{CallID: "call-1", Type: "function", Name: "write_file", Arguments: []byte(`{"path":"a"}`), StateChanging: true, IdempotencyKey: "idem-1"}
	events[3].ToolResult = &ToolResult{CallID: "call-1", Name: "write_file", Result: []byte(`{"ok":true}`), ProviderMetadata: []byte(`{"latency_ms":2}`)}
	events[4].Reasoning = &Reasoning{Text: "private reasoning", ProviderMetadata: []byte(`{"signature":"sig"}`)}
	events[5].ProviderReplay = &ProviderReplay{Provider: "openai", Model: "model", RequestID: "req-1", RequestBytes: []byte(`{"messages":[]}`), ResponseBytes: []byte(`{"id":"resp-1"}`)}
	events[6].Approval = &Approval{ApprovalID: "approval-1", Action: "deploy", Decision: "approved", Authority: "user"}
	events[7].Artifact = &Artifact{ArtifactID: "artifact-1", Kind: "file", Path: "/workspace/result.txt", Digest: "sha256:abc"}
	events[8].UncertainEffect = &UncertainEffect{CallID: "call-2", IdempotencyKey: "idem-2", ToolName: "deploy", Status: "unknown", Evidence: []byte(`{"timeout":true}`)}
	events[9].Supervisor = &Supervisor{IntentID: "intent-1", Attempt: 2, Action: "respawn", Reason: "provider unavailable"}
	events[10].Recovery = &Recovery{State: "resumable", Reason: "provider interrupted", Checkpoint: []byte(`{"step":3}`)}
	return events
}

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session-journal.db")
	store, err := Open(context.Background(), path, bootTestVault(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	return store, path
}

func bootTestVault(t *testing.T, dir string) *vault.Session {
	t.Helper()
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true,
		DataDir:  dir,
		UserDID:  testUser,
		KEKHex:   hex.EncodeToString(bytes.Repeat([]byte{testKEKByte}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}
