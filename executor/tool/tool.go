// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package tool wraps MCP-backed (and v1.1 native chain) tools behind
// a uniform interface that the executor plan walker dispatches against.
//
// Layered above mcp/: where mcp/ owns the JSON-RPC wire and per-server
// lifecycle, tool/ owns:
//
//   - the Matrix tool URI scheme (matrix://tool/...)  — Q17 lock
//   - manifest schemas: AgentManifest declares ServerEntry + ToolEntry — Q22 hash-pinning
//   - the Registry, where NativeTool and MCPTool providers register — Q19
//   - capability gating per ToolCallPayload.SideEffectClass            — Q5
//   - cross-package wiring with mcp.Manager + (next session) cortex Event journaling
package tool

import (
	"context"
	"errors"
	"fmt"
)

// Tool is the uniform interface that every Matrix tool implementation
// satisfies. The plan walker (next session) holds *Tool values
// (resolved via Registry.Get(uri)) and invokes Call.
//
// Result.IsError captures in-band tool failures (e.g. "shell exit 1");
// transport / protocol / capability errors come back through err.
type Tool interface {
	// URI returns the canonical matrix://tool/... URI this tool exposes,
	// version-pinned (Q22).
	URI() string

	// Description is the LLM-facing summary used for tool selection.
	// May embed escape-text from server (text/markdown allowed).
	Description() string

	// SideEffectClass declares what kind of side-effect this tool can
	// have. Closed enum from MCL/ir/plan.go ValidSideEffectClasses.
	// Used by the executor's capability gate before dispatch.
	SideEffectClass() string

	// Call invokes the tool. Args MUST already be schema-validated by
	// the caller (registry does this once at dispatch entry); Call may
	// assume well-formed input.
	Call(ctx context.Context, args map[string]interface{}) (*Result, error)
}

// Result is the typed outcome of a single Tool.Call.
//
// Mirrors mcp.CallToolResult so MCPTool can pass through cleanly, with
// added Matrix-specific fields (CallID, DurationMs) so the executor
// can journal Event memories without re-deriving them.
type Result struct {
	// Content carries the typed return value(s). Text is the common
	// case; image and embedded resources surface for media-producing
	// tools (image-mcp, screenshot-mcp).
	Content []Content `json:"content"`

	// IsError signals an in-band tool failure. Distinct from a Go-level
	// err: IsError=true is "the tool ran and reports failure"
	// (e.g. shell exit 1, fetch returned 404), whereas err is
	// "we couldn't even invoke the tool" (transport, capability, etc.).
	IsError bool `json:"is_error,omitempty"`

	// CallID is a ULID assigned at dispatch entry by the registry.
	// Pinned into the Matrix Event memory and the journal logs path
	// for cross-referencing tool args ↔ tool results.
	CallID string `json:"call_id,omitempty"`

	// DurationMs is wall-clock latency of the call (set by registry
	// dispatch, not by the underlying server).
	DurationMs int64 `json:"duration_ms,omitempty"`
}

// Content is a single piece of a tool result. Mirrors mcp.Content but
// kept separate so the tool layer doesn't leak mcp types upward to
// the executor / plan walker.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`     // base64 for image
	MimeType string `json:"mimeType,omitempty"` // for image
	URI      string `json:"uri,omitempty"`      // for embedded resource
}

// Content type constants (subset mirrored from mcp.ContentType*).
const (
	ContentTypeText     = "text"
	ContentTypeImage    = "image"
	ContentTypeResource = "resource"
)

// SideEffectClass constants — closed enum mirrored from
// MCL/ir/plan.go ValidSideEffectClasses to avoid a circular import.
const (
	SideEffectRead    = "read"
	SideEffectWrite   = "write"
	SideEffectNetwork = "network"
	SideEffectShell   = "shell"
	SideEffectChain   = "chain"
)

// ValidSideEffectClasses is the closed enum used by the capability gate.
var ValidSideEffectClasses = map[string]bool{
	SideEffectRead:    true,
	SideEffectWrite:   true,
	SideEffectNetwork: true,
	SideEffectShell:   true,
	SideEffectChain:   true,
}

// ExtractText concatenates every text-typed Content block. Convenience
// for the common path where a tool returns "what happened" as text.
func ExtractText(r *Result) string {
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

// Sentinel errors from the tool layer.
var (
	// ErrUnknownTool fires when Registry.Get receives a URI for a tool
	// not in the agent manifest. Distinct from "tool returned IsError"
	// — caller must look up a different URI or fail the plan node.
	ErrUnknownTool = errors.New("tool: unknown tool URI")

	// ErrInvalidURI fires when a URI doesn't parse as
	// matrix://tool/<provider>/<name>@<version> form. Q17 lock.
	ErrInvalidURI = errors.New("tool: invalid tool URI")

	// ErrUnpinnedTool fires when a URI has no @version. S4 hard rule.
	ErrUnpinnedTool = errors.New("tool: tool URI missing version pin")

	// ErrSideEffectDenied fires when the executor's capability gate
	// rejects a tool's declared side-effect class.
	ErrSideEffectDenied = errors.New("tool: side-effect class denied by agent capability set")

	// ErrInvalidSideEffect fires when a manifest declares an unknown
	// side-effect class. Closed enum is exhaustive.
	ErrInvalidSideEffect = errors.New("tool: invalid side-effect class")
)

// CapabilityGate decides whether a tool's declared SideEffectClass is
// permitted by the agent's manifest. Default-allow at v1 (all 5 classes
// usable); production agent manifests will narrow this via a closed
// allow-list. Implemented as a function type so tests can vary it.
type CapabilityGate func(sideEffect string) bool

// AllowAllSideEffects is the default gate (allows everything in
// ValidSideEffectClasses). Production agent manifests should pass a
// narrower gate.
func AllowAllSideEffects(sideEffect string) bool {
	return ValidSideEffectClasses[sideEffect]
}

// AllowOnlySideEffects returns a CapabilityGate that admits only the
// listed classes (each must be in the closed enum).
func AllowOnlySideEffects(allowed ...string) CapabilityGate {
	set := make(map[string]bool, len(allowed))
	for _, s := range allowed {
		set[s] = true
	}
	return func(sideEffect string) bool {
		return set[sideEffect]
	}
}

// validateSideEffect rejects unknown classes. Manifest validation
// uses this to fail at load time rather than at dispatch.
func validateSideEffect(s string) error {
	if !ValidSideEffectClasses[s] {
		return fmt.Errorf("%w: %q", ErrInvalidSideEffect, s)
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
