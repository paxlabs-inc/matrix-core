// Package mcp implements a Model Context Protocol server (JSON-RPC 2.0 over stdio,
// protocol version 2024-11-05) that exposes CodeGraph Ultra retrieval tools.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"codegraph-ultra/internal/model"
	"codegraph-ultra/internal/store"
)

const (
	protocolVersion = "2024-11-05"
	serverName      = "codegraph-ultra"
	serverVersion   = "0.1.0"
)

// allKinds is a deterministic iteration order for stats display.
var allKinds = []model.Kind{
	model.KindRepo, model.KindModule, model.KindPackage, model.KindFile,
	model.KindFunc, model.KindMethod, model.KindType, model.KindInterface,
	model.KindConst, model.KindVar, model.KindField, model.KindClass,
	model.KindEnum, model.KindTrait, model.KindStruct,
}

// Server is an MCP server backed by a CodeGraph store.
type Server struct {
	db     *store.DB
	reader *bufio.Reader
	writer io.Writer
}

// New creates an MCP server that reads from r and writes to w.
func New(db *store.DB, r io.Reader, w io.Writer) *Server {
	return &Server{
		db:     db,
		reader: bufio.NewReader(r),
		writer: w,
	}
}

// RunStdio is a convenience constructor that uses stdin/stdout.
func RunStdio(db *store.DB) error {
	s := New(db, os.Stdin, os.Stdout)
	return s.Run()
}

// ── JSON-RPC 2.0 types ──────────────────────────────────────────────

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ── MCP protocol types ──────────────────────────────────────────────

type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      serverInfo         `json:"serverInfo"`
}

type serverCapabilities struct {
	Tools toolsCapability `json:"tools"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type textContent struct {
	Type string `json:"type"` // always "text"
	Text string `json:"text"`
}

type toolCallResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ── tool argument structs ───────────────────────────────────────────

type symbolLookupArgs struct {
	ID string `json:"id"`
}

type neighborsArgs struct {
	ID    string `json:"id"`
	Type  string `json:"type,omitempty"`
	Depth int    `json:"depth,omitempty"`
}

type impactArgs struct {
	ID        string `json:"id"`
	MaxDepth  int    `json:"max_depth,omitempty"`
}

type searchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

type subgraphArgs struct {
	IDs       []string `json:"ids"`
	EdgeTypes []string `json:"edge_types,omitempty"`
}

// ── Run loop ────────────────────────────────────────────────────────

// Run reads JSON-RPC messages from the input and writes responses until EOF.
func (s *Server) Run() error {
	for {
		line, err := s.reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		line = trimWhitespace(line)
		if len(line) == 0 {
			continue
		}

		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeError(nil, -32700, "parse error: "+err.Error())
			continue
		}

		s.dispatch(&req)
	}
}

func trimWhitespace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func (s *Server) dispatch(req *jsonrpcRequest) {
	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
	case "notifications/initialized":
		// notification — no response
	case "tools/list":
		s.handleToolsList(req)
	case "tools/call":
		s.handleToolsCall(req)
	case "ping":
		s.writeResult(req.ID, map[string]any{})
	default:
		s.writeError(req.ID, -32601, "method not found: "+req.Method)
	}
}

// ── handlers ────────────────────────────────────────────────────────

func (s *Server) handleInitialize(req *jsonrpcRequest) {
	s.writeResult(req.ID, initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities:    serverCapabilities{Tools: toolsCapability{}},
		ServerInfo:      serverInfo{Name: serverName, Version: serverVersion},
	})
}

func (s *Server) handleToolsList(req *jsonrpcRequest) {
	tools := []toolDef{
		{
			Name:        "symbol_lookup",
			Description: "Look up a symbol by its fully-qualified node ID and return its .kvx fragment.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Node ID"}},"required":["id"]}`),
		},
		{
			Name:        "neighbors",
			Description: "Get the bounded subgraph around a node (BFS to a given depth along a specific edge type or all edges).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Center node ID"},"type":{"type":"string","description":"Edge type filter (imports, calls, implements, etc.). Omit for all."},"depth":{"type":"integer","description":"BFS depth (default 1)"}},"required":["id"]}`),
		},
		{
			Name:        "impact",
			Description: "Reverse transitive closure — which symbols are affected if the given node changes (callers, references, implementors).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Node to evaluate impact for"},"max_depth":{"type":"integer","description":"Max reverse traversal depth (default 64)"}},"required":["id"]}`),
		},
		{
			Name:        "search",
			Description: "Full-text search across node IDs, names, docs, sigs, and summaries.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search query"},"limit":{"type":"integer","description":"Max results (default 20)"}},"required":["query"]}`),
		},
		{
			Name:        "stats",
			Description: "Return aggregate graph statistics (node/edge counts, languages, breakdown by kind/type).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "subgraph",
			Description: "Fetch a subgraph of specific node IDs with all interconnecting edges, returning .kvx formatted fragments.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"ids":{"type":"array","items":{"type":"string"},"description":"Node IDs to include"},"edge_types":{"type":"array","items":{"type":"string"},"description":"Optional edge type filters"}},"required":["ids"]}`),
		},
	}
	s.writeResult(req.ID, map[string]any{"tools": tools})
}

