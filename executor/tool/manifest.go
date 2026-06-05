// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AgentManifest is the JSON-on-disk schema declaring what tools an
// agent can use. Loaded once at executor startup; passed to Registry
// for resolution.
//
// Schema lock points (executor_locked_design):
//
//	Q17 tool URI scheme: matrix://tool/mcp/<server-alias>/<tool-name>@<version>
//	Q18 server credentials via env-var refs ("$env:GITHUB_TOKEN")
//	Q19 NativeTool slot kept for future chain tools (none ship in v1)
//	Q21 ExpectedTools enumeration enforced by mcp.Manager (tools/list match)
//	Q22 server pinning via PackageDigest (sha256 of distribution package)
type AgentManifest struct {
	// SchemaVersion pins the manifest schema. v1 is the only version.
	SchemaVersion int `json:"schema_version"`

	// Agent identifies the agent (DID for production; freeform string
	// for dev/test). Mirrors research/06-agents.md A1 lock — every
	// agent has stable identity.
	Agent string `json:"agent"`

	// Description is human-readable; not load-bearing.
	Description string `json:"description,omitempty"`

	// Servers declares the MCP servers this agent uses.
	Servers []ServerEntry `json:"servers,omitempty"`

	// NativeTools declares architectural slots for chain-touching
	// native tools (Q19). v1 ships none; agent manifests can be
	// authored against future chain tools without a schema change.
	NativeTools []NativeToolEntry `json:"native_tools,omitempty"`

	// AllowedSideEffects narrows the capability gate. Empty = allow
	// all 5 classes (open default at v1; closed agent manifests will
	// list explicitly: e.g. ["read", "write", "network"] for a
	// research agent that's denied shell + chain).
	AllowedSideEffects []string `json:"allowed_side_effects,omitempty"`
}

