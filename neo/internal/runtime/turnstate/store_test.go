// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package turnstate

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"matrix/neo/internal/runtime/protocol"
	"matrix/vault"
)

const (
	testUser    = "did:matrix:turnstate-test"
	testTurnID  = "turn-0001"
	testActorID = "actor-0001"
	testSession = "session-0001"
)

func TestStoreMigratesSealsAndRecoversTypedState(t *testing.T) {
	ctx := context.Background()
	store, path := openTestStore(t)
	defer store.Close(ctx)
	if version, err := store.SchemaVersion(ctx); err != nil || version != 5 {
		t.Fatalf("SchemaVersion() = %d, %v", version, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %o", info.Mode().Perm())
	}
	if err := store.CreateTurnState(ctx, baseTurnState()); err != nil {
		t.Fatal(err)
	}
	checkpoint := Checkpoint{
		Messages: []protocol.Message{
			{Role: protocol.RoleUser, Content: "prepare the report"},
			{Role: protocol.RoleAssistant, Content: "working"},
		},
		ToolEvents: []json.RawMessage{json.RawMessage(`{"tool":"search","ok":true}`)},
		Step:       2, ProviderAttempts: 3,
		Plan: json.RawMessage(`{"goal":"report"}`),
		PendingCall: &PendingCall{
			CallID: "call-1", IdempotencyKey: "idem-1",
			ToolName: "web_search", Arguments: json.RawMessage(`{"query":"Matrix"}`),
			DispatchedAt: time.Now().UTC(),
		},
	}
	if err := store.SaveTurnCheckpoint(ctx, testTurnID, checkpoint); err != nil {
		t.Fatal(err)
	}
	recovery := Recovery{
		Phase: "provider", StartedAt: time.Now().UTC(),
		AttemptCount: 3, Advice: "resume_from_checkpoint",
		LastToolEvidence: json.RawMessage(`{"call_id":"call-1"}`),
	}
	if err := store.SetTurnRecovery(
		ctx, testTurnID, StatusIncomplete, recovery,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCognitionState(
		ctx, testSession, json.RawMessage(`{"premises":["verified"]}`),
	); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadTurnState(ctx, testTurnID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatusIncomplete ||
		loaded.Checkpoint == nil || loaded.Checkpoint.Step != 2 ||
		loaded.Checkpoint.PendingCall == nil ||
		loaded.Checkpoint.PendingCall.IdempotencyKey != "idem-1" ||
		loaded.Recovery == nil ||
		loaded.Recovery.Advice != "resume_from_checkpoint" {
		t.Fatalf("loaded = %+v", loaded)
	}
	cognition, err := store.LoadCognitionState(ctx, testSession)
	if err != nil || string(cognition) != `{"premises":["verified"]}` {
		t.Fatalf("cognition = %s, %v", cognition, err)
	}
	recoverable, err := store.RecoverableTurnStates(ctx)
	if err != nil || len(recoverable) != 1 ||
		recoverable[0].TurnID != testTurnID {
		t.Fatalf("recoverable = %+v, %v", recoverable, err)
	}
}

func TestStoreRejectsProviderTextAsFailureCode(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	defer store.Close(ctx)
	if err := store.CreateTurnState(ctx, baseTurnState()); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTurnFailure(
		ctx, testTurnID, StatusFailed,
		"provider said your secret token is invalid",
	); err == nil {
		t.Fatal("provider text was accepted as a terminal cause")
	}
	if err := store.SetTurnFailure(
		ctx, testTurnID, StatusFailed, "provider_exhausted",
	); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadTurnState(ctx, testTurnID)
	if err != nil || loaded.FailureCode != "provider_exhausted" ||
		loaded.Status != StatusFailed {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
}

func TestStoreSerializesConcurrentCheckpointWrites(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	defer store.Close(ctx)
	if err := store.CreateTurnState(ctx, baseTurnState()); err != nil {
		t.Fatal(err)
	}
	const writers = 24
	var group sync.WaitGroup
	errs := make(chan error, writers)
	for step := 0; step < writers; step++ {
		group.Add(1)
		go func(step int) {
			defer group.Done()
			errs <- store.SaveTurnCheckpoint(ctx, testTurnID, Checkpoint{
				Messages: []protocol.Message{{
					Role: protocol.RoleUser, Content: "turn",
				}},
				Step: step, SavedAt: time.Now().UTC(),
			})
		}(step)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := store.LoadTurnState(ctx, testTurnID)
	if err != nil || loaded.Checkpoint == nil ||
		loaded.Checkpoint.Step < 0 || loaded.Checkpoint.Step >= writers {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
}

func TestSavePendingEffectCommitsCheckpointAndEffectAtomically(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	defer store.Close(ctx)
	if err := store.CreateTurnState(ctx, baseTurnState()); err != nil {
		t.Fatal(err)
	}
	baseline := Checkpoint{
		Messages: []protocol.Message{{Role: protocol.RoleUser, Content: "turn"}},
		Step:     1,
	}
	if err := store.SaveTurnCheckpoint(ctx, testTurnID, baseline); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginEffect(
		ctx, "occupied", "fs__read_file", json.RawMessage(`{"path":"a"}`), true,
	); err != nil {
		t.Fatal(err)
	}
	conflicting := baseline
	conflicting.Step = 2
	conflicting.PendingCall = &PendingCall{
		CallID: "call-conflict", IdempotencyKey: "occupied",
		ToolName: "fs__write_file", Arguments: json.RawMessage(`{"path":"b"}`),
		DispatchedAt: time.Now().UTC(),
	}
	if err := store.SavePendingEffect(
		ctx, testTurnID, conflicting, "occupied", "fs__write_file",
		conflicting.PendingCall.Arguments, false,
	); err == nil {
		t.Fatal("idempotency conflict unexpectedly committed")
	}
	loaded, err := store.LoadTurnState(ctx, testTurnID)
	if err != nil || loaded.Checkpoint == nil || loaded.Checkpoint.Step != 1 ||
		loaded.Checkpoint.PendingCall != nil {
		t.Fatalf("failed transaction changed checkpoint: state=%+v err=%v", loaded, err)
	}

	pending := baseline
	pending.Step = 2
	pending.PendingCall = &PendingCall{
		CallID: "call-read", IdempotencyKey: "atomic-read",
		ToolName: "fs__read_file", Arguments: json.RawMessage(`{"path":"a"}`),
		DispatchedAt: time.Now().UTC(),
	}
	if err := store.SavePendingEffect(
		ctx, testTurnID, pending, "atomic-read", "fs__read_file",
		pending.PendingCall.Arguments, true,
	); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.LoadTurnState(ctx, testTurnID)
	if err != nil || loaded.Checkpoint == nil ||
		loaded.Checkpoint.PendingCall == nil ||
		loaded.Checkpoint.PendingCall.IdempotencyKey != "atomic-read" {
		t.Fatalf("pending checkpoint missing after commit: state=%+v err=%v", loaded, err)
	}
	effect, err := store.LoadEffect(ctx, "atomic-read")
	if err != nil || effect.Status != EffectStarted || !effect.RetrySafe {
		t.Fatalf("effect missing after commit: effect=%+v err=%v", effect, err)
	}
}

func TestStoreTamperAndTruncationFailClosed(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func([]byte) []byte
		want error
	}{
		{
			name: "tamper",
			edit: func(envelope []byte) []byte {
				envelope[len(envelope)-1] ^= 0xff
				return envelope
			},
			want: vault.ErrAuth,
		},
		{
			name: "truncate",
			edit: func(envelope []byte) []byte {
				return envelope[:5]
			},
			want: vault.ErrTruncated,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, path := openTestStore(t)
			if err := store.CreateTurnState(ctx, baseTurnState()); err != nil {
				t.Fatal(err)
			}
			session := store.vault
			if err := store.Close(ctx); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			var envelope []byte
			if err := db.QueryRow(
				`SELECT state FROM canonical_records
				 WHERE logical_turn_id = ? AND record_type = 'turn' AND record_key = 'current'`, testTurnID,
			).Scan(&envelope); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(
				`UPDATE canonical_records SET state = ?
				 WHERE logical_turn_id = ? AND record_type = 'turn' AND record_key = 'current'`,
				test.edit(envelope), testTurnID,
			); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, path, session)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(ctx)
			if _, err := reopened.LoadTurnState(ctx, testTurnID); !errors.Is(err, test.want) {
				t.Fatalf("LoadTurnState() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestKill9BetweenCheckpointWritesPreservesLatestCommit(t *testing.T) {
	if os.Getenv("TURNSTATE_KILL_HELPER") == "1" {
		runKillHelper()
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "turnstate.db")
	command := exec.Command(
		os.Args[0], "-test.run=TestKill9BetweenCheckpointWritesPreservesLatestCommit",
	)
	command.Env = append(os.Environ(),
		"TURNSTATE_KILL_HELPER=1",
		"TURNSTATE_KILL_DIR="+dir,
		"TURNSTATE_KILL_PATH="+path,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "checkpoint-committed" {
		_ = command.Process.Kill()
		t.Fatalf("helper ready = %q, %v", line, err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	ctx := context.Background()
	session := bootTestVault(t, dir)
	store, err := Open(ctx, path, session)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(ctx)
	loaded, err := store.LoadTurnState(ctx, testTurnID)
	if err != nil || loaded.Checkpoint == nil ||
		loaded.Checkpoint.Step != 1 {
		t.Fatalf("latest committed checkpoint = %+v, %v", loaded.Checkpoint, err)
	}
}

func runKillHelper() {
	dir := os.Getenv("TURNSTATE_KILL_DIR")
	path := os.Getenv("TURNSTATE_KILL_PATH")
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
	if err := store.CreateTurnState(ctx, baseTurnState()); err != nil {
		os.Exit(4)
	}
	if err := store.SaveTurnCheckpoint(ctx, testTurnID, Checkpoint{
		Messages: []protocol.Message{{
			Role: protocol.RoleUser, Content: "turn",
		}},
		Step: 1,
	}); err != nil {
		os.Exit(5)
	}
	_, _ = os.Stdout.WriteString("checkpoint-committed\n")
	select {}
}

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "turnstate.db")
	store, err := Open(context.Background(), path, bootTestVault(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	return store, path
}

func bootTestVault(t *testing.T, dir string) *vault.Session {
	t.Helper()
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: dir, UserDID: testUser,
		KEKHex: hex.EncodeToString(bytesOf(0x42, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func bytesOf(value byte, count int) []byte {
	result := make([]byte, count)
	for index := range result {
		result[index] = value
	}
	return result
}

func baseTurnState() TurnState {
	return TurnState{
		TurnID: testTurnID, ActorID: testActorID,
		SessionID: testSession, Content: "prepare the report",
		Status: StatusRunning, UpdatedAt: time.Now().UTC(),
	}
}
