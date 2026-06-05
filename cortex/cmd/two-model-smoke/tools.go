// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Tool surface exposed to the LLMs. Each tool maps 1:1 onto a cortex
// API call; arguments come in as JSON strings (per OpenAI tool-calling
// shape) and tool results are returned as JSON strings.
//
// Errors are returned as {"error": "..."} JSON so the model can recover
// rather than seeing the call simply fail.

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"matrix/cortex"
	"matrix/cortex/memory"
	"matrix/cortex/query"
)

// toolDefs returns the static tool catalog advertised to the LLMs.
// Schemas are valid JSON Schema (subset OpenAI accepts).
func toolDefs() []ToolDef {
	return []ToolDef{
		{
			Type: "function",
			Function: ToolDefFunction{
				Name: "cortex_write",
				Description: `Write a typed memory to the shared cortex. Returns {uri} on success or {error} on validation failure. CRITICAL: data field names use exact PascalCase (Statement, Subject, Stance, ...) — JSON keys match case-insensitively but do NOT translate underscores; "evidence_for" will be ignored, use "EvidenceFor". Required fields per type and exact valid values:

Identity:   {Name (req), DID, Wallets:[str], Roles:[str]}
            Do NOT include PublicKeys (binary; not for LLM use).
            Example: {"Name":"Alice","DID":"did:matrix:alice","Roles":["researcher"]}

Fact:       {Statement (req), Subject (req), Predicate (req), Source, AsOf}
            Do NOT include Object (binary base64 field; will reject your input).
            Example: {"Statement":"Paxos guarantees safety under crash failures","Subject":"paxos","Predicate":"guarantees"}

Preference: {Topic (req), Polarity (req), StrengthVal (req float 0..1), Rationale}
            Polarity ∈ {"prefer","avoid","neutral","do","dont"}.
            Do NOT include Value (binary CBOR field; not for LLM use).
            Example: {"Topic":"verbose-logs","Polarity":"avoid","StrengthVal":0.8,"Rationale":"signal-to-noise"}

Belief:     {Statement (req), Subject (req), Stance (req), EvidenceFor:[str], EvidenceAgainst:[str]}
            Stance ∈ {"believe","doubt","suspect"} (NO other values; "supportive"/"reject" will reject).
            Example: {"Statement":"FLP impossibility holds","Subject":"flp","Stance":"believe","EvidenceFor":["FLP1985"]}

Event:      {Kind (req), OutcomeVal (req), Counterparty, IntentRef, Summary}
            Kind ∈ {"intent_completed","intent_failed","payment","dispatch","observation","interaction"}.
            OutcomeVal ∈ {"success","failure","partial"}.
            Example: {"Kind":"observation","OutcomeVal":"success","Summary":"alice wrote 5 facts"}

Goal:       {Statement (req), Status (req), HorizonEnd (RFC3339), SuccessCriteria:[str], Subgoals:[str]}
            Status ∈ {"active","paused","completed","abandoned"} (NOT "ongoing" or "horizon_status").
            Example: {"Statement":"Build a verified key-value store","Status":"active","SuccessCriteria":["linearizable reads"]}

Constraint: {Statement (req), Polarity (req), StrengthVal (req), Trigger, Source (req)}
            Polarity ∈ {"prefer","avoid","neutral","do","dont"}.
            StrengthVal ∈ {"soft","firm","hard"} (string enum NOT a number).
            Source ∈ {"user_declared","learned","inferred"}.
            Example: {"Statement":"Never anchor on chain without consent","Polarity":"dont","StrengthVal":"hard","Source":"user_declared"}

Capability: {Subject (req), Capability (req), Verified:bool, LastObserved:RFC3339}
            Example: {"Subject":"alice","Capability":"sign_intents","Verified":true,"LastObserved":"2026-05-22T22:00:00Z"}

Pattern:    {Statement (req), Strength (req float 0..1), Coverage (req INT count, NOT float), DerivedFrom:[uri]}
            Coverage is an INTEGER count of supporting observations, not a fraction.
            Example: {"Statement":"2f+1 replicas tolerate f crash failures","Strength":0.95,"Coverage":12,"DerivedFrom":[]}

Tags must be plain strings; no nested structures. Importance 0-10 (default 5).`,
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"type": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"Identity", "Fact", "Preference", "Belief", "Event", "Goal", "Constraint", "Capability", "Pattern"},
							"description": "Memory type.",
						},
						"data": map[string]interface{}{
							"type":        "object",
							"description": "Type-specific payload using PascalCase field names per the schema in the tool description. Send a JSON object, NOT a JSON-encoded string.",
						},
						"tags": map[string]interface{}{
							"type":        "array",
							"items":       map[string]interface{}{"type": "string"},
							"description": "Plain string tags (1-3, lowercase, hyphenated) for retrieval.",
						},
						"importance": map[string]interface{}{
							"type":    "integer",
							"minimum": 0,
							"maximum": 10,
						},
					},
					"required": []string{"type", "data"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolDefFunction{
				Name:        "cortex_resolve",
				Description: "Read one memory by its canonical URI (matrix://cortex/<Type>/<id>#<version>). Returns full memory record.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"uri": map[string]interface{}{
							"type":        "string",
							"description": "Canonical cortex URI from a previous cortex_write or cortex_find result.",
						},
					},
					"required": []string{"uri"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolDefFunction{
				Name:        "cortex_find",
				Description: "Query the cortex by type, tag, or near-text. Returns matching memories with their rendered short forms. Tag-and-type filters are conjunctive.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"type": map[string]interface{}{
							"type":        "string",
							"description": "Optional type filter. One of Identity, Fact, Preference, Belief, Event, Goal, Constraint, Capability, Pattern, or omit for any.",
							"enum":        []string{"Identity", "Fact", "Preference", "Belief", "Event", "Goal", "Constraint", "Capability", "Pattern"},
						},
						"tag": map[string]interface{}{
							"type":        "string",
							"description": "Optional tag to require (HasTag predicate).",
						},
						"near": map[string]interface{}{
							"type":        "string",
							"description": "Optional natural-language phrase for vector recall (HashEmbedder geometry; substring overlap works, semantics do not).",
						},
						"limit": map[string]interface{}{
							"type":    "integer",
							"default": 10,
						},
					},
				},
			},
		},
		{
			Type: "function",
			Function: ToolDefFunction{
				Name:        "cortex_list",
				Description: "List all memory IDs of a given type. Cheap full-namespace scan; use cortex_find for filtered queries.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"type": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"Identity", "Fact", "Preference", "Belief", "Event", "Goal", "Constraint", "Capability", "Pattern"},
							"description": "Memory type to enumerate.",
						},
						"limit": map[string]interface{}{"type": "integer", "default": 50},
					},
					"required": []string{"type"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolDefFunction{
				Name:        "cortex_update",
				Description: "Replace a memory's typed data. Bumps version (writes new mv/<id>/v/<n+1>); old versions retained for audit. Returns new URI.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"uri": map[string]interface{}{
							"type":        "string",
							"description": "URI of an existing memory (any version).",
						},
						"data": map[string]interface{}{
							"type":        "object",
							"description": "Full replacement payload for the type. Schema must match the existing memory's type.",
						},
					},
					"required": []string{"uri", "data"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolDefFunction{
				Name:        "cortex_update_head",
				Description: "Mutate Head-only fields (tags, declared_importance) without bumping data version. Use for re-tagging or salience adjustment without changing semantics.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"uri": map[string]interface{}{"type": "string"},
						"set_tags": map[string]interface{}{
							"type":        "array",
							"items":       map[string]interface{}{"type": "string"},
							"description": "If present, REPLACES the tag set with this list.",
						},
						"importance": map[string]interface{}{
							"type":        "integer",
							"minimum":     0,
							"maximum":     10,
							"description": "If present (0-10), updates declared_importance.",
						},
					},
					"required": []string{"uri"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolDefFunction{
				Name:        "cortex_tombstone",
				Description: "Soft-delete a memory with an audit reason. The memory becomes invisible to default queries but the version chain is preserved.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"uri":    map[string]interface{}{"type": "string"},
						"reason": map[string]interface{}{"type": "string"},
					},
					"required": []string{"uri", "reason"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolDefFunction{
				Name:        "cortex_add_edge",
				Description: "Add a typed edge between two memories (graph layer).",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"src_uri": map[string]interface{}{"type": "string"},
						"edge_type": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"derived_from", "supersedes", "references", "contradicts", "corroborates", "consents_to", "dispatched_to", "attested_by", "cited_in", "tombstones", "part_of", "instance_of", "caused_by", "observed_by"},
							"description": "Edge semantic.",
						},
						"dst_uri": map[string]interface{}{"type": "string"},
						"weight": map[string]interface{}{
							"type":    "number",
							"default": 1.0,
						},
					},
					"required": []string{"src_uri", "edge_type", "dst_uri"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolDefFunction{
				Name:        "cortex_list_edges",
				Description: "List edges connected to a memory. dir = 'out' (outgoing), 'in' (incoming), or 'both'.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"src_uri":   map[string]interface{}{"type": "string"},
						"direction": map[string]interface{}{"type": "string", "enum": []string{"out", "in", "both"}, "default": "out"},
					},
					"required": []string{"src_uri"},
				},
			},
		},
	}
}

