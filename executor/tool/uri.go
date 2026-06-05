// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tool

import (
	"fmt"
	"strings"
)

// ToolURI is a parsed matrix:// tool URI.
//
// Forms (Q17 lock):
//
//	matrix://tool/mcp/<server-alias>/<tool-name>@<version>     MCP-backed
//	matrix://tool/<namespace>/<tool-name>@<version>            native (chain)
//
// The MCP form has Provider="mcp" and Server set to the alias.
// The native form has Provider equal to one of the closed S6
// namespaces (argus/orob/plv/...); Server is unused.
//
// Bare-head URIs (no @version) are rejected at parse time per S4.
type ToolURI struct {
	Raw      string
	Provider string // "mcp" OR one of validNativeNamespaces
	Server   string // MCP form only; empty for native
	Name     string
	Version  string
}

const toolScheme = "matrix://tool/"

// IsMCP reports whether the URI is MCP-backed.
func (u ToolURI) IsMCP() bool { return u.Provider == "mcp" }

// IsNative reports whether the URI is a native chain tool.
func (u ToolURI) IsNative() bool { return validNativeNamespaces[u.Provider] }

// String returns the URI in canonical form.
func (u ToolURI) String() string {
	if u.IsMCP() {
		return fmt.Sprintf("%s%s/%s/%s@%s", toolScheme, u.Provider, u.Server, u.Name, u.Version)
	}
	return fmt.Sprintf("%s%s/%s@%s", toolScheme, u.Provider, u.Name, u.Version)
}

// ParseToolURI splits a tool URI per Q17 / S4. Returns ErrInvalidURI
// or ErrUnpinnedTool on malformed input.
func ParseToolURI(s string) (ToolURI, error) {
	if !strings.HasPrefix(s, toolScheme) {
		return ToolURI{}, fmt.Errorf("%w: must start with %q", ErrInvalidURI, toolScheme)
	}
	rest := s[len(toolScheme):]

	// Split off the @version suffix (S4 hard rule).
	at := strings.LastIndexByte(rest, '@')
	if at < 0 || at == len(rest)-1 {
		return ToolURI{}, fmt.Errorf("%w: %s", ErrUnpinnedTool, s)
	}
	beforeVersion := rest[:at]
	version := rest[at+1:]
	if !validVersion(version) {
		return ToolURI{}, fmt.Errorf("%w: invalid version %q", ErrInvalidURI, version)
	}

	parts := strings.Split(beforeVersion, "/")
	switch len(parts) {
	case 3:
		// MCP: mcp/<alias>/<name>
		if parts[0] != "mcp" {
			return ToolURI{}, fmt.Errorf("%w: 3-component path requires provider=mcp, got %q", ErrInvalidURI, parts[0])
		}
		if parts[1] == "" || parts[2] == "" {
			return ToolURI{}, fmt.Errorf("%w: empty alias or name", ErrInvalidURI)
		}
		if !validAlias(parts[1]) {
			return ToolURI{}, fmt.Errorf("%w: alias %q has invalid chars", ErrInvalidURI, parts[1])
		}
		return ToolURI{
			Raw:      s,
			Provider: "mcp",
			Server:   parts[1],
			Name:     parts[2],
			Version:  version,
		}, nil
	case 2:
		// Native: <namespace>/<name>
		if !validNativeNamespaces[parts[0]] {
			return ToolURI{}, fmt.Errorf("%w: unknown native namespace %q (S6 closed set)", ErrInvalidURI, parts[0])
		}
		if parts[1] == "" {
			return ToolURI{}, fmt.Errorf("%w: empty native tool name", ErrInvalidURI)
		}
		return ToolURI{
			Raw:      s,
			Provider: parts[0],
			Name:     parts[1],
			Version:  version,
		}, nil
	default:
		return ToolURI{}, fmt.Errorf("%w: expected 2 or 3 path components, got %d", ErrInvalidURI, len(parts))
	}
}

// validVersion is permissive: any non-empty token without '/' is OK.
// Concrete shape (semver, sha256-pinned digest, contract address) is
// validated by the downstream provider.
func validVersion(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == '+' || r == ':':
		default:
			return false
		}
	}
	return true
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
