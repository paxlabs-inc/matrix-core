// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package turnstate

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"centra/agents/neo/internal/runtime/protocol"
	"centra/packages/vault"
)

func TestEffectDispatchFourRealKill9Boundaries(t *testing.T) {
	if os.Getenv("EFFECT_KILL_HELPER") == "1" {
		runEffectKillHelper()
		return
	}
	for _, stage := range []string{"before_dispatch", "after_atomic_commit", "during_effect", "after_completion"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "turnstate.db")
			effectPath := filepath.Join(root, "external-effect")
			command := exec.Command(os.Args[0], "-test.run=TestEffectDispatchFourRealKill9Boundaries")
			command.Env = append(os.Environ(),
				"EFFECT_KILL_HELPER=1",
				"EFFECT_KILL_STAGE="+stage,
				"EFFECT_KILL_ROOT="+root,
				"EFFECT_KILL_PATH="+path,
				"EFFECT_KILL_EXTERNAL="+effectPath,
			)
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || strings.TrimSpace(line) != stage {
				_ = command.Process.Kill()
				t.Fatalf("helper boundary=%q err=%v", line, err)
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = command.Wait()

			store, err := Open(context.Background(), path, effectKillVault(root))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close(context.Background()) })
			state, err := store.LoadTurnState(context.Background(), "effect-kill-turn")
			if err != nil || state.Checkpoint == nil {
				t.Fatalf("load killed state=%+v err=%v", state, err)
			}
			record, effectErr := store.LoadEffect(context.Background(), "effect-kill-key")
			switch stage {
			case "before_dispatch":
				if state.Checkpoint.PendingCall != nil || !errors.Is(effectErr, sql.ErrNoRows) {
					t.Fatalf("pre-dispatch kill left pending/effect: checkpoint=%+v effect=%+v err=%v", state.Checkpoint, record, effectErr)
				}
			case "after_atomic_commit", "during_effect":
				if effectErr != nil || record.Status != EffectStarted || state.Checkpoint.PendingCall == nil {
					t.Fatalf("started boundary lost atomic state: checkpoint=%+v effect=%+v err=%v", state.Checkpoint, record, effectErr)
				}
			case "after_completion":
				if effectErr != nil || record.Status != EffectCompleted || record.Result == nil || state.Checkpoint.PendingCall == nil {
					t.Fatalf("completed boundary lost result: checkpoint=%+v effect=%+v err=%v", state.Checkpoint, record, effectErr)
				}
			}
			_, externalErr := os.Stat(effectPath)
			wantExternal := stage == "during_effect" || stage == "after_completion"
			if wantExternal != (externalErr == nil) {
				t.Fatalf("external effect presence=%t want=%t err=%v", externalErr == nil, wantExternal, externalErr)
			}
		})
	}
}

func runEffectKillHelper() {
	stage := os.Getenv("EFFECT_KILL_STAGE")
	root := os.Getenv("EFFECT_KILL_ROOT")
	path := os.Getenv("EFFECT_KILL_PATH")
	external := os.Getenv("EFFECT_KILL_EXTERNAL")
	ctx := context.Background()
	store, err := Open(ctx, path, effectKillVault(root))
	if err != nil {
		os.Exit(2)
	}
	if err := store.CreateTurnState(ctx, TurnState{
		TurnID: "effect-kill-turn", ActorID: "effect-kill-actor",
		SessionID: "effect-kill-session", Content: "exercise effect",
		Status: StatusRunning, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		os.Exit(3)
	}
	checkpoint := Checkpoint{
		Messages: []protocol.Message{{Role: protocol.RoleUser, Content: "exercise effect"}},
		Step:     1,
	}
	if err := store.SaveTurnCheckpoint(ctx, "effect-kill-turn", checkpoint); err != nil {
		os.Exit(4)
	}
	if stage == "before_dispatch" {
		effectKillReady(stage)
	}
	arguments := json.RawMessage(`{"command":"write external marker"}`)
	checkpoint.PendingCall = &PendingCall{
		CallID: "effect-kill-call", IdempotencyKey: "effect-kill-key",
		ToolName: "shell", Arguments: arguments, DispatchedAt: time.Now().UTC(),
	}
	if err := store.SavePendingEffect(ctx, "effect-kill-turn", checkpoint,
		"effect-kill-key", "shell", arguments, false); err != nil {
		os.Exit(5)
	}
	if stage == "after_atomic_commit" {
		effectKillReady(stage)
	}
	if err := os.WriteFile(external, []byte("effect-began"), 0o600); err != nil {
		os.Exit(6)
	}
	if stage == "during_effect" {
		effectKillReady(stage)
	}
	result := EffectResult{Content: json.RawMessage(`{"outcome":"success","failure_layer":"none","retryable":false,"effect_status":"completed","evidence":[],"normalized_cause":"","suggested_recovery":"","artifact_references":[]}`)}
	if err := store.CompleteEffect(ctx, "effect-kill-key", result); err != nil {
		os.Exit(7)
	}
	effectKillReady(stage)
}

func effectKillReady(stage string) {
	_, _ = os.Stdout.WriteString(stage + "\n")
	_ = os.Stdout.Sync()
	select {}
}

func effectKillVault(root string) *vault.Session {
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: root, UserDID: "did:matrix:effect-kill",
		KEKHex: hex.EncodeToString(bytes.Repeat([]byte{0x71}, 32)),
	})
	if err != nil {
		panic(err)
	}
	return session
}