// Dispatcher executes a tool call against the supplied cortex on
// behalf of the calling agent (whose identity becomes CreatedBy on
// any writes).
type Dispatcher struct {
	c     *cortex.Cortex
	actor string // shared cortex actor; used as CreatedBy fallback if agent identity is unset
}

// Dispatch runs one tool call. Returns a JSON-encoded result string
// suitable for the role="tool" reply message. Errors are wrapped in
// {"error":"..."} JSON so the model sees them as structured data.
func (d *Dispatcher) Dispatch(call ToolCall, agentName string) string {
	out, err := d.dispatch(call, agentName)
	if err != nil {
		return marshalErr(err)
	}
	enc, mErr := json.Marshal(out)
	if mErr != nil {
		return marshalErr(fmt.Errorf("marshal result: %w", mErr))
	}
	return string(enc)
}

func (d *Dispatcher) dispatch(call ToolCall, agentName string) (interface{}, error) {
	if call.Function.Name == "" {
		return nil, fmt.Errorf("empty tool name")
	}
	args := map[string]interface{}{}
	if call.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return nil, fmt.Errorf("decode arguments: %w (raw=%s)", err, truncate(call.Function.Arguments, 256))
		}
	}
	createdBy := agentName
	if createdBy == "" {
		createdBy = d.actor
	}

	switch call.Function.Name {
	case "cortex_write":
		return d.cortexWrite(args, createdBy)
	case "cortex_resolve":
		return d.cortexResolve(args)
	case "cortex_find":
		return d.cortexFind(args)
	case "cortex_list":
		return d.cortexList(args)
	case "cortex_update":
		return d.cortexUpdate(args, createdBy)
	case "cortex_update_head":
		return d.cortexUpdateHead(args, createdBy)
	case "cortex_tombstone":
		return d.cortexTombstone(args, createdBy)
	case "cortex_add_edge":
		return d.cortexAddEdge(args, createdBy)
	case "cortex_list_edges":
		return d.cortexListEdges(args)
	}
	return nil, fmt.Errorf("unknown tool %q", call.Function.Name)
}

