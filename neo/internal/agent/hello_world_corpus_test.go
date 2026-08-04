// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	mcllm "matrix/mcl/llm"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

type helloWorldVariant struct {
	Name          string
	CurrentIntent bool
	Exact         bool
	Posture       bool
}

type helloWorldMetrics struct {
	StaleTaskResurrection    bool
	IndirectReferenceCorrect bool
	CorrectionRetained       bool
	DuplicateSideEffects     int
	ProtocolRecoveryLocal    bool
	SimpleModelCalls         int
	SimpleTurns              int
	SimpleLatency            time.Duration
	CriticalExecutionPassed  bool
}

func TestHelloWorldCorpusAB(t *testing.T) {
	variants := []helloWorldVariant{
		{Name: "baseline"},
		{Name: "current_intent", CurrentIntent: true},
		{Name: "exact_projection", Exact: true},
		{Name: "interaction_posture", Posture: true},
		{Name: "combined", CurrentIntent: true, Exact: true, Posture: true},
	}
	turns := helloWorldCorpusTurns()
	if len(turns) != 100 || !reflect.DeepEqual(turns, helloWorldCorpusTurns()) {
		t.Fatal("hello-world corpus is not a deterministic 100-turn sequence")
	}
	fingerprint := sha256.Sum256([]byte(strings.Join(turns, "\n")))
	t.Logf("corpus_sha256=%s", hex.EncodeToString(fingerprint[:]))

	results := make(map[string]helloWorldMetrics, len(variants))
	for _, variant := range variants {
		metrics := runHelloWorldCorpus(t, variant, turns)
		results[variant.Name] = metrics
		t.Logf("variant=%s stale_task=%t indirect_reference=%t correction=%t duplicate_effects=%d local_protocol_recovery=%t simple_model_calls=%d simple_turns=%d simple_latency_ms=%d critical_execution=%t",
			variant.Name, metrics.StaleTaskResurrection, metrics.IndirectReferenceCorrect,
			metrics.CorrectionRetained, metrics.DuplicateSideEffects, metrics.ProtocolRecoveryLocal,
			metrics.SimpleModelCalls, metrics.SimpleTurns, metrics.SimpleLatency.Milliseconds(), metrics.CriticalExecutionPassed)
		if metrics.DuplicateSideEffects != 0 {
			t.Fatalf("%s violated zero-duplicate-effect gate: %+v", variant.Name, metrics)
		}
		if !metrics.CriticalExecutionPassed {
			t.Fatalf("%s regressed critical execution: %+v", variant.Name, metrics)
		}
	}
	combined := results["combined"]
	if combined.StaleTaskResurrection || !combined.IndirectReferenceCorrect || !combined.CorrectionRetained || !combined.ProtocolRecoveryLocal {
		t.Fatalf("combined substrate did not improve all continuity/recovery reads: %+v", combined)
	}
	if !results["baseline"].StaleTaskResurrection {
		t.Fatalf("baseline failed to reproduce set-once task anchoring: %+v", results["baseline"])
	}
}

func helloWorldCorpusTurns() []string {
	turns := make([]string, 100)
	for index := range turns {
		turns[index] = fmt.Sprintf("Everyday topic %02d: acknowledge this topic change briefly.", index)
	}
	turns[0] = "prepare the operational deployment plan"
	turns[1] = "what film should I watch tonight?"
	turns[5] = "Decision: use PostgreSQL for the session cache."
	turns[6] = "Correction: use SQLite for the session cache, not PostgreSQL."
	turns[20] = "read the reference file and remember its exact token"
	turns[41] = "what was the exact reference token from that tool result?"
	turns[42] = "exercise the invalid tool name corpus case"
	turns[43] = "write using the malformed argument corpus case"
	turns[44] = "write the corpus effect file exactly once and verify it"
	turns[99] = "what database did I finally choose for the session cache?"
	return turns
}