// ServerEntry is one MCP server in an agent's manifest.
type ServerEntry struct {
	// Alias is the local logical name. Used in matrix://tool/mcp/<alias>/...
	// URIs (Q17) and decouples from the underlying package name so the
	// agent can swap implementations without changing every plan that
	// referenced the tool.
	Alias string `json:"alias"`

	// Transport is "stdio" or "http". Q15 lock.
	Transport string `json:"transport"`

	// PackageDigest is the sha256 of the published server package
	// (e.g. an npm tarball or a uv wheel). Q22 hard rule. Validation
	// against the actual installed package is the responsibility of
	// the operator — Matrix records it in the agent manifest so
	// auditors can re-derive what code was running for a given Intent.
	PackageDigest string `json:"package_digest"`

	// Version is the human-readable version string for the server
	// package (e.g. "2024.11.1"). Mirrors PackageDigest semantically;
	// PackageDigest is the source-of-truth for replay attestation.
	Version string `json:"version"`

	// Command + Args + Env apply when Transport=="stdio".
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`

	// Env is a list of "KEY=VALUE" entries OR "KEY=$env:NAME" refs
	// (Q18). EnvRef tokens are resolved at spawn time from the
	// executor process's environment; literal values pass through.
	// Sensitive values MUST be passed as $env: refs so they don't
	// leak through manifest content addressing.
	Env []string `json:"env,omitempty"`

	// Endpoint applies when Transport=="http".
	Endpoint string `json:"endpoint,omitempty"`

	// Headers apply when Transport=="http". Same env-var ref syntax
	// as Env.
	Headers map[string]string `json:"headers,omitempty"`

	// Tools enumerates the tools this server is expected to advertise.
	// Manager verifies tools/list at startup matches exactly (Q21).
	Tools []ToolEntry `json:"tools"`
}

// ToolEntry is a single tool exposed by a server.
type ToolEntry struct {
	// Name is the server-local tool name (e.g. "read_text_file"). Used
	// as the suffix of the matrix://tool/mcp/<alias>/<name>@<version>
	// URI.
	Name string `json:"name"`

	// Description is human-readable; threaded into the executor model's
	// tool-selection prompt verbatim.
	Description string `json:"description,omitempty"`

	// SideEffectClass is one of ValidSideEffectClasses. Required.
	SideEffectClass string `json:"side_effect_class"`

	// TimeoutMs is the per-call timeout in milliseconds. Zero =
	// registry default (30s).
	TimeoutMs int `json:"timeout_ms,omitempty"`

	// Description hints + JSON-Schema validation are the server's job;
	// Matrix stores only what's needed for capability + URI scoping.
}

// NativeToolEntry is a placeholder schema for chain tools (Q19). v1
// ships no implementations; the schema lands now so agent manifests
// don't need a v2 migration when the first chain tool launches.
type NativeToolEntry struct {
	// Namespace is one of "argus", "orob", "plv", "pofq", "registry",
	// "payments", "attest", "chain" — the 8 Paxeer tool wrappers per
	// research/05-skills-and-tools.md S6.
	Namespace string `json:"namespace"`

	// Name is the tool name within the namespace.
	Name string `json:"name"`

	// Version pins a contract address + ABI digest (mirrors PackageDigest).
	Version string `json:"version"`

	// Digest is the sha256 of the concrete contract/abi pinning bundle.
	Digest string `json:"digest"`

	// SideEffectClass is "chain" by definition; recorded for the
	// capability gate without special-casing.
	SideEffectClass string `json:"side_effect_class"`
}

// LoadAgentManifest reads and validates a manifest from a JSON file.
func LoadAgentManifest(path string) (*AgentManifest, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("tool: resolve manifest path: %w", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("tool: read manifest %s: %w", abs, err)
	}
	return ParseAgentManifest(data)
}

// ParseAgentManifest decodes + validates a manifest from raw JSON.
func ParseAgentManifest(data []byte) (*AgentManifest, error) {
	var m AgentManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("tool: parse manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate enforces Q17/Q18/Q19/Q21/Q22 invariants. Returns the first
// failure; callers may surface as-is to fail manifest load.
func (m *AgentManifest) Validate() error {
	if m.SchemaVersion != 1 {
		return fmt.Errorf("tool: unsupported manifest schema_version %d (want 1)", m.SchemaVersion)
	}
	if m.Agent == "" {
		return fmt.Errorf("tool: manifest missing agent")
	}
	for _, c := range m.AllowedSideEffects {
		if !ValidSideEffectClasses[c] {
			return fmt.Errorf("%w: allowed_side_effects has %q", ErrInvalidSideEffect, c)
		}
	}
	aliases := make(map[string]bool, len(m.Servers))
	for i := range m.Servers {
		s := &m.Servers[i]
		if err := s.Validate(); err != nil {
			return fmt.Errorf("tool: server[%d]: %w", i, err)
		}
		if aliases[s.Alias] {
			return fmt.Errorf("tool: duplicate server alias %q", s.Alias)
		}
		aliases[s.Alias] = true
	}
	for i := range m.NativeTools {
		nt := &m.NativeTools[i]
		if err := nt.Validate(); err != nil {
			return fmt.Errorf("tool: native_tools[%d]: %w", i, err)
		}
	}
	return nil
}

// Validate enforces ServerEntry invariants.
func (s *ServerEntry) Validate() error {
	if s.Alias == "" {
		return fmt.Errorf("missing alias")
	}
	if !validAlias(s.Alias) {
		return fmt.Errorf("alias %q must be lowercase letters+digits+'-' only", s.Alias)
	}
	switch s.Transport {
	case "stdio":
		if s.Command == "" {
			return fmt.Errorf("stdio server %q missing command", s.Alias)
		}
	case "http":
		if s.Endpoint == "" {
			return fmt.Errorf("http server %q missing endpoint", s.Alias)
		}
	case "":
		return fmt.Errorf("server %q missing transport", s.Alias)
	default:
		return fmt.Errorf("server %q unsupported transport %q", s.Alias, s.Transport)
	}
	if s.Version == "" {
		return fmt.Errorf("server %q missing version (S4 hard rule)", s.Alias)
	}
	if s.PackageDigest == "" {
		return fmt.Errorf("server %q missing package_digest (Q22 hard rule)", s.Alias)
	}
	if !validDigest(s.PackageDigest) {
		return fmt.Errorf("server %q package_digest must be sha256:<64-hex>", s.Alias)
	}
	if len(s.Tools) == 0 {
		return fmt.Errorf("server %q declares no tools (Q21 manifest must enumerate)", s.Alias)
	}
	toolNames := make(map[string]bool, len(s.Tools))
	for i := range s.Tools {
		te := &s.Tools[i]
		if err := te.Validate(); err != nil {
			return fmt.Errorf("tool[%d]: %w", i, err)
		}
		if toolNames[te.Name] {
			return fmt.Errorf("server %q duplicate tool %q", s.Alias, te.Name)
		}
		toolNames[te.Name] = true
	}
	return nil
}

// Validate enforces ToolEntry invariants.
func (te *ToolEntry) Validate() error {
	if te.Name == "" {
		return fmt.Errorf("missing name")
	}
	if te.SideEffectClass == "" {
		return fmt.Errorf("tool %q missing side_effect_class", te.Name)
	}
	if err := validateSideEffect(te.SideEffectClass); err != nil {
		return fmt.Errorf("tool %q: %w", te.Name, err)
	}
	if te.TimeoutMs < 0 {
		return fmt.Errorf("tool %q negative timeout_ms", te.Name)
	}
	return nil
}

// Validate enforces NativeToolEntry invariants.
func (nt *NativeToolEntry) Validate() error {
	if nt.Namespace == "" || nt.Name == "" || nt.Version == "" || nt.Digest == "" {
		return fmt.Errorf("native tool requires namespace+name+version+digest")
	}
	if !validNativeNamespace(nt.Namespace) {
		return fmt.Errorf("native tool namespace %q not in S6 closed set", nt.Namespace)
	}
	if !validDigest(nt.Digest) {
		return fmt.Errorf("native tool digest must be sha256:<64-hex>")
	}
	if nt.SideEffectClass != "" {
		if err := validateSideEffect(nt.SideEffectClass); err != nil {
			return err
		}
	}
	return nil
}

// validNativeNamespaces is the closed S6 set per research/05.
var validNativeNamespaces = map[string]bool{
	"argus":    true,
	"orob":     true,
	"plv":      true,
	"pofq":     true,
	"registry": true,
	"payments": true,
	"attest":   true,
	"chain":    true,
}

func validNativeNamespace(ns string) bool { return validNativeNamespaces[ns] }

// validAlias permits lowercase letters, digits, hyphens. No '/' so
// the URI parser can split unambiguously.
func validAlias(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// validDigest checks the "sha256:<64-hex>" form. Strict to keep
// chain-attestation surface clean.
func validDigest(d string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(d, prefix) {
		return false
	}
	rest := d[len(prefix):]
	if len(rest) != 64 {
		return false
	}
	for _, r := range rest {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// ResolveEnv expands "$env:NAME" tokens against the supplied lookup
// (typically os.LookupEnv). Used by the manager when spawning servers.
//
// Returns the expanded string and ok=false when a reference fails to
// resolve. Literal values pass through unchanged.
func ResolveEnv(token string, lookup func(string) (string, bool)) (string, bool) {
	const prefix = "$env:"
	if !strings.HasPrefix(token, prefix) {
		return token, true
	}
	name := token[len(prefix):]
	v, ok := lookup(name)
	return v, ok
}

// ResolveEnvList is the slice form: resolves every entry. Returns a
// new slice plus the first unresolved name (if any).
func ResolveEnvList(in []string, lookup func(string) (string, bool)) ([]string, string, error) {
	out := make([]string, 0, len(in))
	for _, e := range in {
		// "KEY=$env:NAME" or "KEY=value" or "$env:NAME" alone.
		if eq := strings.IndexByte(e, '='); eq >= 0 {
			k, v := e[:eq], e[eq+1:]
			vv, ok := ResolveEnv(v, lookup)
			if !ok {
				return nil, v, fmt.Errorf("tool: unresolved env reference %q", v)
			}
			out = append(out, k+"="+vv)
			continue
		}
		vv, ok := ResolveEnv(e, lookup)
		if !ok {
			return nil, e, fmt.Errorf("tool: unresolved env reference %q", e)
		}
		out = append(out, vv)
	}
	return out, "", nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