// ---------- per-tool implementations -------------------------------

func (d *Dispatcher) cortexWrite(args map[string]interface{}, createdBy string) (interface{}, error) {
	typeName, _ := args["type"].(string)
	if typeName == "" {
		return nil, fmt.Errorf("missing 'type'")
	}
	rawData, ok := args["data"]
	if !ok {
		return nil, fmt.Errorf("missing 'data'")
	}
	dataBytes, err := json.Marshal(rawData)
	if err != nil {
		return nil, fmt.Errorf("re-marshal data: %w", err)
	}
	td, err := parseTypedJSON(typeName, string(dataBytes))
	if err != nil {
		return nil, fmt.Errorf("parse %s data: %w", typeName, err)
	}

	rawTags := stringSlice(args["tags"])
	importance := intOr(args["importance"], 5)

	tags := make([]memory.Tag, 0, len(rawTags))
	for _, t := range rawTags {
		tags = append(tags, memory.Tag(t))
	}
	head := memory.Head{
		ActorScope:         d.actor,
		Tags:               tags,
		DeclaredImportance: uint8(clampInt(importance, 0, 10)),
	}
	uri, err := d.c.Write(head, td, cortex.WriteMeta{
		CreatedBy:  createdBy,
		Forms:      memory.Forms{Short: typeName + ":" + summarize(string(dataBytes))},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"uri": string(uri)}, nil
}

func (d *Dispatcher) cortexResolve(args map[string]interface{}) (interface{}, error) {
	uri, _ := args["uri"].(string)
	if uri == "" {
		return nil, fmt.Errorf("missing 'uri'")
	}
	m, err := d.c.Resolve(memory.URI(uri))
	if err != nil {
		return nil, err
	}
	return memorySummary(m), nil
}

func (d *Dispatcher) cortexFind(args map[string]interface{}) (interface{}, error) {
	q := query.Query{Limit: clampInt(intOr(args["limit"], 10), 1, 50)}
	if t, ok := args["type"].(string); ok && t != "" {
		mt := parseTypeName(t)
		if !mt.Valid() {
			return nil, fmt.Errorf("unknown type %q", t)
		}
		q.Type = []memory.Type{mt}
	}
	if tag, ok := args["tag"].(string); ok && tag != "" {
		q.Where = query.HasTag{Tag: tag}
	}
	if near, ok := args["near"].(string); ok && near != "" {
		q.Near = near
	}
	q.Form = query.FormShort
	res, err := d.c.Find(q)
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]interface{}, 0, len(res.Memories))
	for i, m := range res.Memories {
		row := memorySummary(m)
		if i < len(res.Rendered) {
			row["short"] = res.Rendered[i]
		}
		rows = append(rows, row)
	}
	return map[string]interface{}{
		"count":   len(rows),
		"results": rows,
	}, nil
}