func (s *Server) handleToolsCall(req *jsonrpcRequest) {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}

	var result toolCallResult
	var err error

	switch params.Name {
	case "symbol_lookup":
		result, err = s.toolSymbolLookup(params.Arguments)
	case "neighbors":
		result, err = s.toolNeighbors(params.Arguments)
	case "impact":
		result, err = s.toolImpact(params.Arguments)
	case "search":
		result, err = s.toolSearch(params.Arguments)
	case "stats":
		result, err = s.toolStats()
	case "subgraph":
		result, err = s.toolSubgraph(params.Arguments)
	default:
		s.writeError(req.ID, -32601, "unknown tool: "+params.Name)
		return
	}

	if err != nil {
		s.writeResult(req.ID, toolCallResult{
			Content: []textContent{{Type: "text", Text: "error: " + err.Error()}},
			IsError: true,
		})
		return
	}
	s.writeResult(req.ID, result)
}

// ── tool implementations ────────────────────────────────────────────

func (s *Server) toolSymbolLookup(raw json.RawMessage) (toolCallResult, error) {
	var args symbolLookupArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolCallResult{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.ID == "" {
		return toolCallResult{}, fmt.Errorf("id is required")
	}

	n := s.db.GetNode(args.ID)
	if n == nil {
		return toolCallResult{
			Content: []textContent{{Type: "text", Text: fmt.Sprintf("node %q not found", args.ID)}},
		}, nil
	}
	return singleNodeResult(n), nil
}

func (s *Server) toolNeighbors(raw json.RawMessage) (toolCallResult, error) {
	var args neighborsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolCallResult{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.ID == "" {
		return toolCallResult{}, fmt.Errorf("id is required")
	}
	if args.Depth <= 0 {
		args.Depth = 1
	}

	nodes := s.db.Neighbors(args.ID, model.EdgeType(args.Type), args.Depth)
	if len(nodes) == 0 {
		return toolCallResult{
			Content: []textContent{{Type: "text", Text: fmt.Sprintf("no neighbors for %q", args.ID)}},
		}, nil
	}
	return nodesResult(nodes), nil
}

func (s *Server) toolImpact(raw json.RawMessage) (toolCallResult, error) {
	var args impactArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolCallResult{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.ID == "" {
		return toolCallResult{}, fmt.Errorf("id is required")
	}
	if args.MaxDepth <= 0 {
		args.MaxDepth = 64
	}

	nodes := s.db.Impact(args.ID, args.MaxDepth)
	if len(nodes) == 0 {
		return toolCallResult{
			Content: []textContent{{Type: "text", Text: fmt.Sprintf("no impact for %q", args.ID)}},
		}, nil
	}
	return nodesResult(nodes), nil
}

func (s *Server) toolSearch(raw json.RawMessage) (toolCallResult, error) {
	var args searchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolCallResult{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Query == "" {
		return toolCallResult{}, fmt.Errorf("query is required")
	}
	if args.Limit <= 0 {
		args.Limit = 20
	}

	nodes := s.db.Search(args.Query, args.Limit)
	if len(nodes) == 0 {
		return toolCallResult{
			Content: []textContent{{Type: "text", Text: "no results"}},
		}, nil
	}
	return nodesResult(nodes), nil
}

func (s *Server) toolStats() (toolCallResult, error) {
	st := s.db.Stats()
	var b strings.Builder
	fmt.Fprintf(&b, "NODES  total=%d\n", st.TotalNodes)
	fmt.Fprintf(&b, "EDGES  total=%d\n", st.TotalEdges)
	fmt.Fprintf(&b, "FILES  count=%d\n", st.FilesCount)
	fmt.Fprintf(&b, "LANGS  %s\n", strings.Join(st.Languages, ", "))

	b.WriteString("\nNODES BY KIND:\n")
	for _, k := range allKinds {
		if c, ok := st.NodesByKind[k]; ok {
			fmt.Fprintf(&b, "  %-12s %d\n", k, c)
		}
	}

	b.WriteString("\nEDGES BY TYPE:\n")
	for _, et := range model.EdgeTypes {
		if c, ok := st.EdgesByType[et]; ok {
			fmt.Fprintf(&b, "  %-14s %d\n", et, c)
		}
	}

	return toolCallResult{
		Content: []textContent{{Type: "text", Text: b.String()}},
	}, nil
}

func (s *Server) toolSubgraph(raw json.RawMessage) (toolCallResult, error) {
	var args subgraphArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolCallResult{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if len(args.IDs) == 0 {
		return toolCallResult{}, fmt.Errorf("ids is required")
	}

	// Collect the requested nodes.
	idSet := make(map[string]bool, len(args.IDs))
	for _, id := range args.IDs {
		idSet[id] = true
	}

	// Optionally filter edge types.
	etSet := make(map[model.EdgeType]bool, len(args.EdgeTypes))
	for _, et := range args.EdgeTypes {
		etSet[model.EdgeType(et)] = true
	}

	var nodes []*model.Node
	for _, id := range args.IDs {
		if n := s.db.GetNode(id); n != nil {
			nodes = append(nodes, n)
		}
	}

	// Collect interconnecting edges between the requested nodes.
	var edges []model.Edge
	allEdges := s.db.Edges("")
	for _, e := range allEdges {
		if idSet[e.Src] && idSet[e.Dst] {
			if len(etSet) > 0 && !etSet[e.Type] {
				continue
			}
			edges = append(edges, e)
		}
	}

	var b strings.Builder
	for _, n := range nodes {
		b.WriteString(formatNode(n))
		b.WriteByte('\n')
	}
	for _, e := range edges {
		fmt.Fprintf(&b, "EDGE src=%s dst=%s type=%s\n", e.Src, e.Dst, e.Type)
	}

	return toolCallResult{
		Content: []textContent{{Type: "text", Text: b.String()}},
	}, nil
}

// ── .kvx formatting helpers ─────────────────────────────────────────

func formatNode(n *model.Node) string {
	loc := n.File
	if n.Range.StartLine > 0 {
		loc += ":" + n.Range.String()
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("id=%s", n.ID))
	parts = append(parts, fmt.Sprintf("kind=%s", n.Kind))
	parts = append(parts, fmt.Sprintf("loc=%s", loc))
	if n.Sig != "" {
		parts = append(parts, fmt.Sprintf("sig=%s", n.Sig))
	}
	if n.Enrich.Summary != "" {
		parts = append(parts, fmt.Sprintf("summary=%s", n.Enrich.Summary))
	} else if n.Doc != "" {
		parts = append(parts, fmt.Sprintf("doc=%s", firstLine(n.Doc)))
	}
	return "NODE " + strings.Join(parts, " ")
}

func singleNodeResult(n *model.Node) toolCallResult {
	return toolCallResult{
		Content: []textContent{{Type: "text", Text: formatNode(n)}},
	}
}

func nodesResult(nodes []*model.Node) toolCallResult {
	var b strings.Builder
	for i, n := range nodes {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(formatNode(n))
	}
	return toolCallResult{
		Content: []textContent{{Type: "text", Text: b.String()}},
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ── JSON-RPC helpers ────────────────────────────────────────────────

func (s *Server) writeResult(id json.RawMessage, result any) {
	resp := jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result}
	s.writeJSON(resp)
}

func (s *Server) writeError(id json.RawMessage, code int, msg string) {
	resp := jsonrpcResponse{JSONRPC: "2.0", ID: id, Error: &jsonrpcError{Code: code, Message: msg}}
	s.writeJSON(resp)
}

func (s *Server) writeJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	data = append(data, '\n')
	s.writer.Write(data)
}
