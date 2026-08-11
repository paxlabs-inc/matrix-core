// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"matrix/cortexclient"
	"matrix/neo/internal/consolidation"
	"matrix/neo/internal/runtime/turnstate"
)

func realCortexdBinary(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("NEOCORTEX_CORTEXD"); path != "" {
		return path
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve runtime loop test source")
	}
	binary := filepath.Clean(filepath.Join(
		filepath.Dir(thisFile), "..", "..", "..", "..",
		"neocortex", "build", "debug", "cortexd",
	))
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf(
			"real cortexd binary not found at %s; build neocortex or set NEOCORTEX_CORTEXD",
			binary,
		)
	}
	return binary
}

func realNeocortexSeam(
	t *testing.T,
	conversationID string,
) (*cortexclient.LoopSeam, *cortexclient.Client, *cortexclient.Supervisor) {
	t.Helper()
	directory := t.TempDir()
	socket := filepath.Join(directory, "cortexd.sock")
	configPath := filepath.Join(directory, "cortexd.conf")
	config := "socket=" + socket + "\n" +
		"data=" + filepath.Join(directory, "data") + "\n" +
		"user=0102030405060708090a0b0c0d0e0f10\n" +
		"kek=1111111111111111111111111111111111111111111111111111111111111111\n" +
		"signing_seed=2222222222222222222222222222222222222222222222222222222222222222\n" +
		"admin_token=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
		"actor=11:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write cortexd config: %v", err)
	}
	supervisor := &cortexclient.Supervisor{
		Binary:       realCortexdBinary(t),
		ConfigPath:   configPath,
		RestartDelay: 100 * time.Millisecond,
	}
	if err := supervisor.Start(); err != nil {
		t.Fatalf("start cortexd: %v", err)
	}
	t.Cleanup(supervisor.Stop)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if info, err := os.Stat(socket); err == nil &&
			info.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cortexd socket never appeared at %s", socket)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var token [32]byte
	for index := range token {
		token[index] = 0xbb
	}
	var connection [16]byte
	connection[0] = 0x61
	connection[15] = 0x61 ^ 0xa5
	client, err := cortexclient.Dial(cortexclient.Config{
		SocketPath:      socket,
		CapabilityToken: token,
		ConnectionID:    connection,
		DialTimeout:     100 * time.Millisecond,
		PendingLimit:    1,
	})
	if err != nil {
		t.Fatalf("dial cortexd: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	seam, err := cortexclient.NewLoopSeam(client, cortexclient.SeamConfig{
		ConversationID: conversationID,
		BudgetTokens:   100_000,
	})
	if err != nil {
		t.Fatalf("new loop seam: %v", err)
	}
	return seam, client, supervisor
}