func (d *Dispatcher) cortexList(args map[string]interface{}) (interface{}, error) {
	typeName, _ := args["type"].(string)
	mt := parseTypeName(typeName)
	if !mt.Valid() {
		return nil, fmt.Errorf("unknown type %q", typeName)
	}
	limit := clampInt(intOr(args["limit"], 50), 1, 200)
	ids, err := d.c.ListByType(mt, limit)
	if err != nil {
		return nil, err
	}
	uris := make([]string, 0, len(ids))
	for _, id := range ids {
		uris = append(uris, string(cortex.BuildURI(mt, id, 1)))
	}
	return map[string]interface{}{"count": len(uris), "uris": uris}, nil
}

func (d *Dispatcher) cortexUpdate(args map[string]interface{}, createdBy string) (interface{}, error) {
	uri, _ := args["uri"].(string)
	if uri == "" {
		return nil, fmt.Errorf("missing 'uri'")
	}
	t, _, _, err := cortex.ParseURI(memory.URI(uri))
	if err != nil {
		return nil, err
	}
	rawData, ok := args["data"]
	if !ok {
		return nil, fmt.Errorf("missing 'data'")
	}
	dataBytes, err := json.Marshal(rawData)
	if err != nil {
		return nil, fmt.Errorf("re-marshal data: %w", err)
	}
	td, err := parseTypedJSON(t.String(), string(dataBytes))
	if err != nil {
		return nil, fmt.Errorf("parse %s data: %w", t.String(), err)
	}
	newURI, err := d.c.Update(memory.URI(uri), td, cortex.WriteMeta{
		CreatedBy:  createdBy,
		Forms:      memory.Forms{Short: t.String() + ":" + summarize(string(dataBytes))},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"uri": string(newURI)}, nil
}

func (d *Dispatcher) cortexUpdateHead(args map[string]interface{}, createdBy string) (interface{}, error) {
	uri, _ := args["uri"].(string)
	if uri == "" {
		return nil, fmt.Errorf("missing 'uri'")
	}
	patch := cortex.HeadPatch{}
	if raw, ok := args["set_tags"]; ok {
		rawTags := stringSlice(raw)
		tags := make([]memory.Tag, 0, len(rawTags))
		for _, t := range rawTags {
			tags = append(tags, memory.Tag(t))
		}
		patch.Tags = &tags
	}
	if raw, ok := args["importance"]; ok {
		v := uint8(clampInt(intOr(raw, 5), 0, 10))
		patch.DeclaredImportance = &v
	}
	newURI, err := d.c.UpdateHead(memory.URI(uri), patch, cortex.UpdateHeadMeta{CreatedBy: createdBy})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"uri": string(newURI)}, nil
}

func (d *Dispatcher) cortexTombstone(args map[string]interface{}, createdBy string) (interface{}, error) {
	uri, _ := args["uri"].(string)
	reason, _ := args["reason"].(string)
	if uri == "" || reason == "" {
		return nil, fmt.Errorf("uri and reason required")
	}
	if err := d.c.Tombstone(memory.URI(uri), reason, createdBy); err != nil {
		return nil, err
	}
	return map[string]interface{}{"ok": true}, nil
}

