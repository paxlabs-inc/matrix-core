package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/paxlabs-inc/tachyon-tools/internal/engine"
	"github.com/paxlabs-inc/tachyon-tools/pkg/rpc"
	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

// RunStdio serves MCP over newline-delimited JSON-RPC on stdin/stdout.
func RunStdio(eng *engine.Engine) error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      any             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			send(rpcErr(nil, -32700, err.Error()))
			continue
		}
		resp := handle(eng, req.Method, req.Params, req.ID)
		if resp != nil {
			send(resp)
		}
	}
	return scanner.Err()
}

func handle(eng *engine.Engine, method string, params json.RawMessage, id any) map[string]any {
	switch method {
	case "initialize":
		return rpcOk(id, map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]string{"name": "tachyon-tools", "version": engine.Version},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		})
	case "tools/list":
		return rpcOk(id, map[string]any{"tools": Tools()})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return rpcErr(id, -32602, err.Error())
		}
		result, err := callTool(context.Background(), eng, p.Name, p.Arguments)
		if err != nil {
			return rpcOk(id, map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			})
		}
		b, _ := json.Marshal(result)
		return rpcOk(id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(b)}},
		})
	case "notifications/initialized", "ping":
		return nil
	default:
		if id == nil {
			return nil
		}
		return rpcErr(id, -32601, "method not found: "+method)
	}
}

func callTool(ctx context.Context, eng *engine.Engine, name string, args json.RawMessage) (any, error) {
	rpcMethod := name
	if name == "tachyon_chain_list" {
		return eng.ChainList(), nil
	}
	result, rpcErr := rpc.DispatchForMCP(ctx, eng, rpcMethod, args)
	if rpcErr != nil {
		return nil, fmt.Errorf("%s", rpcErr.Message)
	}
	return result, nil
}

func send(v any) {
	b, _ := json.Marshal(v)
	_, _ = os.Stdout.Write(append(b, '\n'))
	_ = os.Stdout.Sync() // MCP stdio clients block until each NDJSON line is flushed.
}

func rpcOk(id, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcErr(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

// Selftest verifies tool registry matches docs list.
func Selftest() error {
	tools := Tools()
	if len(tools) != len(ToolNames) {
		return fmt.Errorf("tool count mismatch: got %d want %d", len(tools), len(ToolNames))
	}
	names := map[string]struct{}{}
	for _, t := range tools {
		names[t.Name] = struct{}{}
	}
	for _, want := range ToolNames {
		if _, ok := names[want]; !ok {
			return fmt.Errorf("missing tool %q", want)
		}
	}
	return nil
}

// FormatToolError returns MCP isError content for structured failures.
func FormatToolError(name string, err *types.Error) string {
	b, _ := json.Marshal(map[string]any{"ok": false, "tool": name, "error": err})
	return string(b)
}
