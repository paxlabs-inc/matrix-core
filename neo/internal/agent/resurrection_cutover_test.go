// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	executortool "matrix/executor/tool"
	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
	"matrix/vault"
)

func TestAgentRuntimeFlagKeepsLegacyDefaultAndServesFrozenSeam(
	t *testing.T,
) {
	if got := config.Default().AgentRuntime; got != "legacy" {
		t.Fatalf("default runtime = %q", got)
	}
	unavailableConfig := config.Default()
	unavailableConfig.AgentRuntime = "resurrection"
	if err := New(Options{
		Config: unavailableConfig,
	}).Chat(t.Context(), "must fail closed"); err == nil ||
		!strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing resurrection runtime error = %v", err)
	}
	unsupportedConfig := config.Default()
	unsupportedConfig.AgentRuntime = "other"
	if err := New(Options{
		Config: unsupportedConfig,
	}).Chat(t.Context(), "must fail closed"); err == nil ||
		!strings.Contains(err.Error(), "unsupported runtime") {
		t.Fatalf("unsupported runtime error = %v", err)
	}
	var realTurns atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			var decoded struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			body, err := io.ReadAll(request.Body)
			if err != nil || json.Unmarshal(body, &decoded) != nil ||
				len(decoded.Messages) == 0 {
				http.Error(writer, "invalid request", http.StatusBadRequest)
				return
			}
			latest := decoded.Messages[len(decoded.Messages)-1].Content
			switch {
			case strings.Contains(latest, "Reply with READY"):
				writeCutoverText(writer, "READY")
			case strings.Contains(
				latest, "matrix_runtime_capability_echo",
			):
				writeCutoverTool(
					writer, "capability-call",
					"matrix_runtime_capability_echo",
					map[string]interface{}{
						"value": "READY", "expect": "returns READY",
					},
				)
			default:
				realTurns.Add(1)
				writeCutoverText(
					writer,
					"Resurrection delivered through the frozen Chat seam.",
				)
			}
		},
	))
	t.Cleanup(gateway.Close)
	manager := &tools.Manager{}
	cfg, pager, runtime := openCutoverRuntime(
		t, gateway.URL, manager,
	)
	reporter := &cutoverReporter{}
	agent := New(Options{
		Config: cfg, Tools: manager, Pager: pager,
		Reporter: reporter, Runtime: runtime,
	})
	if err := agent.Chat(
		t.Context(), "Prove the resurrection Chat seam.",
	); err != nil {
		t.Fatal(err)
	}
	if got := reporter.lastSay(); !strings.Contains(
		got, "Resurrection delivered",
	) {
		t.Fatalf("frozen Chat delivery = %q", got)
	}
	agent.Reset()
	transcript := make(chan AudioTranscript, 1)
	transcript <- AudioTranscript{Text: "Prove the voice turn."}
	var heard string
	if err := agent.ChatAudio(
		t.Context(), "[voice]", &AudioTurn{
			Ref: "media://voice", Transcript: transcript,
			OnTranscript: func(text string) { heard = text },
		},
	); err != nil {
		t.Fatal(err)
	}
	if realTurns.Load() != 2 ||
		!strings.Contains(heard, "Prove the voice turn.") ||
		agent.BestEffort() == "" {
		t.Fatalf(
			"runtime turns=%d transcript=%q best=%q",
			realTurns.Load(), heard, agent.BestEffort(),
		)
	}
}

func TestResurrectionRuntimeSurfacesHonestPartialAsTypedIncomplete(
	t *testing.T,
) {
	var taskCalls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			var decoded struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			body, err := io.ReadAll(request.Body)
			if err != nil || json.Unmarshal(body, &decoded) != nil ||
				len(decoded.Messages) == 0 {
				http.Error(writer, "invalid request", http.StatusBadRequest)
				return
			}
			latest := decoded.Messages[len(decoded.Messages)-1].Content
			switch {
			case strings.Contains(latest, "Reply with READY"):
				writeCutoverText(writer, "READY")
			case strings.Contains(
				latest, "matrix_runtime_capability_echo",
			):
				writeCutoverTool(
					writer, "capability-call",
					"matrix_runtime_capability_echo",
					map[string]interface{}{
						"value": "READY", "expect": "returns READY",
					},
				)
			default:
				taskCalls.Add(1)
				writeCutoverText(
					writer,
					"I already provided the answer above.",
				)
			}
		},
	))
	t.Cleanup(gateway.Close)
	manager := &tools.Manager{}
	cfg, pager, runtime := openCutoverRuntime(
		t, gateway.URL, manager,
	)
	reporter := &cutoverReporter{}
	agent := New(Options{
		Config: cfg, Tools: manager, Pager: pager,
		Reporter: reporter, Runtime: runtime,
	})
	err := agent.Chat(
		t.Context(), "Produce a complete bounded resurrection result.",
	)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Chat() error = %v, want ErrIncomplete", err)
	}
	if taskCalls.Load() != 3 {
		t.Fatalf("bounded repair calls = %d, want 3", taskCalls.Load())
	}
	if partial := reporter.lastPartial(); !strings.Contains(
		partial, "will not claim",
	) {
		t.Fatalf("honest partial = %q", partial)
	}
}

