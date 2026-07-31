// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// EffectClass describes the material consequence of an operation.
type EffectClass string

const (
	EffectReadOnly     EffectClass = "read_only"
	EffectWrite        EffectClass = "write"
	EffectDestructive  EffectClass = "destructive"
	EffectExternal     EffectClass = "external"
	EffectMoney        EffectClass = "money"
	EffectNotification EffectClass = "notification"
)

// Reversibility describes whether an operation can be undone.
type Reversibility string

const (
	Reversible   Reversibility = "reversible"
	Irreversible Reversibility = "irreversible"
	Conditional  Reversibility = "conditional"
)

// OperationManifest fully describes a single tool operation's bounds,
// preconditions, dependencies, effects, outcomes, recovery, and verification.
// Per O1 req.3 ac.1.
type OperationManifest struct {
	Operation       string            `json:"operation"`
	Description     string            `json:"description"`
	InputBounds     map[string]string `json:"input_bounds"`
	Preconditions   []string          `json:"preconditions"`
	Dependencies    []string          `json:"dependencies"`
	Permissions     []string          `json:"permissions"`
	Effects         EffectClass       `json:"effects"`
	Idempotency     string            `json:"idempotency"`
	Reversibility   Reversibility     `json:"reversibility"`
	OutcomeVariants []string          `json:"outcome_variants"`
	Recovery        RecoverySpec      `json:"recovery"`
	Verifier        string            `json:"verifier"`
	ContextPolicy   string            `json:"context_policy"`
}

// RecoverySpec describes the bounded recovery automaton for an operation.
type RecoverySpec struct {
	Initial  string   `json:"initial"`
	Allowed  []string `json:"allowed"`
	Terminal []string `json:"terminal"`
	Bound    int      `json:"bound"`
}

// ManifestSet is the authoritative registry of all shipped operation manifests,
// keyed by operation name (alias__name for MCP tools, constant name for synthetics).
type ManifestSet struct {
	manifests map[string]OperationManifest
}

// LoadManifests reads a manifest JSON file and returns the set.
func LoadManifests(path string) (*ManifestSet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw []OperationManifest
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("decode manifests: %w", err)
	}
	ms := &ManifestSet{manifests: make(map[string]OperationManifest, len(raw))}
	for _, m := range raw {
		if m.Operation == "" {
			continue
		}
		ms.manifests[m.Operation] = m
	}
	return ms, nil
}

// Get returns the manifest for an operation, or nil if not registered.
func (ms *ManifestSet) Get(operation string) *OperationManifest {
	if ms == nil {
		return nil
	}
	m, ok := ms.manifests[operation]
	if !ok {
		return nil
	}
	return &m
}

