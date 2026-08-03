// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"matrix/cortex"
	cortexstore "matrix/cortex/store"
	executortool "matrix/executor/tool"
	mclllm "matrix/mcl/llm"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	neomemory "matrix/neo/internal/memory"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/provider"
	"matrix/neo/internal/runtime/turnstate"
	neotools "matrix/neo/internal/tools"
	"matrix/neo/internal/writeback"
	"matrix/vault"
)

type recordedDelta struct {
	Turn    int
	Channel string
	Text    string
}

type recordingReporter struct {
	mu     sync.Mutex
	deltas []recordedDelta
	says   []string
}

func (reporter *recordingReporter) Delta(
	turn int,
	channel string,
	text string,
) {
	reporter.mu.Lock()
	reporter.deltas = append(reporter.deltas, recordedDelta{
		Turn: turn, Channel: channel, Text: text,
	})
	reporter.mu.Unlock()
}

func (reporter *recordingReporter) snapshot() []recordedDelta {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	return append([]recordedDelta(nil), reporter.deltas...)
}

func (reporter *recordingReporter) Say(
	content string,
	_ bool,
) {
	reporter.mu.Lock()
	reporter.says = append(reporter.says, content)
	reporter.mu.Unlock()
}

func (reporter *recordingReporter) said() []string {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	return append([]string(nil), reporter.says...)
}

