// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"matrix/neo/internal/conversation"
	"matrix/neo/internal/sessionjournal"
)

func TestBuildRecoveryHandoffLeadsWithLatestUserContext(t *testing.T) {
	ctx := context.Background()
	conversations := conversation.Open(t.TempDir())
	const conversationID = "prior-thread"
	conversations.AppendUser(conversationID, "", "Initial request")
	conversations.AppendAssistant(conversationID, "", "I started the work")
	conversations.AppendUser(conversationID, "", "Latest exact user request")
	conversations.AppendAssistant(conversationID, "", "Current agent work state")

	journal, err := sessionjournal.Open(ctx, filepath.Join(t.TempDir(), "journal.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	_, err = journal.Append(ctx, sessionjournal.Event{
		ConversationID: conversationID,
		Kind:           sessionjournal.KindToolResult,
		DisplayContent: "failed with Bearer private-token and API_KEY=private-key",
		ToolResult: &sessionjournal.ToolResult{
			CallID: "tool-1", Name: "exec", IsError: true, FailureClass: "runtime",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	handoff := (&Engine{conv: conversations, journal: journal}).buildRecoveryHandoff(ctx, conversationID)
	latest := strings.Index(handoff, "Latest exact user request")
	failure := strings.Index(handoff, "Failure and unresolved context (secondary)")
	if latest < 0 || failure < 0 || latest >= failure {
		t.Fatalf("latest user context must lead failure diagnostics:\n%s", handoff)
	}
	if !strings.Contains(handoff, "Recent thread context (primary, chronological)") ||
		!strings.Contains(handoff, "Current agent work state") {
		t.Fatalf("recent thread context was not preserved:\n%s", handoff)
	}
	if strings.Contains(handoff, "private-token") || strings.Contains(handoff, "private-key") {
		t.Fatalf("recovery handoff leaked credential material:\n%s", handoff)
	}
	if strings.Count(handoff, "[REDACTED]") < 2 {
		t.Fatalf("credential material was not visibly redacted:\n%s", handoff)
	}
}

func TestRecoveryRequiresExplicitConfirmation(t *testing.T) {
	server := &Server{engine: &Engine{conv: conversation.Open(t.TempDir())}}
	request := httptest.NewRequest(http.MethodPost, "/recovery/recenter", strings.NewReader(`{"confirm":"no"}`))
	response := httptest.NewRecorder()
	server.handleRecovery(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("want explicit-confirmation rejection, got %d: %s", response.Code, response.Body.String())
	}
}

func TestRecoveryDefaultsToMostRecentRealThread(t *testing.T) {
	conversations := conversation.Open(t.TempDir())
	conversations.AppendUser("older", "", "old request")
	time.Sleep(time.Millisecond)
	conversations.AppendUser("latest", "", "most recent request")
	engine := &Engine{conv: conversations}

	sourceID := ""
	if sourceID == "" {
		if items := engine.conv.List(); len(items) > 0 {
			sourceID = items[0].ConversationID
		}
	}
	if sourceID != "latest" {
		t.Fatalf("blank recovery source selected %q, want latest", sourceID)
	}
}

func TestRecenterStopsVerifiedServiceAndPersistsFreshHandoff(t *testing.T) {
	bridge, err := recoveryExecBridgePath()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	stateOne := filepath.Join(dataDir, "services")
	stateTwo := filepath.Join(dataDir, "neo", "services")
	if err := os.MkdirAll(stateTwo, 0o700); err != nil {
		t.Fatal(err)
	}

	pid := startRecoveryTestService(t, bridge, dataDir, stateOne, root)
	t.Cleanup(func() {
		command := exec.Command("node", bridge, "--recenter-registry")
		command.Env = append(os.Environ(),
			"MATRIX_DATA_DIR="+dataDir,
			"MATRIX_EXEC_STATE_DIR="+stateOne,
			"MATRIX_EXEC_WORKDIR="+root,
		)
		_ = command.Run()
	})

	conversations := conversation.Open(filepath.Join(dataDir, "conversations"))
	const sourceID = "active-thread"
	conversations.AppendUser(sourceID, "", "Please finish the deployment repair")
	conversations.AppendAssistant(sourceID, "", "The runtime is still failing during verification")

	t.Setenv("MATRIX_DATA_DIR", dataDir)
	t.Setenv("MATRIX_EXEC_BRIDGE_PATH", bridge)
	t.Setenv("MATRIX_RECOVERY_EXEC_STATE_DIRS", stateOne+string(os.PathListSeparator)+stateTwo)
	engine := &Engine{conv: conversations, runs: map[string]*run{}}
	server := &Server{engine: engine}
	body := fmt.Sprintf(`{"confirm":"RECENTER","conversation_id":%q}`, sourceID)
	request := httptest.NewRequest(http.MethodPost, "/recovery/recenter", strings.NewReader(body))
	response := httptest.NewRecorder()
	server.handleRecovery(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("recenter failed with %d: %s", response.Code, response.Body.String())
	}

	var result struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ConversationID == "" || result.ConversationID == sourceID {
		t.Fatalf("recenter did not create a fresh conversation: %+v", result)
	}
	if waitForProcessExit(pid, 3*time.Second) == false {
		t.Fatalf("verified supervised process %d is still running", pid)
	}
	registry, err := os.ReadFile(filepath.Join(stateOne, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(registry)) != "{}" {
		t.Fatalf("supervised registry was not cleared: %s", registry)
	}
	source, handoff := conversations.RecoveryHandoff(result.ConversationID)
	if source != sourceID || !strings.Contains(handoff, "Latest user request (primary)") ||
		!strings.Contains(handoff, "Please finish the deployment repair") {
		t.Fatalf("fresh conversation did not receive the prior-thread handoff: source=%q handoff=%q", source, handoff)
	}
	for _, summary := range conversations.List() {
		if summary.ConversationID == result.ConversationID {
			t.Fatal("metadata-only recovery conversation became visible before its first user turn")
		}
	}
}

func startRecoveryTestService(t *testing.T, bridge, dataDir, stateDir, workDir string) int {
	t.Helper()
	request := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "service_start",
			"arguments": map[string]interface{}{
				"name": "recovery-owned", "command": `node -e "setInterval(() => {}, 1000)"`,
				"cwd": workDir, "autostart": true,
			},
		},
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("node", bridge)
	command.Env = append(os.Environ(),
		"MATRIX_DATA_DIR="+dataDir,
		"MATRIX_EXEC_STATE_DIR="+stateDir,
		"MATRIX_EXEC_WORKDIR="+workDir,
	)
	command.Stdin = bytes.NewReader(append(encoded, '\n'))
	output, err := command.Output()
	if err != nil {
		t.Fatalf("start supervised service: %v", err)
	}
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output), &response); err != nil || len(response.Result.Content) != 1 {
		t.Fatalf("decode service response: %v output=%s", err, output)
	}
	var started struct {
		OK  bool `json:"ok"`
		PID int  `json:"pid"`
	}
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &started); err != nil || !started.OK || started.PID <= 0 {
		t.Fatalf("decode service start result: %v result=%s", err, response.Result.Content[0].Text)
	}
	return started.PID
}

func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return true
		}
		closeParen := bytes.LastIndex(stat, []byte(") "))
		if closeParen >= 0 && closeParen+2 < len(stat) && (stat[closeParen+2] == 'Z' || stat[closeParen+2] == 'X') {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}