func runHelloWorldCorpus(t *testing.T, variant helloWorldVariant, turns []string) helloWorldMetrics {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	referencePath := filepath.Join(root, "reference.txt")
	effectPath := filepath.Join(root, "corpus-effect.txt")
	const referenceToken = "REF-NEO-2040"
	if err := os.WriteFile(referencePath, []byte(referenceToken), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := tools.Spawn(ctx, tools.Options{ManifestPath: fsToolManifest(t, root), StderrSink: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if warnings := manager.Warnings(); len(warnings) != 0 {
		t.Fatalf("real filesystem MCP unavailable: %v", warnings)
	}
	readName := ""
	for _, schema := range manager.Schemas() {
		if readOnlyToolName(schema.Function.Name) && strings.Contains(schema.Function.Name, "read") {
			readName = schema.Function.Name
			break
		}
	}
	if readName == "" {
		t.Fatal("real filesystem MCP has no read operation")
	}

	journal := openRealAgentJournal(t)
	cfg := config.Default()
	cfg.DataRoot = filepath.Join(root, "Neocortex")
	cfg.NeocortexActor = "hello-world-" + variant.Name
	cfg.SessionCurrentIntent = variant.CurrentIntent
	cfg.SessionExactProjection = variant.Exact
	cfg.InteractionPosture = variant.Posture
	cfg.CassandraEnabled = false
	cfg.EpistemicPremises = false
	cfg.EpistemicPredictions = false
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pager.Close() })

	script := newHelloWorldScript(referencePath, effectPath, readName)
	model := httptest.NewServer(http.HandlerFunc(script.serve))
	t.Cleanup(model.Close)
	client, err := llm.New(mcllm.Config{
		Model: "accounts/fireworks/models/gpt-oss-120b", Provider: mcllm.ProviderFireworks,
		ProviderSet: true, GatewayURL: model.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	const conversationID = "hello-world-corpus"
	newAgent := func() *Agent {
		return New(Options{
			Config: cfg, Main: client, Cheap: client, Tools: manager, Pager: pager,
			ConvID: conversationID, Journal: journal,
			Observer: func(event ToolEvent) {
				if event.Phase == ToolStart && event.Name == "fs__write_file" {
					script.effectDispatches++
				}
			},
		})
	}
	a := newAgent()
	metrics := helloWorldMetrics{}
	for index, prompt := range turns {
		if index == 40 {
			previous := a
			a = newAgent()
			if variant.Exact {
				replay, replayErr := journal.Replay(ctx, conversationID)
				if replayErr != nil {
					t.Fatal(replayErr)
				}
				if _, replayErr = a.SeedReplay(replay); replayErr != nil {
					t.Fatal(replayErr)
				}
			} else {
				history := previous.ProtocolHistory()
				visible := make([]llm.Message, 0, len(history))
				for _, message := range history {
					if message.Role == llm.RoleUser || (message.Role == llm.RoleAssistant && len(message.ToolCalls) == 0) {
						visible = append(visible, message)
					}
				}
				if len(visible) > 16 {
					visible = visible[len(visible)-16:]
				}
				a.Seed(visible, previous.TurnObjective())
			}
		}
		beforeCalls := script.mainCalls
		started := time.Now()
		err := a.Chat(ctx, prompt)
		elapsed := time.Since(started)
		if index == 43 && !variant.Posture {
			if err == nil {
				t.Fatalf("%s baseline malformed call unexpectedly recovered locally", variant.Name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s turn %d: %v", variant.Name, index, err)
		}
		if index == 1 {
			metrics.StaleTaskResurrection = a.TurnObjective() != prompt
		}
		if index == 41 {
			metrics.IndirectReferenceCorrect = strings.Contains(script.lastAnswer, referenceToken)
		}
		if index == 44 {
			metrics.CriticalExecutionPassed = a.turn.runLedger != nil && a.TurnPosture() == PostureExecution
		}
		if index == 99 {
			metrics.CorrectionRetained = strings.Contains(script.lastAnswer, "SQLite")
		}
		if index != 20 && index != 41 && index != 42 && index != 43 && index != 44 {
			metrics.SimpleTurns++
			metrics.SimpleModelCalls += script.mainCalls - beforeCalls
			metrics.SimpleLatency += elapsed
		}
	}
	metrics.DuplicateSideEffects = script.effectDispatches - 1
	metrics.ProtocolRecoveryLocal = !variant.Posture || script.invalidRecovered && script.malformedRecovered
	if !variant.Posture {
		metrics.ProtocolRecoveryLocal = false
	}
	return metrics
}

type helloWorldScript struct {
	mu                 sync.Mutex
	referencePath      string
	effectPath         string
	readName           string
	mainCalls          int
	effectDispatches   int
	providerFailed     bool
	referenceIssued    bool
	invalidIssued      bool
	malformedIssued    bool
	effectIssued       bool
	invalidRecovered   bool
	malformedRecovered bool
	lastAnswer         string
}

func newHelloWorldScript(referencePath, effectPath, readName string) *helloWorldScript {
	return &helloWorldScript{referencePath: referencePath, effectPath: effectPath, readName: readName}
}

func (script *helloWorldScript) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := string(raw)
	script.mu.Lock()
	defer script.mu.Unlock()
	if strings.Contains(body, "stated expectation") {
		writeSSEAnswer(w, "match")
		return
	}
	script.mainCalls++
	if strings.Contains(body, "read the reference file") && !script.referenceIssued {
		script.referenceIssued = true
		args, _ := json.Marshal(map[string]string{"path": script.referencePath, "expect": "the exact reference token is returned"})
		writeSSEToolCall(w, fmt.Sprintf(`{"index":0,"id":"ref-read","type":"function","function":{"name":%q,"arguments":%q}}`, script.readName, string(args)))
		return
	}
	if strings.Contains(body, "invalid tool name corpus case") && !script.invalidIssued {
		script.invalidIssued = true
		writeSSEToolCall(w, `{"index":0,"id":"bad-name","type":"function","function":{"name":"fs__not_a_real_tool","arguments":"{}"}}`)
		return
	}
	if strings.Contains(body, "invalid_tool_name") || strings.Contains(body, "not available in this session") {
		script.invalidRecovered = true
	}
	if strings.Contains(body, "malformed argument corpus case") && !script.malformedIssued {
		script.malformedIssued = true
		writeSSEToolCall(w, `{"index":0,"id":"bad-json","type":"function","function":{"name":"fs__write_file","arguments":"{\"path\":"}}`)
		return
	}
	if strings.Contains(body, "invalid_tool_arguments") {
		script.malformedRecovered = true
	}
	if strings.Contains(body, "corpus effect file exactly once") && !script.effectIssued {
		script.effectIssued = true
		args, _ := json.Marshal(map[string]string{"path": script.effectPath, "content": "single corpus effect", "expect": "the file contains single corpus effect"})
		writeSSEToolCall(w, fmt.Sprintf(`{"index":0,"id":"effect-write","type":"function","function":{"name":"fs__write_file","arguments":%q}}`, string(args)))
		return
	}
	if strings.Contains(body, "effect-write") && !script.providerFailed {
		script.providerFailed = true
		http.Error(w, `{"error":{"message":"temporary corpus provider failure"}}`, http.StatusServiceUnavailable)
		return
	}
	answer := "acknowledged"
	switch {
	case strings.Contains(body, "what database did I finally choose"):
		if strings.Contains(body, "Correction: use SQLite") {
			answer = "You finally chose SQLite."
		} else if strings.Contains(body, "Decision: use PostgreSQL") {
			answer = "You chose PostgreSQL."
		} else {
			answer = "I no longer have that decision."
		}
	case strings.Contains(body, "what was the exact reference token"):
		if strings.Contains(body, "REF-NEO-2040") {
			answer = "The exact reference token was REF-NEO-2040."
		} else {
			answer = "I no longer have the exact reference token."
		}
	}
	script.lastAnswer = answer
	writeSSEAnswer(w, answer)
}