func (d *Dispatcher) cortexAddEdge(args map[string]interface{}, createdBy string) (interface{}, error) {
	srcURI, _ := args["src_uri"].(string)
	dstURI, _ := args["dst_uri"].(string)
	edgeName, _ := args["edge_type"].(string)
	if srcURI == "" || dstURI == "" || edgeName == "" {
		return nil, fmt.Errorf("src_uri, dst_uri, edge_type all required")
	}
	et, ok := memory.ParseEdgeType(edgeName)
	if !ok {
		return nil, fmt.Errorf("unknown edge type %q", edgeName)
	}
	_, srcID, _, err := cortex.ParseURI(memory.URI(srcURI))
	if err != nil {
		return nil, fmt.Errorf("parse src: %w", err)
	}
	_, dstID, _, err := cortex.ParseURI(memory.URI(dstURI))
	if err != nil {
		return nil, fmt.Errorf("parse dst: %w", err)
	}
	weight := float32(floatOr(args["weight"], 1.0))
	if err := d.c.AddEdge(srcID, et, dstID, cortex.AddEdgeMeta{CreatedBy: createdBy, Weight: weight}); err != nil {
		return nil, err
	}
	return map[string]interface{}{"ok": true}, nil
}

func (d *Dispatcher) cortexListEdges(args map[string]interface{}) (interface{}, error) {
	srcURI, _ := args["src_uri"].(string)
	if srcURI == "" {
		return nil, fmt.Errorf("missing src_uri")
	}
	dir := strings.ToLower(stringOr(args["direction"], "out"))
	_, srcID, _, err := cortex.ParseURI(memory.URI(srcURI))
	if err != nil {
		return nil, fmt.Errorf("parse src: %w", err)
	}
	type edgeRow struct {
		Direction  string  `json:"direction"`
		EdgeType   string  `json:"edge_type"`
		Other      string  `json:"other"`
		Weight     float32 `json:"weight,omitempty"`
		Tombstoned bool    `json:"tombstoned,omitempty"`
	}
	var rows []edgeRow
	opts := cortex.IterEdgesOptions{IncludeTombstoned: true}
	collect := func(direction string) func(*memory.EdgeRecord) error {
		return func(rec *memory.EdgeRecord) error {
			other := rec.Dst
			if direction == "in" {
				other = rec.Src
			}
			rows = append(rows, edgeRow{
				Direction:  direction,
				EdgeType:   rec.Type.String(),
				Other:      hexID(other),
				Weight:     rec.Weight,
				Tombstoned: rec.Tombstoned,
			})
			return nil
		}
	}
	if dir == "out" || dir == "both" {
		if err := d.c.IterEdgesOut(srcID, opts, collect("out")); err != nil {
			return nil, err
		}
	}
	if dir == "in" || dir == "both" {
		if err := d.c.IterEdgesIn(srcID, opts, collect("in")); err != nil {
			return nil, err
		}
	}
	return map[string]interface{}{"count": len(rows), "edges": rows}, nil
}

// ---------- helpers ------------------------------------------------

func memorySummary(m *memory.Memory) map[string]interface{} {
	row := map[string]interface{}{
		"uri":        string(cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)),
		"type":       m.Head.Type.String(),
		"version":    m.Head.CurrentVersion,
		"tags":       m.Head.Tags,
		"importance": m.Head.DeclaredImportance,
		"created_by": m.Version.CreatedBy,
		"short":      m.Version.Forms.Short,
	}
	if m.Head.Tombstoned != nil {
		row["tombstoned"] = map[string]interface{}{
			"reason": m.Head.Tombstoned.Reason,
			"by":     m.Head.Tombstoned.By,
		}
	}
	return row
}

func marshalErr(err error) string {
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	return string(b)
}

func stringSlice(v interface{}) []string {
	if v == nil {
		return nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func intOr(v interface{}, def int) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		n, err := x.Int64()
		if err == nil {
			return int(n)
		}
	}
	return def
}

func floatOr(v interface{}, def float64) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case json.Number:
		n, err := x.Float64()
		if err == nil {
			return n
		}
	}
	return def
}

func stringOr(v interface{}, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func summarize(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

func hexID(id memory.ID) string {
	return fmt.Sprintf("%x", id[:])
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