func openCutoverRuntime(
	t *testing.T,
	gatewayURL string,
	manager *tools.Manager,
) (config.Config, *memory.Pager, *ResurrectionRuntime) {
	t.Helper()
	t.Setenv("MATRIX_GATEWAY_TOKEN", "cutover-runtime-token")
	root := t.TempDir()
	session, err := vault.Boot(t.Context(), vault.Config{
		Required: true, DataDir: root,
		UserDID: "did:matrix:cutover-runtime",
		KEKHex: hex.EncodeToString(
			bytes.Repeat([]byte{0x47}, 32),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AgentRuntime = "resurrection"
	cfg.ActorDID = "did:matrix:cutover-runtime"
	cfg.MainModel = "mimo-v2"
	cfg.GatewayURL = gatewayURL
	cfg.RuntimeProvider.GatewayURL = gatewayURL
	cfg.RuntimeProvider.MaxAttempts = 1
	cfg.CortexRoot = filepath.Join(root, "cortex")
	cfg.CortexActor = "cutover-runtime"
	cfg.SelfModelGraph = filepath.Join(root, "self-model")
	cfg.Vault = session
	cfg.VaultUser = cfg.ActorDID
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pager.Close(); err != nil {
			t.Errorf("close pager: %v", err)
		}
	})
	runtime, err := OpenResurrectionRuntime(
		t.Context(), cfg, manager, pager,
		filepath.Join(root, "turnstate.db"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(context.Background()); err != nil {
			t.Errorf("close resurrection runtime: %v", err)
		}
	})
	return cfg, pager, runtime
}

func TestResurrectionRuntimeSurfaceRideAlongProbes(t *testing.T) {
	manager := cutoverProbeManager(t)
	var delegated, previewed, grounded, spawned atomic.Int32
	manager.SetDelegate(func(
		_ context.Context,
		intent string,
	) (string, error) {
		delegated.Add(1)
		if intent != "prove the secure delegation lane" {
			return "", fmt.Errorf("unexpected delegated intent %q", intent)
		}
		return "secure delegation completed", nil
	})
	manager.SetPreview(func(context.Context) (string, error) {
		previewed.Add(1)
		return "the workbench preview is starting", nil
	})
	manager.SetDesktop(
		nil,
		func(context.Context) (string, error) {
			grounded.Add(1)
			return "button Submit at 420,240", nil
		},
	)
	manager.SetSwarm(func(
		_ context.Context,
		specs []tools.SubagentSpec,
	) (string, error) {
		spawned.Add(1)
		if len(specs) != 2 {
			return "", fmt.Errorf(
				"sub-agent count = %d, want 2", len(specs),
			)
		}
		return "two scoped sub-agents completed", nil
	}, 4)

	gateway := cutoverProbeGateway(t)
	cfg, pager, runtime := openCutoverRuntime(
		t, gateway.URL, manager,
	)
	observer := &cutoverToolObserver{}
	reporter := &cutoverReporter{}
	interactive := New(Options{
		Config: cfg, Tools: manager, Pager: pager,
		Reporter: reporter, Runtime: runtime,
		Observer: observer.observe, ConvID: "cutover-interactive",
	})
	for _, prompt := range []string{
		"probe:workbench",
		"probe:dojo",
		"probe:core_execute",
		"probe:subagents",
	} {
		if err := interactive.Chat(t.Context(), prompt); err != nil {
			t.Fatalf("%s: %v", prompt, err)
		}
	}
	if delegated.Load() != 1 || previewed.Load() != 1 ||
		grounded.Load() != 1 || spawned.Load() != 1 {
		t.Fatalf(
			"ride-along calls: delegate=%d preview=%d desktop=%d swarm=%d",
			delegated.Load(), previewed.Load(),
			grounded.Load(), spawned.Load(),
		)
	}
	for _, name := range []string{
		tools.PreviewTool,
		tools.DesktopA11yTool,
		tools.CoreExecuteTool,
		tools.SpawnSubagentsTool,
	} {
		if !observer.committed(name) {
			t.Errorf("%s did not cross the committed tool observer", name)
		}
	}

	automatrixReporter := &cutoverReporter{}
	automatrix := NewAutomatrix(Options{
		Config: cfg, Tools: manager, Pager: pager,
		Reporter: automatrixReporter, Runtime: runtime,
		ConvID: "cutover-automatrix",
	})
	if err := tools.AssertNoValueTransferTools(
		automatrix.AdvertisedTools(),
	); err != nil {
		t.Fatal(err)
	}
	if err := automatrix.Chat(
		t.Context(), "probe:automatrix-wake",
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		automatrixReporter.lastSay(), "Automatrix ride-along complete",
	) {
		t.Fatalf(
			"automatrix delivery = %q",
			automatrixReporter.lastSay(),
		)
	}

	briefReporter := &cutoverReporter{}
	brief := NewMorningBrief(Options{
		Config: cfg, Tools: manager, Pager: pager,
		Reporter: briefReporter, Runtime: runtime,
		ConvID: "cutover-morning-brief",
	})
	if err := tools.AssertBriefSurface(
		brief.AdvertisedTools(),
	); err != nil {
		t.Fatal(err)
	}
	if err := brief.Chat(
		t.Context(), "probe:morning-brief",
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		briefReporter.lastSay(), "Morning brief ride-along complete",
	) {
		t.Fatalf(
			"morning brief delivery = %q",
			briefReporter.lastSay(),
		)
	}
}

func cutoverProbeManager(t *testing.T) *tools.Manager {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node is required for real tool binding: %v", err)
	}
	execBridge := filepath.Clean(
		"../../../tools/exec/exec.mjs",
	)
	if _, err := os.Stat(execBridge); err != nil {
		t.Fatalf("real exec bridge: %v", err)
	}
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	manifest := executortool.AgentManifest{
		SchemaVersion: 1,
		Agent:         "matrix://agent/resurrection-cutover-probe",
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
	manager, err := tools.Spawn(t.Context(), tools.Options{
		ManifestPath: manifestPath,
		SpawnTimeout: 20 * time.Second,
		StderrSink:   io.Discard,
		EscalatePatterns: []string{
			"service_stop",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if warnings := manager.Warnings(); len(warnings) != 0 {
		_ = manager.Close()
		t.Fatalf("real exec bridge warnings: %v", warnings)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close probe manager: %v", err)
		}
	})
	return manager
}

func cutoverProbeGateway(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(
					writer, err.Error(), http.StatusBadRequest,
				)
				return
			}
			var decoded struct {
				Messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
				Tools []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil ||
				len(decoded.Messages) == 0 {
				http.Error(
					writer, "invalid request", http.StatusBadRequest,
				)
				return
			}
			latest := decoded.Messages[len(decoded.Messages)-1]
			content := string(latest.Content)
			var textContent string
			if json.Unmarshal(latest.Content, &textContent) == nil {
				content = textContent
			}
			hasToolResult := false
			probe := ""
			for _, message := range decoded.Messages {
				if message.Role == "tool" {
					hasToolResult = true
				}
				var messageContent string
				if json.Unmarshal(
					message.Content, &messageContent,
				) == nil {
					switch strings.TrimSpace(messageContent) {
					case "probe:workbench",
						"probe:dojo",
						"probe:core_execute",
						"probe:subagents",
						"probe:automatrix-wake",
						"probe:morning-brief":
						probe = strings.TrimSpace(messageContent)
					}
				}
			}
			if probe == "probe:automatrix-wake" {
				for _, tool := range decoded.Tools {
					switch tool.Function.Name {
					case tools.CoreExecuteTool,
						tools.SpawnSubagentsTool,
						tools.PreviewTool,
						tools.DesktopLookTool,
						tools.DesktopA11yTool:
						http.Error(
							writer,
							"automatrix received forbidden tool "+
								tool.Function.Name,
							http.StatusBadRequest,
						)
						return
					}
				}
			}
			if probe == "probe:morning-brief" {
				for _, tool := range decoded.Tools {
					if tool.Function.Name != tools.MemoryRecallTool {
						http.Error(
							writer,
							"morning brief received forbidden tool "+
								tool.Function.Name,
							http.StatusBadRequest,
						)
						return
					}
				}
			}
			switch {
			case strings.Contains(content, "Reply with READY"):
				writeCutoverText(writer, "READY")
			case strings.Contains(
				content, "matrix_runtime_capability_echo",
			):
				writeCutoverTool(
					writer, "capability-call",
					"matrix_runtime_capability_echo",
					map[string]interface{}{
						"value": "READY", "expect": "returns READY",
					},
				)
			case hasToolResult:
				writeCutoverText(
					writer, cutoverProbeFinal(probe),
				)
			case probe == "probe:workbench":
				writeCutoverTool(
					writer, "workbench-call", tools.PreviewTool,
					map[string]interface{}{
						"expect": "the preview starts",
					},
				)
			case probe == "probe:dojo":
				writeCutoverTool(
					writer, "dojo-call", tools.DesktopA11yTool,
					map[string]interface{}{
						"expect": "an accessibility tree",
					},
				)
			case probe == "probe:core_execute":
				writeCutoverTool(
					writer, "core-call", tools.CoreExecuteTool,
					map[string]interface{}{
						"intent": "prove the secure delegation lane",
						"expect": "a verified secure outcome",
					},
				)
			case probe == "probe:subagents":
				writeCutoverTool(
					writer, "swarm-call", tools.SpawnSubagentsTool,
					map[string]interface{}{
						"agents": []interface{}{
							map[string]interface{}{
								"name": "Reader", "persona": "reader",
								"task": "read the first independent surface",
							},
							map[string]interface{}{
								"name": "Reviewer", "persona": "reviewer",
								"task": "review the second independent surface",
							},
						},
						"expect": "two scoped results",
					},
				)
			case probe == "probe:automatrix-wake":
				writeCutoverText(
					writer, "Automatrix ride-along complete.",
				)
			case probe == "probe:morning-brief":
				writeCutoverText(
					writer, "Morning brief ride-along complete.",
				)
			default:
				http.Error(
					writer, "unknown probe", http.StatusBadRequest,
				)
			}
		},
	))
	t.Cleanup(server.Close)
	return server
}

