// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// MCP protocol types — typed structs for the methods the client uses
// against an MCP server. Subset chosen for v1 executor scope:
//
//	initialize                    — handshake; pin protocol version
//	notifications/initialized     — post-handshake ready signal
//	tools/list                    — enumerate server tools (Q21 verify)
//	tools/call                    — invoke a tool
//	ping                          — health check (Q16 health-pinged)
//
// Methods omitted at v1 (deferred per executor_locked_design):
//
//	prompts/list, prompts/get          — prompt-template surface; not
//	                                     used by Matrix skills (skills
//	                                     are MCL-authored, not server-supplied)
//	resources/list, resources/read     — resource surface; deferred,
//	                                     cortex is the canonical
//	                                     memory layer
//	sampling/createMessage             — server→client LLM callback;
//	                                     out of scope for v1 (executor
//	                                     owns its own llm slot)
//	logging/setLevel                   — operational, deferred
//	completion/complete                — argument completion UX, deferred
package mcp

import "encoding/json"

// ProtocolVersion is the MCP wire-protocol version this client speaks.
// Pinned to "2024-11-05" — the snapshot at which streamable HTTP became
// canonical and SSE-only legacy was deprecated. Servers reporting an
// older version are accepted (servers MUST be backward-compatible per
// spec) but downgrade behaviour is logged so manifest authors can
// upgrade their server pins.
const ProtocolVersion = "2024-11-05"

// ClientName + ClientVersion identify Matrix in the initialize handshake
// so MCP servers can log who's connecting.
const (
	ClientName    = "matrix-executor"
	ClientVersion = "0.1.0"
)

// InitializeParams is the params object sent on the initialize request.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// InitializeResult is the server's response to initialize.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	// Instructions is an optional server-supplied human-readable hint
	// (often documentation about tool semantics). Logged at debug,
	// not threaded into prompts.
	Instructions string `json:"instructions,omitempty"`
}

// Implementation identifies a peer (server or client) by name + version.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientCapabilities is the capability set we advertise to servers.
// v1 advertises no client-side capabilities (no roots subscription,
// no sampling callback) — Matrix is a pure client-of-tools.
type ClientCapabilities struct {
	// Roots advertises filesystem roots clients expose to servers.
	// Deferred — Matrix routes through filesystem-mcp on the server side.
	Roots *RootsCapability `json:"roots,omitempty"`

	// Sampling advertises that the client can serve sampling/createMessage
	// callbacks. Not in v1 (Q-locked: executor owns its own llm slot).
	Sampling *SamplingCapability `json:"sampling,omitempty"`

	// Experimental holds any non-standardised capability extensions.
	// Always omitted in v1.
	Experimental map[string]json.RawMessage `json:"experimental,omitempty"`
}

// ServerCapabilities is the capability set the server advertises in
// initialize. We inspect Tools to decide whether to verify the
// tools/list manifest at startup (Q21).
type ServerCapabilities struct {
	Tools     *ToolsCapability     `json:"tools,omitempty"`
	Prompts   *PromptsCapability   `json:"prompts,omitempty"`
	Resources *ResourcesCapability `json:"resources,omitempty"`
	Logging   *LoggingCapability   `json:"logging,omitempty"`

	Experimental map[string]json.RawMessage `json:"experimental,omitempty"`
}

// ToolsCapability tells us if the server supports list-changed
// notifications (forward-compatibility flag; Matrix v1 doesn't react
// to these — manifest is static per Q21).
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// PromptsCapability mirrors ToolsCapability shape for server prompts
// (deferred at v1).
type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability mirrors ToolsCapability shape for server resources
// (deferred at v1).
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// LoggingCapability is a capability flag (no fields) that signals the
// server emits notifications/message log events. Matrix v1 ignores them.
type LoggingCapability struct{}

// RootsCapability advertises that the client maintains a list of
// filesystem roots (deferred at v1).
type RootsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// SamplingCapability advertises that the client can be sampled by the
// server (deferred at v1).
type SamplingCapability struct{}

// Tool is the wire-format description of a single tool exposed by an
// MCP server. The Matrix tool registry pins each (Name, Server) pair
// against a manifest at startup and rejects drift (Q21).
type Tool struct {
	// Name uniquely identifies the tool within its server. Tools sharing
	// names across servers are addressed via matrix://tool/mcp/<server>/<name>.
	Name string `json:"name"`

	// Description is the natural-language summary the LLM sees when
	// deciding whether to invoke the tool. Threaded into the executor
	// model's tool-selection prompt.
	Description string `json:"description,omitempty"`

	// InputSchema is a JSON Schema describing the tool's args. Validated
	// at call time by both the executor (prior to dispatch) and the MCP
	// server (after receipt).
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ToolsListResult is the typed response to tools/list.
type ToolsListResult struct {
	Tools []Tool `json:"tools"`
	// NextCursor is set when the server supports pagination. Matrix v1
	// fetches the full list in one shot at startup; we'll page only
	// if a server returns NextCursor.
	NextCursor string `json:"nextCursor,omitempty"`
}

// CallToolParams is the params object sent on tools/call.
type CallToolParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
}

// CallToolResult is the typed response to tools/call. IsError captures
// in-band tool failure (e.g. "shell command exited 1") without the
// transport-level error path. Both surfaces are mapped to Matrix
// failure modes by the executor.
type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Content is a single piece of the tool result. MCP supports text,
// image, and embedded-resource content kinds; Matrix v1 surfaces text
// directly to the LLM and stores image/resource as base64 cortex Fact
// memories.
type Content struct {
	Type string `json:"type"`

	// For Type=="text".
	Text string `json:"text,omitempty"`

	// For Type=="image".
	Data     string `json:"data,omitempty"`     // base64
	MimeType string `json:"mimeType,omitempty"` // e.g. "image/png"

	// For Type=="resource".
	Resource *EmbeddedResource `json:"resource,omitempty"`
}

// EmbeddedResource is a server-supplied resource attached to a tool result.
type EmbeddedResource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"` // for text resources
	Blob     string `json:"blob,omitempty"` // base64 for binary resources
}

// PingParams is the (empty) params object for ping. Spec allows an
// arbitrary object; we send {} for compactness.
type PingParams struct{}

// PingResult is the (empty) response object for ping. A response with
// no error is success; the body is irrelevant.
type PingResult struct{}

// MCP method names — exported as constants so the client and tests
// share one source of truth.
const (
	MethodInitialize             = "initialize"
	MethodNotificationsInit      = "notifications/initialized"
	MethodNotificationsCancelled = "notifications/cancelled"
	MethodNotificationsProgress  = "notifications/progress"
	MethodToolsList              = "tools/list"
	MethodToolsCall              = "tools/call"
	MethodPing                   = "ping"
)

// Content type constants for CallToolResult.Content.
const (
	ContentTypeText     = "text"
	ContentTypeImage    = "image"
	ContentTypeResource = "resource"
)

// ExtractText concatenates every text-typed content block in a
// CallToolResult. Convenience for the common path where a tool returns
// "what happened" as plain text.
func ExtractText(r *CallToolResult) string {
	if r == nil {
		return ""
	}
	var out string
	for i, c := range r.Content {
		if c.Type != ContentTypeText {
			continue
		}
		if i > 0 && out != "" {
			out += "\n"
		}
		out += c.Text
	}
	return out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