func TestCleanWindowLawWithRealMiMoAndExecDispatch(t *testing.T) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	var (
		mu       sync.Mutex
		requests []gatewayRequest
		step     int
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
			requests = append(requests, decoded)
			step++
			current := step
			mu.Unlock()
			switch current {
			case 1:
				writeSSEText(
					writer,
					"unsafe <tool_call>broken</tool_call>",
				)
			case 2:
				writeSSETool(writer, "missing-expect", "exec__shell",
					map[string]interface{}{
						"command": "printf missing-expect",
						"cwd":     workdir,
					})
			case 3:
				writeSSETool(writer, "real-dispatch", "exec__shell",
					map[string]interface{}{
						"command": "printf resurrection-real-dispatch",
						"cwd":     workdir,
						"expect":  "prints resurrection-real-dispatch",
					})
			case 4:
				writeSSEText(writer, "")
			case 5:
				writeSSEText(
					writer,
					"I already provided the answer above.",
				)
			default:
				writeSSEText(
					writer,
					"The real exec bridge ran the requested resurrection command and returned resurrection-real-dispatch.",
				)
			}
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "clean-window-real"
	store := realTurnStore(t, turnID,
		"Run the resurrection command and report the result.")
	generator := realMiMoGenerator(t, gateway.URL)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &recordingReporter{}
	runtimeLoop, err := New(
		generator, adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			SystemPrompt: "Work through the active tool surface.",
			IdleTimeout:  20 * time.Second,
		},
		Dependencies{
			Observer: NewReporterObserver(reporter, 0),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Turn(
		t.Context(),
		"Run the resurrection command and report the result.",
	)
	if err != nil {
		t.Fatalf("Turn() = %v", err)
	}
	if response.HonestPartial ||
		!strings.Contains(response.Content, "resurrection-real-dispatch") ||
		len(response.ToolEvents) != 1 ||
		response.ToolEvents[0].Call.ID != "real-dispatch" ||
		response.ToolEvents[0].Error != "" {
		t.Fatalf("response = %+v", response)
	}

	loaded, err := store.LoadTurnState(t.Context(), turnID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != turnstate.StatusCompleted ||
		loaded.Checkpoint == nil ||
		loaded.Checkpoint.PendingCall != nil {
		t.Fatalf("durable state = %+v", loaded)
	}
	transcript, err := json.Marshal(loaded.Checkpoint.Messages)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"unsafe <tool_call>",
		"missing-expect",
		"I already provided the answer above.",
		textualToolRepairPrompt,
		expectationRepairPrompt,
		emptyFinalRepairPrompt,
		"The previous candidate answer was rejected",
	} {
		if strings.Contains(string(transcript), forbidden) {
			t.Fatalf(
				"rejected or repair content reached durable transcript: %s",
				transcript,
			)
		}
	}
	if got := roleSequence(loaded.Checkpoint.Messages); got !=
		"user,assistant,tool,assistant" {
		t.Fatalf("durable role sequence = %s", got)
	}
	if len(loaded.Checkpoint.ToolEvents) != 1 {
		t.Fatalf(
			"durable tool events = %d",
			len(loaded.Checkpoint.ToolEvents),
		)
	}

	deltas := reporter.snapshot()
	retractions := 0
	commits := 0
	for _, delta := range deltas {
		switch delta.Channel {
		case "retraction":
			retractions++
		case "commit":
			commits++
		}
	}
	if retractions != 1 || commits != 2 {
		t.Fatalf(
			"observer retractions=%d commits=%d deltas=%+v",
			retractions, commits, deltas,
		)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 6 {
		t.Fatalf("task provider calls = %d, want 6", len(requests))
	}
	assertPromptIsRequestLocal(t, requests, textualToolRepairPrompt)
	assertPromptIsRequestLocal(t, requests, expectationRepairPrompt)
	assertPromptIsRequestLocal(t, requests, emptyFinalRepairPrompt)
}

func TestRealLoopCommitsVerifiableToolEventBeforeCompletion(
	t *testing.T,
) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	var taskCalls int
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayRequest
			_ = json.Unmarshal(body, &decoded)
			if handleCapabilityCanary(writer, decoded) {
				return
			}
			taskCalls++
			if taskCalls == 1 {
				writeSSETool(
					writer, "anchored-call", "exec__shell",
					map[string]interface{}{
						"command": "printf anchored-output",
						"cwd":     workdir,
						"expect":  "prints anchored-output",
					},
				)
				return
			}
			writeSSEText(
				writer,
				"The anchored tool evidence completed the requested report.",
			)
		},
	))
	t.Cleanup(gateway.Close)
	journalStore, err := cortexstore.Open(
		t.TempDir(), "runtime-integrity-test", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journalStore.Close() })
	cx := cortex.New(journalStore)
	before, err := cx.OverallRoot()
	if err != nil {
		t.Fatal(err)
	}
	turnID := "integrity-spine-real-loop"
	userContent := "Run the anchored command and report its evidence."
	turns := realTurnStore(t, turnID, userContent)
	tools, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), tools, turns,
		Config{
			TurnID: turnID, ConversationID: "integrity-conversation",
			Model: "mimo-v2", IdleTimeout: 10 * time.Second,
		},
		Dependencies{EvidenceJournal: &CortexToolJournal{
			Cortex: cx, CreatedBy: "did:matrix:integrity-test",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Turn(t.Context(), userContent)
	if err != nil || len(response.ToolEvents) != 1 {
		t.Fatalf("integrity response=%+v err=%v", response, err)
	}
	event := response.ToolEvents[0]
	if event.Citation == nil ||
		event.MatchVerdict != cortex.ToolMatchMatched ||
		event.SubgoalID != "root" {
		t.Fatalf("tool event missing integrity fields: %+v", event)
	}
	payload, err := cx.VerifyToolEventCitation(*event.Citation)
	if err != nil || payload.CallID != event.Call.ID ||
		payload.ToolName != event.Call.Name ||
		payload.MatchVerdict != cortex.ToolMatchMatched {
		t.Fatalf("verified tool payload=%+v err=%v", payload, err)
	}
	after, err := cx.OverallRoot()
	if err != nil || before == after {
		t.Fatalf("OverallRoot before=%x after=%x err=%v", before, after, err)
	}
}

func TestCortexActivationRefreshAndSingleDeliveryChoke(t *testing.T) {
	manager := realExecManager(t)
	userContent := "Check resurrection activation and report the evidence."
	workdir := t.TempDir()
	var (
		mu       sync.Mutex
		requests []gatewayRequest
		step     int
	)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayRequest
			_ = json.Unmarshal(body, &decoded)
			if handleCapabilityCanary(writer, decoded) {
				return
			}
			mu.Lock()
			requests = append(requests, decoded)
			step++
			current := step
			mu.Unlock()
			if current == 1 {
				writeSSETool(
					writer, "activation-dispatch", "exec__shell",
					map[string]interface{}{
						"command": "printf 'resurrection activation evidence'",
						"cwd":     workdir,
						"expect":  "prints resurrection activation evidence",
					},
				)
				return
			}
			if current == 2 {
				writeSSEText(
					writer,
					"I already provided the answer above.",
				)
				return
			}
			writeSSEText(
				writer,
				"I'm Grok, and the resurrection activation evidence was verified.",
			)
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "cortex-activation-delivery"
	conversationID := "conversation-cortex-activation"
	store := realTurnStore(t, turnID, userContent)
	cortexAdapter, pager, consolidator, stopConsolidator,
		extractionCalls, extractionBodies :=
		realCortexDelivery(t, conversationID)
	if cortexAdapter.budgetTokens != 20_000 {
		t.Fatalf(
			"1M activation budget = %d, want 20000",
			cortexAdapter.budgetTokens,
		)
	}
	premises := &PremiseSet{}
	premises.Replace([]string{
		"The exec result must contain resurrection activation evidence.",
	})
	toolAdapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &recordingReporter{}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL),
		toolAdapter,
		store,
		Config{
			TurnID: turnID, ConversationID: conversationID,
			Model:        "mimo-v2",
			SystemPrompt: "byte-stable-resurrection-prefix",
			IdleTimeout:  20 * time.Second,
		},
		Dependencies{
			Activation: cortexAdapter,
			Premises:   premises,
			Recorder:   cortexAdapter,
			Delivery: &DeliveryChoke{
				AgentName: "Neo", Reporter: reporter,
				Recorder: cortexAdapter, Consolidator: consolidator,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Turn(t.Context(), userContent)
	if err != nil {
		t.Fatal(err)
	}
	stopConsolidator()
	if response.Content !=
		"I'm Neo, and the resurrection activation evidence was verified." {
		t.Fatalf("scrubbed response = %q", response.Content)
	}
	if said := reporter.said(); len(said) != 1 ||
		said[0] != response.Content {
		t.Fatalf("delivery reporter = %v", said)
	}

	mu.Lock()
	if len(requests) != 3 {
		mu.Unlock()
		t.Fatalf("provider iterations = %d, want 3", len(requests))
	}
	first := requests[0]
	second := requests[1]
	third := requests[2]
	mu.Unlock()
	activationContent := func(t *testing.T, iteration int, request gatewayRequest) string {
		t.Helper()
		activationIndex := -1
		lastUserIndex := -1
		for messageIndex, message := range request.Messages {
			if message.Role == "system" && strings.Contains(message.Content, "Active premises:") {
				activationIndex = messageIndex
			}
			if message.Role == "user" {
				lastUserIndex = messageIndex
			}
		}
		if activationIndex < 0 || lastUserIndex < 0 || activationIndex >= lastUserIndex {
			t.Fatalf(
				"iteration %d did not keep activation before the authoritative user message: %+v",
				iteration, request.Messages,
			)
		}
		content := request.Messages[activationIndex].Content
		if len([]rune(content)) > cortexAdapter.budgetTokens*4 {
			t.Fatalf("iteration %d activation exceeded budget", iteration)
		}
		return content
	}
	for index, request := range []gatewayRequest{first, second, third} {
		if len(request.Messages) < 2 ||
			request.Messages[0].Role != "system" ||
			request.Messages[0].Content !=
				"byte-stable-resurrection-prefix" {
			t.Fatalf(
				"iteration %d changed stable prefix: %+v",
				index+1, request.Messages,
			)
		}
	}
	firstTail := activationContent(t, 1, first)
	secondTail := activationContent(t, 2, second)
	thirdTail := activationContent(t, 3, third)
	if strings.Contains(firstTail, "tool result") ||
		!strings.Contains(secondTail, "resurrection activation evidence") ||
		firstTail == secondTail ||
		secondTail != thirdTail {
		t.Fatalf(
			"activation cadence violated\nfirst=%s\nsecond=%s\nthird=%s",
			firstTail, secondTail, thirdTail,
		)
	}
	assertPromptIsRequestLocal(
		t, requests,
		finalAnswerRepairPrompt(
			"it exposed internal control flow or referred to an answer that was not delivered",
		),
	)

	transcript, err := pager.Transcript(conversationID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	rendered, _ := json.Marshal(transcript)
	if strings.Contains(string(rendered), "Grok") ||
		strings.Contains(string(rendered), "already provided") ||
		strings.Contains(string(rendered), "previous candidate") ||
		!strings.Contains(string(rendered), "I'm Neo") {
		t.Fatalf("cortex delivery transcript = %s", rendered)
	}
	if extractionCalls.Load() != 1 {
		t.Fatalf(
			"consolidation calls = %d, want exactly 1",
			extractionCalls.Load(),
		)
	}
	extractionBodies.mu.Lock()
	defer extractionBodies.mu.Unlock()
	if len(extractionBodies.values) != 1 ||
		strings.Contains(extractionBodies.values[0], "Grok") ||
		strings.Contains(extractionBodies.values[0], "already provided") ||
		strings.Contains(extractionBodies.values[0], "previous candidate") ||
		!strings.Contains(extractionBodies.values[0], "I'm Neo") {
		t.Fatalf(
			"consolidation body = %v", extractionBodies.values,
		)
	}
}

func TestDeliverySuppressionAndIncompleteConsolidateExactlyOnce(
	t *testing.T,
) {
	manager := realExecManager(t)
	tests := []struct {
		name          string
		userContent   string
		answer        string
		providerErr   bool
		honestPartial bool
		wantSays      int
	}{
		{
			name:        "heartbeat",
			userContent: "HEARTBEAT: review active work.",
			answer:      heartbeatOK,
		},
		{
			name:        "automatrix",
			userContent: "AUTOMATRIX: review pending work.",
			answer:      automatrixIdle,
		},
		{
			name:        "provider incomplete",
			userContent: "Complete the resurrection check.",
			providerErr: true,
			wantSays:    1,
		},
		{
			name:          "honest partial",
			userContent:   "Complete the resurrection check.",
			answer:        "I already provided the answer above.",
			honestPartial: true,
			wantSays:      1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(
				func(writer http.ResponseWriter, request *http.Request) {
					body, _ := io.ReadAll(request.Body)
					var decoded gatewayRequest
					_ = json.Unmarshal(body, &decoded)
					if handleCapabilityCanary(writer, decoded) {
						return
					}
					if test.providerErr {
						http.Error(
							writer, "provider unavailable",
							http.StatusServiceUnavailable,
						)
						return
					}
					writeSSEText(writer, test.answer)
				},
			))
			t.Cleanup(gateway.Close)
			turnID := "delivery-" + strings.ReplaceAll(
				test.name, " ", "-",
			)
			conversationID := turnID + "-conversation"
			store := realTurnStore(
				t, turnID, test.userContent,
			)
			cortexAdapter, _, consolidator, stopConsolidator,
				extractionCalls, _ :=
				realCortexDelivery(t, conversationID)
			toolAdapter, err := NewToolManagerAdapter(manager, nil)
			if err != nil {
				t.Fatal(err)
			}
			reporter := &recordingReporter{}
			runtimeLoop, err := New(
				realMiMoGenerator(t, gateway.URL),
				toolAdapter, store,
				Config{
					TurnID: turnID, ConversationID: conversationID,
					Model:       "mimo-v2",
					IdleTimeout: 20 * time.Second,
				},
				Dependencies{
					Activation: cortexAdapter,
					Recorder:   cortexAdapter,
					Delivery: &DeliveryChoke{
						Reporter: reporter, Recorder: cortexAdapter,
						Consolidator: consolidator,
					},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			response, turnErr := runtimeLoop.Turn(
				t.Context(), test.userContent,
			)
			stopConsolidator()
			if test.providerErr {
				var incomplete *Incomplete
				if !errors.As(turnErr, &incomplete) {
					t.Fatalf("provider terminal = %v", turnErr)
				}
			} else if turnErr != nil ||
				!test.honestPartial && response.Content != test.answer ||
				test.honestPartial && !response.HonestPartial {
				t.Fatalf(
					"suppressed response=%+v err=%v",
					response, turnErr,
				)
			}
			if len(reporter.said()) != test.wantSays {
				t.Fatalf(
					"terminal delivery count = %d, want %d: %v",
					len(reporter.said()), test.wantSays,
					reporter.said(),
				)
			}
			if extractionCalls.Load() != 1 {
				t.Fatalf(
					"terminal consolidation calls = %d, want 1",
					extractionCalls.Load(),
				)
			}
		})
	}
}

func TestHonestPartialFloorAndBoundedRepairLadder(t *testing.T) {
	manager := realExecManager(t)
	var taskCalls int
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayRequest
			_ = json.Unmarshal(body, &decoded)
			if handleCapabilityCanary(writer, decoded) {
				return
			}
			taskCalls++
			writeSSEText(
				writer,
				"I already provided the answer above.",
			)
		},
	))
	t.Cleanup(gateway.Close)
	turnID := "honest-partial-real"
	userContent := "Produce the resurrection result."
	store := realTurnStore(t, turnID, userContent)
	generator := realMiMoGenerator(t, gateway.URL)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		generator, adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			FinalAnswerRepairLimit: 2,
			IdleTimeout:            20 * time.Second,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Turn(t.Context(), userContent)
	if err != nil {
		t.Fatalf("Turn() = %v", err)
	}
	if !response.HonestPartial ||
		!strings.Contains(response.Content, "will not claim") ||
		taskCalls != 3 {
		t.Fatalf(
			"response=%+v provider calls=%d", response, taskCalls,
		)
	}
	loaded, err := store.LoadTurnState(t.Context(), turnID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != turnstate.StatusCompleted ||
		loaded.Checkpoint == nil ||
		len(loaded.Checkpoint.Messages) != 2 ||
		strings.Contains(
			loaded.Checkpoint.Messages[1].Content,
			"already provided",
		) {
		t.Fatalf("durable honest partial = %+v", loaded)
	}
}

func TestLiveLoopRetractsRejectedRepairBeforeCommittingValidAnswer(
	t *testing.T,
) {
	manager := realExecManager(t)
	var taskCalls int
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayRequest
			_ = json.Unmarshal(body, &decoded)
			if handleCapabilityCanary(writer, decoded) {
				return
			}
			taskCalls++
			if taskCalls == 1 {
				writeSSEText(
					writer,
					"I already provided the answer above.",
				)
				return
			}
			writeSSEText(
				writer,
				"The requested resurrection repair completed successfully.",
			)
		},
	))
	t.Cleanup(gateway.Close)
	turnID := "retracted-repair-live-loop"
	userContent := "Complete the resurrection repair."
	store := realTurnStore(t, turnID, userContent)
	generator := realMiMoGenerator(t, gateway.URL)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &recordingReporter{}
	runtimeLoop, err := New(
		generator, adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			FinalAnswerRepairLimit: 2,
			IdleTimeout:            20 * time.Second,
		},
		Dependencies{
			Observer: NewReporterObserver(reporter, 0),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Turn(t.Context(), userContent)
	if err != nil {
		t.Fatal(err)
	}
	if taskCalls != 2 ||
		response.Content !=
			"The requested resurrection repair completed successfully." {
		t.Fatalf("response=%+v provider calls=%d", response, taskCalls)
	}
	deltas := reporter.snapshot()
	want := []recordedDelta{
		{
			Turn: 0, Channel: "content",
			Text: "I already provided the answer above.",
		},
		{Turn: 0, Channel: "retraction"},
		{
			Turn: 1, Channel: "content",
			Text: "The requested resurrection repair completed successfully.",
		},
		{Turn: 1, Channel: "commit"},
	}
	if len(deltas) != len(want) {
		t.Fatalf("stream deltas = %+v, want %+v", deltas, want)
	}
	for index := range want {
		if deltas[index] != want[index] {
			t.Fatalf(
				"stream delta %d = %+v, want %+v",
				index, deltas[index], want[index],
			)
		}
	}
	loaded, err := store.LoadTurnState(t.Context(), turnID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Checkpoint == nil ||
		len(loaded.Checkpoint.Messages) != 2 ||
		strings.Contains(
			loaded.Checkpoint.Messages[1].Content,
			"already provided",
		) {
		t.Fatalf("durable repaired checkpoint = %+v", loaded)
	}
}

func TestTerminalPathsAlwaysDeliverOrReturnTypedIncomplete(t *testing.T) {
	manager := realExecManager(t)
	tests := []struct {
		name       string
		status     int
		content    string
		gate       *StateCompletionGate
		wantResult string
	}{
		{
			name: "accepted", status: http.StatusOK,
			content:    "The resurrection request completed with verified output.",
			wantResult: "delivery",
		},
		{
			name: "controller stop", status: http.StatusOK,
			content: "The resurrection work reached the authority boundary.",
			gate: NewStateCompletionGate(CompletionDecision{
				Stop: true, Reason: "owner authorization is required",
			}),
			wantResult: "delivery",
		},
		{
			name: "completion ceiling", status: http.StatusOK,
			content: "The resurrection work has more verified steps.",
			gate: NewStateCompletionGate(CompletionDecision{
				Reason: "evidence missing", NextAction: "run the next check",
			}),
			wantResult: "incomplete",
		},
		{
			name:       "provider exhaustion",
			status:     http.StatusServiceUnavailable,
			wantResult: "incomplete",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(
				func(writer http.ResponseWriter, request *http.Request) {
					body, _ := io.ReadAll(request.Body)
					var decoded gatewayRequest
					_ = json.Unmarshal(body, &decoded)
					if handleCapabilityCanary(writer, decoded) {
						return
					}
					if test.status != http.StatusOK {
						http.Error(writer, "transient outage", test.status)
						return
					}
					writeSSEText(writer, test.content)
				},
			))
			t.Cleanup(gateway.Close)
			turnID := "terminal-" + strings.ReplaceAll(
				test.name, " ", "-",
			)
			userContent := "Complete the resurrection request."
			store := realTurnStore(t, turnID, userContent)
			generator := realMiMoGenerator(t, gateway.URL)
			adapter, err := NewToolManagerAdapter(manager, nil)
			if err != nil {
				t.Fatal(err)
			}
			runtimeLoop, err := New(
				generator, adapter, store,
				Config{
					TurnID: turnID, Model: "mimo-v2",
					CompletionDeferrals: 1,
					IdleTimeout:         20 * time.Second,
				},
				Dependencies{CompletionGate: test.gate},
			)
			if err != nil {
				t.Fatal(err)
			}
			response, turnErr := runtimeLoop.Turn(
				t.Context(), userContent,
			)
			switch test.wantResult {
			case "delivery":
				if turnErr != nil || strings.TrimSpace(response.Content) == "" {
					t.Fatalf(
						"terminal path lost delivery: response=%+v err=%v",
						response, turnErr,
					)
				}
			case "incomplete":
				var incomplete *Incomplete
				if !errors.As(turnErr, &incomplete) ||
					len(incomplete.Checkpoint.Messages) == 0 ||
					strings.TrimSpace(incomplete.RecoveryAdvice) == "" {
					t.Fatalf(
						"terminal path is not typed incomplete: response=%+v err=%v",
						response, turnErr,
					)
				}
			}
		})
	}
}

