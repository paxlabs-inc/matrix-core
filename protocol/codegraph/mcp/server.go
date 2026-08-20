// Package mcp serves the CodeGraph L4 retrieval tools over MCP: newline-
// delimited JSON-RPC 2.0 on stdio, matching the repo convention (protocol
// 2024-11-05). Every tool result is a compact .kvx fragment passed through
// guardFragment before it leaves the process, so raw source can never cross the
// tool boundary.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"centra/protocol/codegraph/model"
	"centra/protocol/codegraph/retrieve"
	"centra/protocol/codegraph/store"
)

const protocolVersion = "2024-11-05"

// tool is one registered MCP tool over the retrieval surface.
type tool struct {
	Name        string
	Description string
	Schema      map[string]any
	call        func(args json.RawMessage) (string, error)
}

// Server serves the CodeGraph retrieval tools over MCP. Construct it with New,
// then drive it with Serve. Tools are held in a fixed registration order so
// tools/list is deterministic.
type Server struct {
	api   *retrieve.API
	tools []tool
}

// New builds a Server over an in-memory graph Index and registers the retrieval
// tools that are currently implemented (symbol_lookup, neighbors, impact).
// Additional tools (diff, subgraph, search) register here as they land.
func New(ix *model.Index) *Server {
	s := &Server{api: retrieve.New(ix)}

	s.tools = append(s.tools, tool{
		Name:        "symbol_lookup",
		Description: "Locate a symbol by name (exact, then fuzzy). Returns a .kvx NODE fragment with location — never raw source.",
		Schema: object(props{
			"name": prop("string", "symbol name to look up"),
			"kind": prop("string", "optional node kind filter (func, method, type, interface, const, var, field, package, file, module, repo)"),
		}, "name"),
		call: func(args json.RawMessage) (string, error) {
			var p struct {
				Name string `json:"name"`
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return "", err
			}
			if p.Name == "" {
				return "", fmt.Errorf("name is required")
			}
			return s.api.SymbolLookup(p.Name, model.Kind(p.Kind)), nil
		},
	})

	s.tools = append(s.tools, tool{
		Name:        "neighbors",
		Description: "Return the bounded subgraph around a node id over an edge type (or all) in a direction, to depth <= 3. Depth > 3 is rejected.",
		Schema: object(props{
			"id":        prop("string", "node id"),
			"edge_type": prop("string", "edge type to traverse (contains, imports, calls, implements, embeds, references, defines, tests); empty = all"),
			"direction": prop("string", "out | in | both (default both)"),
			"depth":     prop("integer", "traversal depth, 1..3 (default 1)"),
		}, "id"),
		call: func(args json.RawMessage) (string, error) {
			var p struct {
				Id        string `json:"id"`
				EdgeType  string `json:"edge_type"`
				Direction string `json:"direction"`
				Depth     int    `json:"depth"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return "", err
			}
			if p.Id == "" {
				return "", fmt.Errorf("id is required")
			}
			if p.Depth == 0 {
				p.Depth = 1
			}
			return s.api.Neighbors(p.Id, model.EdgeType(p.EdgeType), parseDir(p.Direction), p.Depth)
		},
	})

	s.tools = append(s.tools, tool{
		Name:        "impact",
		Description: "Return the affected node set of a change to id: the reverse-edge transitive closure over calls, references, and implements.",
		Schema: object(props{
			"id":        prop("string", "node id whose change impact to assess"),
			"max_depth": prop("integer", "optional closure depth bound (default full closure)"),
		}, "id"),
		call: func(args json.RawMessage) (string, error) {
			var p struct {
				Id       string `json:"id"`
				MaxDepth int    `json:"max_depth"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return "", err
			}
			if p.Id == "" {
				return "", fmt.Errorf("id is required")
			}
			return s.api.Impact(p.Id, p.MaxDepth), nil
		},
	})

	s.tools = append(s.tools, tool{
		Name:        "diff",
		Description: "Compare two graph stores by node digest and edge set. Returns added/removed/changed node ids and per-source edge deltas as a .kvx fragment.",
		Schema: object(props{
			"a": prop("string", "base graph store directory (rev_a)"),
			"b": prop("string", "new graph store directory (rev_b)"),
		}, "a", "b"),
		call: func(args json.RawMessage) (string, error) {
			var p struct {
				A string `json:"a"`
				B string `json:"b"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return "", err
			}
			if p.A == "" || p.B == "" {
				return "", fmt.Errorf("a and b store directories are required")
			}
			ixA, _, err := store.Load(p.A)
			if err != nil {
				return "", fmt.Errorf("load a: %w", err)
			}
			ixB, _, err := store.Load(p.B)
			if err != nil {
				return "", fmt.Errorf("load b: %w", err)
			}
			return retrieve.Diff(ixA, ixB), nil
		},
	})

	return s
}

// ToolNames returns the registered tool names in registration order.
func (s *Server) ToolNames() []string {
	out := make([]string, len(s.tools))
	for i, t := range s.tools {
		out[i] = t.Name
	}
	return out
}

// Serve reads newline-delimited JSON-RPC requests from in and writes responses
// to out until in is exhausted.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	bw := bufio.NewWriter(out)
	for scanner.Scan() {
		line := scanner.Bytes()
		if strings.TrimSpace(string(line)) == "" {
			continue
		}
		resp := s.handle(line)
		if resp != nil {
			b, _ := json.Marshal(resp)
			bw.Write(b)
			bw.WriteByte('\n')
			bw.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return bw.Flush()
}

func (s *Server) handle(line []byte) map[string]any {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return rpcErr(nil, -32700, err.Error())
	}
	switch req.Method {
	case "initialize":
		return rpcOk(req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"serverInfo":      map[string]string{"name": "codegraph", "version": "0.1.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		})
	case "tools/list":
		return rpcOk(req.ID, map[string]any{"tools": s.toolList()})
	case "tools/call":
		return s.callTool(req.ID, req.Params)
	case "notifications/initialized", "ping":
		return nil
	default:
		if req.ID == nil {
			return nil
		}
		return rpcErr(req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) toolList() []map[string]any {
	out := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.Schema,
		})
	}
	return out
}

func (s *Server) callTool(id any, params json.RawMessage) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return rpcErr(id, -32602, err.Error())
	}
	for _, t := range s.tools {
		if t.Name != p.Name {
			continue
		}
		frag, err := t.call(p.Arguments)
		if err != nil {
			return toolError(id, err.Error())
		}
		// Fragments-not-source: never let a result that is not a well-formed
		// fragment reach the agent.
		if err := guardFragment(frag); err != nil {
			return toolError(id, "guard: "+err.Error())
		}
		return rpcOk(id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": frag}},
		})
	}
	return toolError(id, "unknown tool: "+p.Name)
}

func toolError(id any, msg string) map[string]any {
	return rpcOk(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	})
}

func parseDir(s string) model.Direction {
	switch s {
	case "out":
		return model.Out
	case "in":
		return model.In
	default:
		return model.Both
	}
}

func rpcOk(id, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcErr(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

// --- tiny JSON-schema helpers ---

type props map[string]map[string]any

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

func object(p props, required ...string) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any(mapProps(p)),
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func mapProps(p props) map[string]any {
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}