// Operations returns all registered operation names in sorted order.
func (ms *ManifestSet) Operations() []string {
	if ms == nil {
		return nil
	}
	out := make([]string, 0, len(ms.manifests))
	for k := range ms.manifests {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SelectMinimal returns the operations needed by the task contract's required
// capabilities. Per O1 req.3 ac.2: the model sees only operations valid for
// the current contract state.
func (ms *ManifestSet) SelectMinimal(requiredCapabilities []string) []OperationManifest {
	if ms == nil {
		return nil
	}
	need := map[string]bool{}
	for _, cap := range requiredCapabilities {
		need[cap] = true
	}
	var out []OperationManifest
	for _, m := range ms.manifests {
		if matchesCapability(m.Operation, need) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Operation < out[j].Operation })
	return out
}

// capabilityGroups maps capability names to operation prefixes.
// Synthetic tools map to their constant names; MCP tools map by alias prefix.
var capabilityGroups = map[string][]string{
	"filesystem":        {"fs__", "read_file", "write_file", "edit_file", "delete_file", "list_directory"},
	"web_evidence":      {"web-search__", "exa__", "fetch__", "fetch", "search", "news"},
	"browser":           {"browser__"},
	"media":             {"media__"},
	"memory":            {"memory_recall", "memory_mutate", "write_skill"},
	"scheduling":        {"chronos__"},
	"chain_or_wallet":   {"paxeer-net__", "layerx__", "deus__", "kindle__"},
	"workspace_preview": {"workspace_preview", "sandbox__"},
	"execution":         {"core_execute", "exec__", "shell__"},
	"code_edit":         {"fs__write_file", "fs__edit_file", "fs__read_file"},
	"subagents":         {"spawn_subagents"},
	"construct":         {"construct_render"},
	"task_state":        {"todo"},
	"git":               {"git__"},
}

func matchesCapability(operation string, need map[string]bool) bool {
	if len(need) == 0 {
		return false
	}
	for cap, prefixes := range capabilityGroups {
		if !need[cap] {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(operation, prefix) || operation == prefix {
				return true
			}
		}
	}
	return false
}

// PreflightEnvironment describes the runtime state needed for preflight checks.
type PreflightEnvironment struct {
	InstalledTools     map[string]bool   `json:"installed_tools"`
	ToolVersions       map[string]string `json:"tool_versions"`
	CredentialsPresent map[string]bool   `json:"credentials_present"`
	Permissions        map[string]bool   `json:"permissions"`
	NetworkReachable   bool              `json:"network_reachable"`
	StorageAvailable   int64             `json:"storage_available_bytes"`
	MaxPayloadBytes    int64             `json:"max_payload_bytes"`
	ProjectRoot        string            `json:"project_root"`
	FrameworkVersion   string            `json:"framework_version"`
}

// PreflightResult is the deterministic outcome of a preflight check.
// Per O1 req.3 ac.3 and ac.4.
type PreflightResult struct {
	OK        bool     `json:"ok"`
	Operation string   `json:"operation"`
	Failed    []string `json:"failed_preconditions,omitempty"`
	Repairs   []string `json:"authorized_repairs,omitempty"`
}

// RunPreflight checks all manifest preconditions against the environment.
// Returns OK=true when the operation is safe to expose, or a list of failed
// preconditions. Per O1 req.3 ac.3: every knowable prerequisite is verified
// before the model attempts the operation.
func RunPreflight(env PreflightEnvironment, manifest OperationManifest) PreflightResult {
	result := PreflightResult{OK: true, Operation: manifest.Operation}
	for _, pre := range manifest.Preconditions {
		if !checkPrecondition(env, pre) {
			result.OK = false
			result.Failed = append(result.Failed, pre)
		}
	}
	for _, dep := range manifest.Dependencies {
		key := "dep:" + dep
		if !env.InstalledTools[dep] && !env.InstalledTools[key] {
			result.OK = false
			result.Failed = append(result.Failed, "missing_dependency:"+dep)
		}
	}
	for _, perm := range manifest.Permissions {
		if !env.Permissions[perm] {
			result.OK = false
			result.Failed = append(result.Failed, "missing_permission:"+perm)
		}
	}
	if manifest.Effects == EffectExternal && !env.NetworkReachable {
		result.OK = false
		result.Failed = append(result.Failed, "network_unreachable")
	}
	return result
}

func checkPrecondition(env PreflightEnvironment, pre string) bool {
	switch {
	case pre == "network_reachable":
		return env.NetworkReachable
	case pre == "project_root_exists":
		return env.ProjectRoot != ""
	case strings.HasPrefix(pre, "tool:"):
		toolName := strings.TrimPrefix(pre, "tool:")
		return env.InstalledTools[toolName]
	case strings.HasPrefix(pre, "credential:"):
		credName := strings.TrimPrefix(pre, "credential:")
		return env.CredentialsPresent[credName]
	case strings.HasPrefix(pre, "permission:"):
		permName := strings.TrimPrefix(pre, "permission:")
		return env.Permissions[permName]
	case strings.HasPrefix(pre, "version:"):
		// version:toolName:minVersion — tool must be installed
		parts := strings.SplitN(strings.TrimPrefix(pre, "version:"), ":", 2)
		if len(parts) == 1 {
			return env.InstalledTools[parts[0]]
		}
		ver, ok := env.ToolVersions[parts[0]]
		return ok && versionAtLeast(ver, parts[1])
	case strings.HasPrefix(pre, "storage:"):
		// storage:N — at least N bytes available
		required, err := strconv.ParseInt(strings.TrimPrefix(pre, "storage:"), 10, 64)
		return err == nil && required >= 0 && env.StorageAvailable >= required
	case strings.HasPrefix(pre, "payload:"):
		required, err := strconv.ParseInt(strings.TrimPrefix(pre, "payload:"), 10, 64)
		return err == nil && required >= 0 && env.MaxPayloadBytes >= required
	case strings.HasPrefix(pre, "framework:"):
		required := strings.TrimPrefix(pre, "framework:")
		return required != "" && versionAtLeast(env.FrameworkVersion, required)
	default:
		return false
	}
}

func versionAtLeast(actual, minimum string) bool {
	parse := func(v string) ([]int, bool) {
		v = strings.TrimSpace(strings.TrimPrefix(v, "v"))
		if v == "" {
			return nil, false
		}
		parts := strings.Split(v, ".")
		out := make([]int, len(parts))
		for i, part := range parts {
			if cut := strings.IndexAny(part, "-+"); cut >= 0 {
				part = part[:cut]
			}
			n, err := strconv.Atoi(part)
			if err != nil || part == "" {
				return nil, false
			}
			out[i] = n
		}
		return out, true
	}
	a, ok := parse(actual)
	if !ok {
		return false
	}
	m, ok := parse(minimum)
	if !ok {
		return false
	}
	n := len(a)
	if len(m) > n {
		n = len(m)
	}
	for i := 0; i < n; i++ {
		var av, mv int
		if i < len(a) {
			av = a[i]
		}
		if i < len(m) {
			mv = m[i]
		}
		if av != mv {
			return av > mv
		}
	}
	return true
}

// Validate checks a manifest set for completeness and consistency.
func (ms *ManifestSet) Validate() error {
	if ms == nil {
		return fmt.Errorf("nil manifest set")
	}
	var problems []string
	for name, m := range ms.manifests {
		if m.Operation == "" {
			problems = append(problems, name+": missing operation")
		}
		if m.Description == "" {
			problems = append(problems, name+": missing description")
		}
		if m.Effects == "" {
			problems = append(problems, name+": missing effects")
		}
		if m.Verifier == "" {
			problems = append(problems, name+": missing verifier")
		}
		if m.Recovery.Initial == "" && len(m.Recovery.Terminal) == 0 {
			// read_only ops may not need recovery
			if m.Effects != EffectReadOnly {
				problems = append(problems, name+": non-read-only op lacks recovery spec")
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("manifest validation failed:\n- %s", strings.Join(problems, "\n- "))
	}
	return nil
}