func cutoverProbeFinal(probe string) string {
	switch probe {
	case "probe:workbench":
		return "Workbench ride-along complete."
	case "probe:dojo":
		return "Dojo desktop ride-along complete."
	case "probe:core_execute":
		return "Secure core execution ride-along complete."
	case "probe:subagents":
		return "Sub-agent ride-along complete."
	default:
		return "Ride-along tool completed."
	}
}

type cutoverToolObserver struct {
	mu     sync.Mutex
	events []ToolEvent
}

func (observer *cutoverToolObserver) observe(event ToolEvent) {
	observer.mu.Lock()
	observer.events = append(observer.events, event)
	observer.mu.Unlock()
}

func (observer *cutoverToolObserver) committed(name string) bool {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	for _, event := range observer.events {
		if event.Name == name && event.Phase == ToolEnd &&
			!event.IsErr {
			return true
		}
	}
	return false
}

type cutoverReporter struct {
	mu       sync.Mutex
	says     []string
	partials []string
}

func (reporter *cutoverReporter) Say(text string, _ bool) {
	reporter.mu.Lock()
	reporter.says = append(reporter.says, text)
	reporter.mu.Unlock()
}

func (reporter *cutoverReporter) SayHonestPartial(text string) {
	reporter.mu.Lock()
	reporter.partials = append(reporter.partials, text)
	reporter.mu.Unlock()
}

func (*cutoverReporter) Status(string)             {}
func (*cutoverReporter) Progress(string)           {}
func (*cutoverReporter) Notice(string)             {}
func (*cutoverReporter) Think(string)              {}
func (*cutoverReporter) Delta(int, string, string) {}

func (reporter *cutoverReporter) lastSay() string {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if len(reporter.says) == 0 {
		return ""
	}
	return reporter.says[len(reporter.says)-1]
}

func (reporter *cutoverReporter) lastPartial() string {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if len(reporter.partials) == 0 {
		return ""
	}
	return reporter.partials[len(reporter.partials)-1]
}

func writeCutoverText(writer http.ResponseWriter, content string) {
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

func writeCutoverTool(
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
