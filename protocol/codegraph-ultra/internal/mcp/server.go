// Package mcp implements a Model Context Protocol server (JSON-RPC 2.0 over stdio,
// protocol version 2024-11-05) that exposes CodeGraph Ultra retrieval tools.
// All tool outputs pass through a fragment guard to prevent raw source leaks.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"centra/protocol/codegraph-ultra/internal/model"
	"centra/protocol/codegraph-ultra/internal/store"
)

const (
	protocolVersion = "2024-11-05"
	serverName      = "codegraph-ultra"
	serverVersion   = "0.2.0"
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
	ID       string `json:"id"`
	MaxDepth int    `json:"max_depth,omitempty"`
}

type searchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

type subgraphArgs struct {
	IDs       []string `json:"ids"`
	EdgeTypes []string `json:"edge_types,omitempty"`
}

type callChainArgs struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type typeHierarchyArgs struct {
	ID string `json:"id"`
}

type fileSymbolsArgs struct {
	File string `json:"file"`
}

type graphDiffArgs struct {
	A string `json:"a"`
	B string `json:"b"`
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
			Description: "Look up a symbol by its fully-qualified node ID and return its .kvx fragment with signature, doc, location, and enrichment.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Node ID (e.g. pkg/path.SymbolName)"}},"required":["id"]}`),
		},
		{
			Name:        "search",
			Description: "Full-text search across node IDs, names, docs, sigs, and summaries. Returns ranked results.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search query (AND-joined terms)"},"limit":{"type":"integer","description":"Max results (default 20)"}},"required":["query"]}`),
		},
		{
			Name:        "neighbors",
			Description: "Get the bounded subgraph around a node (BFS to a given depth along a specific edge type or all edges). Shows callers, callees, imports, references, etc.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Center node ID"},"type":{"type":"string","description":"Edge type filter: calls, imports, references, implements, embeds, contains, uses, inherits, overrides. Omit for all."},"depth":{"type":"integer","description":"BFS depth 1-3 (default 1)"}},"required":["id"]}`),
		},
		{
			Name:        "impact",
			Description: "Blast radius analysis: which symbols are affected if the given node changes. Traces reverse transitive closure over calls, references, and implements edges.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Node to evaluate impact for"},"max_depth":{"type":"integer","description":"Max reverse traversal depth (default 64)"}},"required":["id"]}`),
		},
		{
			Name:        "call_chain",
			Description: "Find the shortest call path between two symbols. Returns the chain of intermediate callers/callees connecting source to target.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"from":{"type":"string","description":"Source node ID"},"to":{"type":"string","description":"Target node ID"}},"required":["from","to"]}`),
		},
		{
			Name:        "type_hierarchy",
			Description: "Show the type hierarchy for a node: what it implements, what implements it, what it embeds, what embeds it, and inheritance chain.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Node ID to show hierarchy for"}},"required":["id"]}`),
		},
		{
			Name:        "file_symbols",
			Description: "List all symbols defined in a specific file, grouped by kind (func, type, const, var, etc.).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file":{"type":"string","description":"File path (relative to repo root)"}},"required":["file"]}`),
		},
		{
			Name:        "graph_diff",
			Description: "Compare two graph databases and show added/removed/changed nodes and edges.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","description":"Path to base graph DB"},"b":{"type":"string","description":"Path to new graph DB"}},"required":["a","b"]}`),
		},
		{
			Name:        "stats",
			Description: "Return aggregate graph statistics: node/edge counts, languages, breakdown by kind/type.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "subgraph",
			Description: "Fetch a subgraph of specific node IDs with all interconnecting edges. Useful for understanding relationships between a set of symbols.",
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
	case "call_chain":
		result, err = s.toolCallChain(params.Arguments)
	case "type_hierarchy":
		result, err = s.toolTypeHierarchy(params.Arguments)
	case "file_symbols":
		result, err = s.toolFileSymbols(params.Arguments)
	case "graph_diff":
		result, err = s.toolGraphDiff(params.Arguments)
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
	if args.Depth > 3 {
		args.Depth = 3
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
			Content: []textContent{{Type: "text", Text: fmt.Sprintf("no impact for %q — nothing depends on it", args.ID)}},
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
		Content: []textContent{{Type: "text", Text: guardFragmentSafe(b.String())}},
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

	idSet := make(map[string]bool, len(args.IDs))
	for _, id := range args.IDs {
		idSet[id] = true
	}

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
		Content: []textContent{{Type: "text", Text: guardFragmentSafe(b.String())}},
	}, nil
}

// ── new advanced tools ──────────────────────────────────────────────

func (s *Server) toolCallChain(raw json.RawMessage) (toolCallResult, error) {
	var args callChainArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolCallResult{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.From == "" || args.To == "" {
		return toolCallResult{}, fmt.Errorf("both 'from' and 'to' are required")
	}

	from := s.db.GetNode(args.From)
	to := s.db.GetNode(args.To)
	if from == nil {
		return toolCallResult{}, fmt.Errorf("source node %q not found", args.From)
	}
	if to == nil {
		return toolCallResult{}, fmt.Errorf("target node %q not found", args.To)
	}

	// BFS to find shortest path along calls edges.
	path := s.findPath(args.From, args.To, model.EdgeCalls)
	if path == nil {
		// Try reverse direction too
		path = s.findPath(args.From, args.To, "")
	}

	if path == nil {
		return toolCallResult{
			Content: []textContent{{Type: "text", Text: fmt.Sprintf("no call path found between %s and %s", args.From, args.To)}},
		}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# FRAGMENT tool=call_chain from=%s to=%s hops=%d\n", args.From, args.To, len(path)-1)
	for i, id := range path {
		n := s.db.GetNode(id)
		if n != nil {
			if i > 0 {
				b.WriteString("  -> ")
			}
			b.WriteString(formatNode(n))
			b.WriteByte('\n')
		}
	}

	return toolCallResult{
		Content: []textContent{{Type: "text", Text: guardFragmentSafe(b.String())}},
	}, nil
}

func (s *Server) findPath(from, to string, edgeType model.EdgeType) []string {
	type state struct {
		id    string
		path  []string
	}
	
	visited := map[string]bool{from: true}
	queue := []state{{id: from, path: []string{from}}}
	
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		
		if cur.id == to {
			return cur.path
		}
		
		if len(cur.path) > 6 {
			continue
		}
		
		var neighbors []string
		if edgeType != "" {
			neighbors = s.db.Forward(cur.id, edgeType)
		} else {
			neighbors = s.db.ForwardAll(cur.id)
		}
		
		for _, nb := range neighbors {
			if !visited[nb] {
				visited[nb] = true
				newPath := make([]string, len(cur.path)+1)
				copy(newPath, cur.path)
				newPath[len(cur.path)] = nb
				queue = append(queue, state{id: nb, path: newPath})
			}
		}
	}
	return nil
}

func (s *Server) toolTypeHierarchy(raw json.RawMessage) (toolCallResult, error) {
	var args typeHierarchyArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolCallResult{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.ID == "" {
		return toolCallResult{}, fmt.Errorf("id is required")
	}

	n := s.db.GetNode(args.ID)
	if n == nil {
		return toolCallResult{}, fmt.Errorf("node %q not found", args.ID)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# FRAGMENT tool=type_hierarchy root=%s\n", args.ID)
	b.WriteString(formatNode(n))
	b.WriteByte('\n')

	// What this implements
	implements := s.db.Forward(args.ID, model.EdgeImplements)
	if len(implements) > 0 {
		b.WriteString("\nIMPLEMENTS:\n")
		for _, id := range implements {
			if cn := s.db.GetNode(id); cn != nil {
				fmt.Fprintf(&b, "  %s\n", formatNode(cn))
			}
		}
	}

	// What implements this
	implementedBy := s.db.Reverse(args.ID, model.EdgeImplements)
	if len(implementedBy) > 0 {
		b.WriteString("\nIMPLEMENTED_BY:\n")
		for _, id := range implementedBy {
			if cn := s.db.GetNode(id); cn != nil {
				fmt.Fprintf(&b, "  %s\n", formatNode(cn))
			}
		}
	}

	// What this embeds
	embeds := s.db.Forward(args.ID, model.EdgeEmbeds)
	if len(embeds) > 0 {
		b.WriteString("\nEMBEDS:\n")
		for _, id := range embeds {
			if cn := s.db.GetNode(id); cn != nil {
				fmt.Fprintf(&b, "  %s\n", formatNode(cn))
			}
		}
	}

	// What embeds this
	embeddedBy := s.db.Reverse(args.ID, model.EdgeEmbeds)
	if len(embeddedBy) > 0 {
		b.WriteString("\nEMBEDDED_BY:\n")
		for _, id := range embeddedBy {
			if cn := s.db.GetNode(id); cn != nil {
				fmt.Fprintf(&b, "  %s\n", formatNode(cn))
			}
		}
	}

	// Inheritance
	inherits := s.db.Forward(args.ID, model.EdgeInherits)
	if len(inherits) > 0 {
		b.WriteString("\nINHERITS:\n")
		for _, id := range inherits {
			if cn := s.db.GetNode(id); cn != nil {
				fmt.Fprintf(&b, "  %s\n", formatNode(cn))
			}
		}
	}

	inheritedBy := s.db.Reverse(args.ID, model.EdgeInherits)
	if len(inheritedBy) > 0 {
		b.WriteString("\nINHERITED_BY:\n")
		for _, id := range inheritedBy {
			if cn := s.db.GetNode(id); cn != nil {
				fmt.Fprintf(&b, "  %s\n", formatNode(cn))
			}
		}
	}

	return toolCallResult{
		Content: []textContent{{Type: "text", Text: guardFragmentSafe(b.String())}},
	}, nil
}

func (s *Server) toolFileSymbols(raw json.RawMessage) (toolCallResult, error) {
	var args fileSymbolsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolCallResult{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.File == "" {
		return toolCallResult{}, fmt.Errorf("file is required")
	}

	// Find all nodes in this file
	allNodes := s.db.Nodes("")
	var fileNodes []*model.Node
	for _, n := range allNodes {
		if n.File == args.File {
			fileNodes = append(fileNodes, n)
		}
	}

	if len(fileNodes) == 0 {
		return toolCallResult{
			Content: []textContent{{Type: "text", Text: fmt.Sprintf("no symbols found in %q", args.File)}},
		}, nil
	}

	// Sort by kind then name
	sort.Slice(fileNodes, func(i, j int) bool {
		if fileNodes[i].Kind != fileNodes[j].Kind {
			return fileNodes[i].Kind < fileNodes[j].Kind
		}
		return fileNodes[i].Name < fileNodes[j].Name
	})

	var b strings.Builder
	fmt.Fprintf(&b, "# FRAGMENT tool=file_symbols file=%s count=%d\n", args.File, len(fileNodes))
	
	var lastKind model.Kind
	for _, n := range fileNodes {
		if n.Kind != lastKind {
			fmt.Fprintf(&b, "\n%s:\n", strings.ToUpper(string(n.Kind)))
			lastKind = n.Kind
		}
		b.WriteString("  ")
		b.WriteString(formatNode(n))
		b.WriteByte('\n')
	}

	return toolCallResult{
		Content: []textContent{{Type: "text", Text: guardFragmentSafe(b.String())}},
	}, nil
}

func (s *Server) toolGraphDiff(raw json.RawMessage) (toolCallResult, error) {
	var args graphDiffArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolCallResult{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.A == "" || args.B == "" {
		return toolCallResult{}, fmt.Errorf("both 'a' and 'b' database paths are required")
	}

	dbA, err := store.Open(args.A)
	if err != nil {
		return toolCallResult{}, fmt.Errorf("open base db: %w", err)
	}
	defer dbA.Close()

	dbB, err := store.Open(args.B)
	if err != nil {
		return toolCallResult{}, fmt.Errorf("open new db: %w", err)
	}
	defer dbB.Close()

	ixA := dbA.LoadIndex()
	ixB := dbB.LoadIndex()

	// Simple diff: compare node IDs and digests
	var added, removed, changed int
	for id, newNode := range ixB.Nodes {
		oldNode := ixA.Nodes[id]
		if oldNode == nil {
			added++
		} else if oldNode.Digest != newNode.Digest {
			changed++
		}
	}
	for id := range ixA.Nodes {
		if ixB.Nodes[id] == nil {
			removed++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# FRAGMENT tool=graph_diff\n")
	fmt.Fprintf(&b, "DELTA nodes_added=%d nodes_removed=%d nodes_changed=%d\n", added, removed, changed)

	if added > 0 {
		b.WriteString("\n# Added nodes:\n")
		for id, n := range ixB.Nodes {
			if ixA.Nodes[id] == nil {
				fmt.Fprintf(&b, "+NODE id=%s kind=%s\n", n.ID, n.Kind)
			}
		}
	}
	if removed > 0 {
		b.WriteString("\n# Removed nodes:\n")
		for id, n := range ixA.Nodes {
			if ixB.Nodes[id] == nil {
				fmt.Fprintf(&b, "-NODE id=%s kind=%s\n", n.ID, n.Kind)
			}
		}
	}
	if changed > 0 {
		b.WriteString("\n# Changed nodes:\n")
		for id, newNode := range ixB.Nodes {
			oldNode := ixA.Nodes[id]
			if oldNode != nil && oldNode.Digest != newNode.Digest {
				fmt.Fprintf(&b, "~NODE id=%s kind=%s\n", newNode.ID, newNode.Kind)
			}
		}
	}

	return toolCallResult{
		Content: []textContent{{Type: "text", Text: guardFragmentSafe(b.String())}},
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
		parts = append(parts, fmt.Sprintf("sig=%s", sanitizeKVXValue(n.Sig)))
	}
	if n.Enrich.Salience > 0 {
		parts = append(parts, fmt.Sprintf("sal=%.3f", n.Enrich.Salience))
	}
	if n.Enrich.Summary != "" {
		parts = append(parts, fmt.Sprintf("summary=%s", sanitizeKVXValue(n.Enrich.Summary)))
	} else if n.Doc != "" {
		parts = append(parts, fmt.Sprintf("doc=%s", sanitizeKVXValue(firstLine(n.Doc))))
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
		Content: []textContent{{Type: "text", Text: guardFragmentSafe(b.String())}},
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
