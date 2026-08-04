package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"matrix/neo/internal/tools"
)

func replayFetchManager(t *testing.T) *tools.Manager {
	t.Helper()
	manifest := filepath.Join(t.TempDir(), "agent.json")
	doc := `{"schema_version":1,"agent":"matrix://agent/replay","allowed_side_effects":["network"],"servers":[{"alias":"fetch","transport":"stdio","command":"uvx","args":["--offline","--from","mcp-server-fetch==2025.4.7","--with","mcp==1.29.0","mcp-server-fetch"],"env":[],"package_digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","version":"2025.4.7","tools":[{"name":"fetch","description":"Fetch a URL","side_effect_class":"network","timeout_ms":30000}]}]}`
	if err := os.WriteFile(manifest, []byte(doc), 0644); err != nil {
		t.Fatal(err)
	}
	manager, err := tools.Spawn(context.Background(), tools.Options{ManifestPath: manifest, StderrSink: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if warnings := manager.Warnings(); len(warnings) != 0 {
		_ = manager.Close()
		t.Fatalf("real fetch tool unavailable: %v", warnings)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func sseChatServer(t *testing.T, answer string, lastBody *[]byte, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*lastBody = body
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		frame := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": answer}}}}
		encoded, _ := json.Marshal(frame)
		fmt.Fprintf(w, "data: %s\n", encoded)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(server.Close)
	return server
}