func TestRealLoopSmokeOnNeocortexSubstrate(t *testing.T) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	seam, _, _ := realNeocortexSeam(t, "neocortex-smoke")
	adapter, err := NewNeocortexAdapter(seam)
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu   sync.Mutex
		step int
	)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				return
			}
			var decoded gatewayRequest
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Error(err)
				return
			}
			if handleCapabilityCanary(writer, decoded) {
				return
			}
			mu.Lock()
			step++
			current := step
			mu.Unlock()
			if current == 1 {
				writeSSETool(writer, "neocortex-dispatch", "exec__shell",
					map[string]interface{}{
						"command": "printf neocortex-smoke-dispatch",
						"cwd":     workdir,
						"expect":  "prints neocortex-smoke-dispatch",
					})
				return
			}
			writeSSEText(
				writer,
				"The command ran on the neocortex substrate and returned neocortex-smoke-dispatch.",
			)
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "neocortex-smoke-turn"
	turns := realTurnStore(t, turnID,
		"Run the neocortex smoke command and report the result.")
	checkpoints := &NeocortexCheckpointStore{Seam: seam, Turns: turns}
	toolAdapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &recordingReporter{}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), toolAdapter, checkpoints,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			SystemPrompt: "Work through the active tool surface.",
			IdleTimeout:  20 * time.Second,
		},
		Dependencies{
			Observer:        NewReporterObserver(reporter, 0),
			Activation:      adapter,
			Recorder:        adapter,
			EvidenceJournal: adapter,
			Delivery: &DeliveryChoke{
				Reporter: reporter, Recorder: adapter,
				Consolidator: adapter, IntentID: turnID, Attempt: 1,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Turn(
		t.Context(),
		"Run the neocortex smoke command and report the result.",
	)
	if err != nil {
		t.Fatalf("Turn() = %v", err)
	}
	if response.HonestPartial ||
		!strings.Contains(response.Content, "neocortex-smoke-dispatch") ||
		len(response.ToolEvents) != 1 ||
		response.ToolEvents[0].Error != "" {
		t.Fatalf("response = %+v", response)
	}
	citation := response.ToolEvents[0].Citation
	var zero [32]byte
	if citation == nil || citation.Seq == 0 ||
		citation.LeafCount < citation.Seq ||
		citation.LeafHash == zero || citation.JournalRoot == zero {
		t.Fatalf("tool evidence is not anchored in cortexd: %+v", citation)
	}
	payload, err := adapter.VerifyToolEventCitation(*citation)
	if err != nil || payload.CallID != "neocortex-dispatch" ||
		payload.ToolName != "exec__shell" {
		t.Fatalf("verify live cortexd evidence: payload=%+v err=%v", payload, err)
	}

	loaded, err := turns.LoadTurnState(t.Context(), turnID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != turnstate.StatusCompleted ||
		loaded.Checkpoint == nil || loaded.Checkpoint.PendingCall != nil {
		t.Fatalf("durable state = %+v", loaded)
	}
	if got := roleSequence(loaded.Checkpoint.Messages); got !=
		"user,assistant,tool,assistant" {
		t.Fatalf("durable role sequence = %s", got)
	}

	if _, _, err := seam.LatestTurnCheckpoint(t.Context(), turnID); err == nil {
		t.Fatal("operational turn checkpoint leaked into the Neocortex event log")
	}
	localOnly := *loaded.Checkpoint
	localOnly.Step += 1000
	if err := turns.SaveTurnCheckpoint(t.Context(), turnID, localOnly); err != nil {
		t.Fatalf("write divergent local checkpoint: %v", err)
	}
	recovered, err := checkpoints.LoadTurnState(t.Context(), turnID)
	if err != nil {
		t.Fatalf("load checkpoint through neocortex store: %v", err)
	}
	if recovered.Checkpoint == nil || recovered.Checkpoint.Step != localOnly.Step {
		t.Fatalf("recovery did not use the dedicated operational store: recovered=%+v local=%d",
			recovered.Checkpoint, localOnly.Step)
	}

	conversation, sequenceLow, sequenceHigh := adapter.ProvenanceRange()
	adapter.Consolidate(consolidation.Job{
		IntentID: "consolidation-contract", Attempt: 1, Terminal: true,
		Objective: "verify the consolidation sink",
		Evidence: []consolidation.Evidence{{
			Kind: consolidation.EvidenceVerified, Content: "real cortexd acknowledged the tool result",
		}},
		Outcome: "neocortex-consolidation-only-marker",
		Conv:    conversation, HaveSeq: true, SeqLo: sequenceLow, SeqHi: sequenceHigh,
	})
	if err := adapter.RecordError(); err != nil {
		t.Fatalf("neocortex recorder/consolidator error: %v", err)
	}

	activation, err := adapter.Activate(t.Context(), ActivationRequest{
		Query: "neocortex smoke command result",
	})
	if err != nil {
		t.Fatalf("activate on neocortex substrate: %v", err)
	}
	for _, forbidden := range []string{
		"Run the neocortex smoke command",
		"neocortex-smoke-dispatch",
		"Consolidation event",
	} {
		if strings.Contains(activation, forbidden) {
			t.Fatalf("same-conversation or consolidation content leaked through activation as %q:\n%s", forbidden, activation)
		}
	}
}

func TestRealLoopRecoversFromCortexdSIGKILLDuringToolTurn(t *testing.T) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	conversationID := "neocortex-sigkill"
	seam, client, supervisor := realNeocortexSeam(t, conversationID)
	adapter, err := NewNeocortexAdapter(seam)
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu   sync.Mutex
		step int
	)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				return
			}
			var decoded gatewayRequest
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Error(err)
				return
			}
			if handleCapabilityCanary(writer, decoded) {
				return
			}
			mu.Lock()
			step++
			current := step
			mu.Unlock()
			if current == 1 {
				writeSSETool(writer, "sigkill-dispatch", "exec__shell",
					map[string]interface{}{
						"command": "printf 'applied\\n' >> applications.log; " +
							": > tool-started; " +
							"while [ ! -f release-tool ]; do sleep 0.02; done; " +
							"printf cortexd-recovered-result",
						"cwd":    workdir,
						"expect": "records one application and prints cortexd-recovered-result",
					})
				return
			}
			writeSSEText(writer,
				"The active tool turn survived cortexd recovery and returned cortexd-recovered-result.")
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "neocortex-sigkill-turn"
	userContent := "Run the guarded command once while cortexd is restarted and report the result."
	turns := realTurnStore(t, turnID, userContent)
	checkpoints := &NeocortexCheckpointStore{Seam: seam, Turns: turns}
	toolAdapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &recordingReporter{}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), toolAdapter, checkpoints,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			SystemPrompt: "Work through the active tool surface.",
			IdleTimeout:  20 * time.Second,
		},
		Dependencies{
			Observer:        NewReporterObserver(reporter, 0),
			Activation:      adapter,
			Recorder:        adapter,
			EvidenceJournal: adapter,
			Delivery: &DeliveryChoke{
				Reporter: reporter, Recorder: adapter,
				Consolidator: adapter, IntentID: turnID, Attempt: 1,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	type turnResult struct {
		response Response
		err      error
	}
	done := make(chan turnResult, 1)
	go func() {
		response, turnErr := runtimeLoop.Turn(t.Context(), userContent)
		done <- turnResult{response: response, err: turnErr}
	}()

	releasePath := filepath.Join(workdir, "release-tool")
	releaseTool := func() {
		_ = os.WriteFile(releasePath, []byte("release"), 0o600)
	}
	defer releaseTool()
	startedPath := filepath.Join(workdir, "tool-started")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("real tool did not enter its active section at %s", startedPath)
		}
		time.Sleep(20 * time.Millisecond)
	}
	applicationsPath := filepath.Join(workdir, "applications.log")
	applications, err := os.ReadFile(applicationsPath)
	if err != nil || strings.Count(string(applications), "applied\n") != 1 {
		t.Fatalf("applications before SIGKILL = %q, err=%v", applications, err)
	}

	oldPID := supervisor.Pid()
	if oldPID <= 0 {
		t.Fatalf("supervised cortexd pid = %d", oldPID)
	}
	if err := syscall.Kill(oldPID, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL cortexd pid %d: %v", oldPID, err)
	}
	deadline = time.Now().Add(10 * time.Second)
	newPID := 0
	for {
		newPID = supervisor.Pid()
		if newPID > 0 && newPID != oldPID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cortexd did not restart after SIGKILL of pid %d", oldPID)
		}
		time.Sleep(20 * time.Millisecond)
	}

	var interrupted turnstate.Checkpoint
	deadline = time.Now().Add(10 * time.Second)
	for {
		loaded, checkpointErr := turns.LoadTurnState(t.Context(), turnID)
		if checkpointErr == nil && loaded.Checkpoint != nil {
			interrupted = *loaded.Checkpoint
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("checkpoint unavailable after cortexd restart pid %d: %v",
				newPID, checkpointErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if interrupted.PendingCall == nil ||
		interrupted.PendingCall.CallID != "sigkill-dispatch" ||
		roleSequence(interrupted.Messages) != "user,assistant" {
		t.Fatalf("restart lost active-turn checkpoint: %+v", interrupted)
	}

	releaseTool()
	var result turnResult
	select {
	case result = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("runtime loop did not complete after cortexd recovery")
	}
	if result.err != nil {
		t.Fatalf("Turn() after cortexd recovery = %v", result.err)
	}
	if result.response.HonestPartial ||
		!strings.Contains(result.response.Content, "cortexd-recovered-result") ||
		len(result.response.ToolEvents) != 1 ||
		result.response.ToolEvents[0].Error != "" {
		t.Fatalf("recovered response = %+v", result.response)
	}
	applications, err = os.ReadFile(applicationsPath)
	if err != nil || strings.Count(string(applications), "applied\n") != 1 {
		t.Fatalf("tool application count after recovery = %q, err=%v", applications, err)
	}
	citation := result.response.ToolEvents[0].Citation
	if citation == nil {
		t.Fatal("recovered tool event has no cortexd citation")
	}
	payload, err := adapter.VerifyToolEventCitation(*citation)
	if err != nil || payload.CallID != "sigkill-dispatch" ||
		payload.ToolName != "exec__shell" {
		t.Fatalf("recovered evidence verification: payload=%+v err=%v", payload, err)
	}

	loaded, err := checkpoints.LoadTurnState(t.Context(), turnID)
	if err != nil {
		t.Fatalf("load recovered turn state: %v", err)
	}
	if loaded.Status != turnstate.StatusCompleted || loaded.Checkpoint == nil ||
		loaded.Checkpoint.PendingCall != nil ||
		roleSequence(loaded.Checkpoint.Messages) != "user,assistant,tool,assistant" ||
		len(loaded.Checkpoint.ToolEvents) != 1 {
		t.Fatalf("recovered durable turn state = %+v", loaded)
	}

	records, truncated, err := client.Transcript(
		t.Context(), cortexclient.ConversationBytes(conversationID), 0, 256,
	)
	if err != nil {
		t.Fatalf("read recovered cortexd transcript: %v", err)
	}
	if truncated {
		t.Fatalf("recovered cortexd transcript unexpectedly truncated at %d records", len(records))
	}
	counts := map[cortexclient.EventKind]int{}
	for _, record := range records {
		counts[record.Kind]++
	}
	if counts[cortexclient.KindUserMsg] != 1 ||
		counts[cortexclient.KindToolCall] != 1 ||
		counts[cortexclient.KindToolResult] != 1 ||
		counts[cortexclient.KindDeliveredMsg] != 1 ||
		counts[cortexclient.KindReasoning] != 0 {
		t.Fatalf("recovered transcript event counts = %+v", counts)
	}
	mu.Lock()
	providerSteps := step
	mu.Unlock()
	if providerSteps != 2 {
		t.Fatalf("provider generations after recovery = %d, want 2", providerSteps)
	}
}

func TestNeocortexRecorderFailureDoesNotDestroyDeliveredAnswer(t *testing.T) {
	seam, _, supervisor := realNeocortexSeam(t, "neocortex-recorder-failure")
	adapter, err := NewNeocortexAdapter(seam)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.Stop()
	adapter.RecordUser("accepted into the one-entry degraded queue")
	adapter.RecordUser("must fail instead of disappearing past the queue bound")
	if err := adapter.RecordError(); !errors.Is(err, cortexclient.ErrQueueFull) {
		t.Fatalf("RecordError() = %v, want %v", err, cortexclient.ErrQueueFull)
	}
	reporter := &recordingReporter{}
	delivery := (&DeliveryChoke{
		Reporter: reporter, Recorder: adapter,
	}).Deliver(
		t.Context(), "preserve this result", turnstate.Checkpoint{},
		"must not surface without durable recording", false,
	)
	if delivery.Suppressed || len(reporter.said()) != 1 ||
		reporter.said()[0] != "must not surface without durable recording" {
		t.Fatalf("bookkeeping failure destroyed delivery: result=%+v output=%v",
			delivery, reporter.said())
	}
}