func TestPendingEffectStopsForReconciliationBeforeResume(t *testing.T) {
	manager := realExecManager(t)
	var providerCalls int
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			providerCalls++
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayRequest
			_ = json.Unmarshal(body, &decoded)
			if handleCapabilityCanary(writer, decoded) {
				return
			}
			writeSSEText(
				writer,
				"The reconciled resurrection effect is present.",
			)
		},
	))
	t.Cleanup(gateway.Close)
	turnID := "pending-reconcile"
	userContent := "Resume the resurrection effect."
	store := realTurnStore(t, turnID, userContent)
	pending := turnstate.PendingCall{
		CallID: "pending-call", IdempotencyKey: "idem-pending",
		ToolName:     "exec__shell",
		Arguments:    json.RawMessage(`{"command":"printf must-not-run","cwd":"/tmp"}`),
		Expect:       "prints reconciled output",
		DispatchedAt: time.Now().UTC(),
	}
	checkpoint := turnstate.Checkpoint{
		Messages: []protocol.Message{
			{Role: protocol.RoleUser, Content: userContent},
			{
				Role: protocol.RoleAssistant,
				ToolCalls: []protocol.NormalizedToolCall{{
					ID: pending.CallID, Name: pending.ToolName,
					Arguments: pending.Arguments,
				}},
			},
		},
		Step: 1, PendingCall: &pending,
		SavedAt: time.Now().UTC(),
	}
	if err := store.SaveTurnCheckpoint(
		t.Context(), turnID, checkpoint,
	); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			IdleTimeout: 20 * time.Second,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadTurnState(t.Context(), turnID)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Resume(
		t.Context(), userContent, *loaded.Checkpoint,
	)
	var incomplete *Incomplete
	if !errors.As(err, &incomplete) ||
		incomplete.Phase != "effect_reconciliation" ||
		incomplete.RecoveryAdvice !=
			"reconcile_effect_by_idempotency_key" ||
		len(response.ToolEvents) != 0 ||
		providerCalls != 0 {
		t.Fatalf(
			"reconciliation response=%+v provider_calls=%d err=%v",
			response, providerCalls, err,
		)
	}
}

type gatewayRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func handleCapabilityCanary(
	writer http.ResponseWriter,
	request gatewayRequest,
) bool {
	canary := false
	for _, tool := range request.Tools {
		if tool.Function.Name == "matrix_runtime_capability_echo" {
			canary = true
			break
		}
	}
	if !canary {
		return false
	}
	latest := request.Messages[len(request.Messages)-1].Content
	if strings.Contains(latest, "Reply with READY") {
		writeSSEText(writer, "READY")
		return true
	}
	writeSSETool(
		writer, "capability-call",
		"matrix_runtime_capability_echo",
		map[string]interface{}{
			"value": "READY", "expect": "returns READY",
		},
	)
	return true
}

func writeSSEText(writer http.ResponseWriter, content string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	payload, _ := json.Marshal(map[string]interface{}{
		"model": "mimo-v2",
		"choices": []interface{}{map[string]interface{}{
			"index":         0,
			"delta":         map[string]interface{}{"content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": 4, "completion_tokens": 3,
			"total_tokens": 7,
		},
	})
	fmt.Fprintf(writer, "data: %s\n\n", payload)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}

func writeSSETool(
	writer http.ResponseWriter,
	id string,
	name string,
	arguments map[string]interface{},
) {
	writer.Header().Set("Content-Type", "text/event-stream")
	rawArguments, _ := json.Marshal(arguments)
	payload, _ := json.Marshal(map[string]interface{}{
		"model": "mimo-v2",
		"choices": []interface{}{map[string]interface{}{
			"index": 0,
			"delta": map[string]interface{}{
				"tool_calls": []interface{}{map[string]interface{}{
					"index": 0, "id": id, "type": "function",
					"function": map[string]interface{}{
						"name": name, "arguments": string(rawArguments),
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": 4, "completion_tokens": 3,
			"total_tokens": 7,
		},
	})
	fmt.Fprintf(writer, "data: %s\n\n", payload)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}

type lockedStrings struct {
	mu     sync.Mutex
	values []string
}

func realCortexDelivery(
	t *testing.T,
	conversationID string,
) (
	*CortexAdapter,
	*neomemory.Pager,
	*writeback.Consolidator,
	func(),
	*atomic.Int32,
	*lockedStrings,
) {
	t.Helper()
	cfg := config.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "resurrection-runtime-" + conversationID
	cfg.ContextWindowTokens = 1_000_000
	pager, err := neomemory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pager.Close(); err != nil {
			t.Errorf("close cortex pager: %v", err)
		}
	})
	if _, err := pager.SetMemoryConsent(
		t.Context(), true, "runtime loop test user",
	); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewCortexAdapter(pager, cfg, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	bodies := &lockedStrings{}
	extractionServer := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			raw, _ := io.ReadAll(request.Body)
			bodies.mu.Lock()
			bodies.values = append(bodies.values, string(raw))
			bodies.mu.Unlock()
			calls.Add(1)
			writeSSEText(
				writer,
				`{"facts":[],"user_facts":[],"preferences":[],"corrections":[],"patterns":[],"opportunities":[],"outcome":null}`,
			)
		},
	))
	t.Cleanup(extractionServer.Close)
	extractionClient, err := llm.New(mclllm.Config{
		Model:       "resurrection-consolidator",
		Endpoint:    extractionServer.URL,
		GatewayURL:  extractionServer.URL,
		Provider:    mclllm.ProviderFireworks,
		ProviderSet: true,
		APIKey:      "runtime-loop-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	consolidator := writeback.New(
		extractionClient, nil, pager, cfg,
	)
	consolidator.Start()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(consolidator.Stop)
	}
	t.Cleanup(stop)
	return adapter, pager, consolidator, stop, &calls, bodies
}

func realMiMoGenerator(
	t *testing.T,
	gatewayURL string,
) *provider.MiMoGenerator {
	t.Helper()
	t.Setenv("MATRIX_GATEWAY_TOKEN", "test-gateway-token")
	adapter := &provider.MiMoAdapter{}
	client, err := provider.New(adapter, provider.Config{
		GatewayURL:     gatewayURL,
		BearerEnv:      "MATRIX_GATEWAY_TOKEN",
		ActorDID:       "did:matrix:runtime-loop-test",
		MaxAttempts:    3,
		BackoffInitial: time.Millisecond,
		BackoffMax:     2 * time.Millisecond,
		IdleTimeout:    10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	generator, err := provider.NewMiMoGenerator(client, adapter)
	if err != nil {
		t.Fatal(err)
	}
	return generator
}

func realExecManager(t *testing.T) *neotools.Manager {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node is required for real exec dispatch: %v", err)
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve runtime loop test source")
	}
	execBridge := filepath.Clean(filepath.Join(
		filepath.Dir(thisFile), "..", "..", "..", "..",
		"tools", "exec", "exec.mjs",
	))
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	manifest := executortool.AgentManifest{
		SchemaVersion: 1,
		Agent:         "matrix://agent/resurrection-loop-test",
		Servers: []executortool.ServerEntry{{
			Alias: "exec", Transport: "stdio",
			Command: "node", Args: []string{execBridge},
			PackageDigest: "sha256:" + strings.Repeat("c", 64),
			Version:       "0.1.0",
			Tools: []executortool.ToolEntry{
				{
					Name:            "shell",
					SideEffectClass: executortool.SideEffectShell,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_start",
					SideEffectClass: executortool.SideEffectShell,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_list",
					SideEffectClass: executortool.SideEffectRead,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_logs",
					SideEffectClass: executortool.SideEffectRead,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_stop",
					SideEffectClass: executortool.SideEffectShell,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_restart",
					SideEffectClass: executortool.SideEffectShell,
					TimeoutMs:       10_000,
				},
			},
		}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := neotools.Spawn(
		t.Context(),
		neotools.Options{
			ManifestPath: manifestPath,
			SpawnTimeout: 20 * time.Second,
			StderrSink:   io.Discard,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if warnings := manager.Warnings(); len(warnings) != 0 {
		_ = manager.Close()
		t.Fatalf("real exec bridge warnings: %v", warnings)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func realTurnStore(
	t *testing.T,
	turnID string,
	userContent string,
) *turnstate.Store {
	t.Helper()
	dir := t.TempDir()
	session, err := vault.Boot(t.Context(), vault.Config{
		Required: true, DataDir: dir,
		UserDID: "did:matrix:runtime-loop-test",
		KEKHex: hex.EncodeToString(
			bytes.Repeat([]byte{0x5a}, 32),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := turnstate.Open(
		t.Context(), filepath.Join(dir, "turnstate.db"), session,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTurnState(
		t.Context(),
		turnstate.TurnState{
			TurnID: turnID, ActorID: "runtime-loop-test",
			SessionID: "runtime-loop-session",
			Content:   userContent, Status: turnstate.StatusRunning,
			UpdatedAt: time.Now().UTC(),
		},
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer cancel()
		if err := store.Close(ctx); err != nil {
			t.Errorf("close turn store: %v", err)
		}
	})
	return store
}

func roleSequence(messages []protocol.Message) string {
	roles := make([]string, 0, len(messages))
	for _, message := range messages {
		roles = append(roles, string(message.Role))
	}
	return strings.Join(roles, ",")
}

func assertPromptIsRequestLocal(
	t *testing.T,
	requests []gatewayRequest,
	prompt string,
) {
	t.Helper()
	seen := 0
	for _, request := range requests {
		for _, message := range request.Messages {
			if message.Content == prompt {
				seen++
			}
		}
	}
	if seen != 1 {
		encoded, _ := json.Marshal(requests)
		t.Fatalf(
			"repair prompt appeared in %d requests, want 1; requests=%s",
			seen, encoded,
		)
	}
}
