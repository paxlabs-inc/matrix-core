package loop

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	executortool "matrix/executor/tool"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/provider"
	"matrix/neo/internal/runtime/turnstate"
	neotools "matrix/neo/internal/tools"
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

func handleCapabilityCanary(writer http.ResponseWriter, request gatewayRequest) bool {
	found := false
	for _, tool := range request.Tools {
		if tool.Function.Name == "matrix_runtime_capability_echo" {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	latest := request.Messages[len(request.Messages)-1].Content
	if strings.Contains(latest, "Reply with READY") {
		writeSSEText(writer, "READY")
		return true
	}
	writeSSETool(writer, "capability-call", "matrix_runtime_capability_echo", map[string]interface{}{"value": "READY", "expect": "returns READY"})
	return true
}

func writeSSEText(writer http.ResponseWriter, content string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	payload, _ := json.Marshal(map[string]interface{}{"model": "mimo-v2", "choices": []interface{}{map[string]interface{}{"index": 0, "delta": map[string]interface{}{"content": content}, "finish_reason": "stop"}}, "usage": map[string]interface{}{"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7}})
	fmt.Fprintf(writer, "data: %s\n\n", payload)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}

func writeSSETool(writer http.ResponseWriter, id, name string, arguments map[string]interface{}) {
	writer.Header().Set("Content-Type", "text/event-stream")
	raw, _ := json.Marshal(arguments)
	payload, _ := json.Marshal(map[string]interface{}{"model": "mimo-v2", "choices": []interface{}{map[string]interface{}{"index": 0, "delta": map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"index": 0, "id": id, "type": "function", "function": map[string]interface{}{"name": name, "arguments": string(raw)}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]interface{}{"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7}})
	fmt.Fprintf(writer, "data: %s\n\n", payload)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}

func (r *recordingReporter) Delta(turn int, channel, text string) {
	r.mu.Lock()
	r.deltas = append(r.deltas, recordedDelta{turn, channel, text})
	r.mu.Unlock()
}
func (r *recordingReporter) snapshot() []recordedDelta {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedDelta(nil), r.deltas...)
}
func (r *recordingReporter) Say(content string, _ bool) {
	r.mu.Lock()
	r.says = append(r.says, content)
	r.mu.Unlock()
}
func (r *recordingReporter) said() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.says...)
}

func realMiMoGenerator(t *testing.T, gatewayURL string) *provider.MiMoGenerator {
	t.Helper()
	t.Setenv("MATRIX_GATEWAY_TOKEN", "test-gateway-token")
	adapter := &provider.MiMoAdapter{}
	client, err := provider.New(adapter, provider.Config{GatewayURL: gatewayURL, BearerEnv: "MATRIX_GATEWAY_TOKEN", ActorDID: "did:matrix:runtime-loop-test", MaxAttempts: 3, BackoffInitial: time.Millisecond, BackoffMax: 2 * time.Millisecond, IdleTimeout: 10 * time.Second})
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
		t.Fatalf("node is required: %v", err)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test source")
	}
	bridge := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "tools", "exec", "exec.mjs"))
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	manifest := executortool.AgentManifest{SchemaVersion: 1, Agent: "matrix://agent/resurrection-loop-test", Servers: []executortool.ServerEntry{{Alias: "exec", Transport: "stdio", Command: "node", Args: []string{bridge}, PackageDigest: "sha256:" + strings.Repeat("c", 64), Version: "0.1.0", Tools: []executortool.ToolEntry{{Name: "shell", SideEffectClass: executortool.SideEffectShell, TimeoutMs: 10000}, {Name: "service_start", SideEffectClass: executortool.SideEffectShell, TimeoutMs: 10000}, {Name: "service_list", SideEffectClass: executortool.SideEffectRead, TimeoutMs: 10000}, {Name: "service_logs", SideEffectClass: executortool.SideEffectRead, TimeoutMs: 10000}, {Name: "service_stop", SideEffectClass: executortool.SideEffectShell, TimeoutMs: 10000}, {Name: "service_restart", SideEffectClass: executortool.SideEffectShell, TimeoutMs: 10000}}}}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := neotools.Spawn(t.Context(), neotools.Options{ManifestPath: manifestPath, SpawnTimeout: 20 * time.Second, StderrSink: io.Discard})
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

func realNativeManager(t *testing.T) (*neotools.Manager, string) {
	t.Helper()
	root := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	manifest := executortool.AgentManifest{
		SchemaVersion: 1,
		Agent:         "matrix://agent/resurrection-native-test",
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := neotools.Spawn(t.Context(), neotools.Options{
		ManifestPath:   manifestPath,
		StderrSink:     io.Discard,
		NativeRoot:     root,
		NativeStateDir: filepath.Join(t.TempDir(), "services"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if warnings := manager.Warnings(); len(warnings) != 0 {
		_ = manager.Close()
		t.Fatalf("native manager warnings: %v", warnings)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, root
}

func realTurnStore(t *testing.T, turnID, userContent string) *turnstate.Store {
	t.Helper()
	dir := t.TempDir()
	session, err := vault.Boot(t.Context(), vault.Config{Required: true, DataDir: dir, UserDID: "did:matrix:runtime-loop-test", KEKHex: hex.EncodeToString(bytes.Repeat([]byte{0x5a}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	store, err := turnstate.Open(t.Context(), filepath.Join(dir, "turnstate.db"), session)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTurnState(t.Context(), turnstate.TurnState{TurnID: turnID, ActorID: "runtime-loop-test", SessionID: "runtime-loop-session", Content: userContent, Status: turnstate.StatusRunning, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = store.Close(ctx)
	})
	return store
}

func roleSequence(messages []protocol.Message) string {
	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, string(m.Role))
	}
	return strings.Join(roles, ",")
}
