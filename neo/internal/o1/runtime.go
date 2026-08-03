// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"matrix/neo/internal/llm"
)

// RuntimeCapabilities is the result of compiling the live advertised schema
// set against a task contract. The selected schemas are the only operations
// that may be sent to the provider for the turn.
type RuntimeCapabilities struct {
	Tools     []llm.Tool
	Manifests []OperationManifest
}

// CompileRuntimeCapabilities derives manifests from the live, already-bound
// production tool schemas and selects the smallest contract-authorized set.
// A schema that cannot be represented as a bounded object is rejected before
// it can reach the model.
func CompileRuntimeCapabilities(contract TaskContract, available []llm.Tool) (RuntimeCapabilities, error) {
	if !contract.ReadyForMutation() && len(contract.RequiredCapabilities) > 0 {
		return RuntimeCapabilities{}, fmt.Errorf("task contract is not ready for mutation")
	}
	byName := make(map[string]llm.Tool, len(available))
	ms := &ManifestSet{manifests: make(map[string]OperationManifest, len(available))}
	for _, schema := range available {
		if err := validateToolSchema(schema); err != nil {
			return RuntimeCapabilities{}, err
		}
		name := schema.Function.Name
		if _, exists := byName[name]; exists {
			return RuntimeCapabilities{}, fmt.Errorf("duplicate live tool schema %q", name)
		}
		byName[name] = schema
		ms.manifests[name] = manifestFromTool(schema)
	}
	selected := ms.SelectMinimal(contract.RequiredCapabilities)
	for _, manifest := range selectExplicitProvider(contract.Objective.Text, ms) {
		if !hasManifest(selected, manifest.Operation) {
			selected = append(selected, manifest)
		}
	}
	for _, name := range []string{"read_overflow", "memory_recall", "todo"} {
		manifest := ms.Get(name)
		if manifest == nil || hasManifest(selected, name) {
			continue
		}
		selected = append(selected, *manifest)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Operation < selected[j].Operation })
	env := PreflightEnvironment{
		InstalledTools: make(map[string]bool, len(available)),
		// available is the already-bound live schema surface. Reaching this
		// compiler means the owning MCP process/registry connection exists;
		// endpoint-level failures remain typed operation outcomes.
		NetworkReachable: true,
	}
	for name := range byName {
		env.InstalledTools[name] = true
	}
	for _, manifest := range selected {
		result := RunPreflight(env, manifest)
		if !result.OK {
			return RuntimeCapabilities{}, fmt.Errorf("operation %q failed deterministic preflight: %s",
				manifest.Operation, strings.Join(result.Failed, ", "))
		}
	}
	result := RuntimeCapabilities{Manifests: selected}
	for _, manifest := range selected {
		result.Tools = append(result.Tools, byName[manifest.Operation])
	}
	return result, nil
}

// CompileAllCapabilities is the FULL-SURFACE compilation: it derives an
// OperationManifest for every live, already-bound tool schema and exposes them
// all, without the task-contract minimal narrowing. The O1 evidence machinery
// (typed manifests, deterministic preflight over the whole surface) still runs,
// but no tool is hidden from the model — the request->capability text heuristic
// cannot silently strip an essential tool (e.g. fetch) it failed to name. Used
// when O1ConstrainTools is off (the default). A schema that cannot be
// represented as a bounded object is still rejected before it can reach the
// model.
func CompileAllCapabilities(available []llm.Tool) (RuntimeCapabilities, error) {
	byName := make(map[string]llm.Tool, len(available))
	env := PreflightEnvironment{
		InstalledTools:   make(map[string]bool, len(available)),
		NetworkReachable: true,
	}
	var selected []OperationManifest
	for _, schema := range available {
		if err := validateToolSchema(schema); err != nil {
			return RuntimeCapabilities{}, err
		}
		name := schema.Function.Name
		if _, exists := byName[name]; exists {
			return RuntimeCapabilities{}, fmt.Errorf("duplicate live tool schema %q", name)
		}
		byName[name] = schema
		env.InstalledTools[name] = true
		selected = append(selected, manifestFromTool(schema))
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Operation < selected[j].Operation })
	for _, manifest := range selected {
		result := RunPreflight(env, manifest)
		if !result.OK {
			return RuntimeCapabilities{}, fmt.Errorf("operation %q failed deterministic preflight: %s",
				manifest.Operation, strings.Join(result.Failed, ", "))
		}
	}
	result := RuntimeCapabilities{Manifests: selected}
	for _, manifest := range selected {
		result.Tools = append(result.Tools, byName[manifest.Operation])
	}
	return result, nil
}

func selectExplicitProvider(objective string, manifests *ManifestSet) []OperationManifest {
	if manifests == nil {
		return nil
	}
	lower := strings.ToLower(objective)
	providers := map[string]bool{}
	for _, operation := range manifests.Operations() {
		alias, _, ok := strings.Cut(strings.ToLower(operation), "__")
		if ok && alias != "" && requestContainsTerm(lower, alias) {
			providers[alias] = true
		}
	}
	var selected []OperationManifest
	for _, operation := range manifests.Operations() {
		alias, _, ok := strings.Cut(strings.ToLower(operation), "__")
		if !ok || !providers[alias] {
			continue
		}
		if manifest := manifests.Get(operation); manifest != nil {
			selected = append(selected, *manifest)
		}
	}
	return selected
}

func hasManifest(manifests []OperationManifest, operation string) bool {
	for _, manifest := range manifests {
		if manifest.Operation == operation {
			return true
		}
	}
	return false
}

// ConformChatResult validates the production provider result against the O1
// normalized protocol and representation budget before any call can dispatch.
func ConformChatResult(result *llm.ChatResult, budget RepresentationBudget, dialect ...ProviderDialect) error {
	if result == nil {
		return fmt.Errorf("nil provider result")
	}
	wireDialect := DialectGeneric
	if len(dialect) > 0 {
		wireDialect = dialect[0]
	}
	generation := &NormalizedGeneration{
		Content: result.Message.Content, Reasoning: result.Message.Reasoning,
		FinishReason: result.FinishReason,
		Usage: Usage{
			PromptTokens: result.Usage.PromptTokens, CompletionTokens: result.Usage.CompletionTokens,
			TotalTokens: result.Usage.TotalTokens,
		},
		Dialect: wireDialect,
	}
	for _, call := range result.Message.ToolCalls {
		generation.ToolCalls = append(generation.ToolCalls, NormalizedToolCall{
			ID: call.ID, Name: call.Function.Name, Arguments: json.RawMessage(call.Function.Arguments),
			RawDialect: wireDialect,
		})
	}
	if err := NewProviderConformer(wireDialect).Validate(generation); err != nil {
		return err
	}
	return budget.BudgetCheck(generation)
}

func DialectForProvider(provider string) ProviderDialect {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "fireworks":
		return DialectFireworks
	case "baseten":
		return DialectBaseten
	case "xai":
		return DialectXAI
	case "xiaomi":
		return DialectMimo
	default:
		return DialectOpenAI
	}
}

func validateToolSchema(schema llm.Tool) error {
	if schema.Type != "function" || strings.TrimSpace(schema.Function.Name) == "" {
		return fmt.Errorf("invalid live tool schema: missing function identity")
	}
	if schema.Function.Parameters == nil || schema.Function.Parameters["type"] != "object" {
		return fmt.Errorf("tool %q has an unbounded non-object input schema", schema.Function.Name)
	}
	if _, err := json.Marshal(schema.Function.Parameters); err != nil {
		return fmt.Errorf("tool %q has an invalid input schema: %w", schema.Function.Name, err)
	}
	return nil
}

func manifestFromTool(schema llm.Tool) OperationManifest {
	name := schema.Function.Name
	effect := EffectReadOnly
	reversibility := Reversible
	lower := strings.ToLower(name)
	switch {
	case containsAny(lower, "delete", "remove", "drop", "rollback"):
		effect, reversibility = EffectDestructive, Conditional
	case lower == "build_project":
		effect, reversibility = EffectWrite, Conditional
	case containsAny(lower, "core_execute", "browser__click", "browser__fill", "browser__navigate", "fetch", "web-search"):
		effect, reversibility = EffectExternal, Conditional
	case containsAny(lower, "shell", "exec__", "service_", "move_file"):
		effect, reversibility = EffectWrite, Conditional
	case containsAny(lower, "write", "edit", "create", "append", "mutate", "send", "post", "schedule", "render"):
		effect, reversibility = EffectWrite, Conditional
	}
	if containsAny(lower, "pay", "transfer", "swap", "deposit", "withdraw") {
		effect, reversibility = EffectMoney, Irreversible
	}
	return OperationManifest{
		Operation:       name,
		Description:     schema.Function.Description,
		InputBounds:     schemaBounds(schema.Function.Parameters),
		Preconditions:   []string{"tool:" + name},
		Effects:         effect,
		Idempotency:     "operation_id",
		Reversibility:   reversibility,
		OutcomeVariants: []string{"success", "typed_failure"},
		Recovery: RecoverySpec{
			Initial: "initial", Allowed: []string{"probe", "reconcile", "stop"},
			Terminal: []string{"success", "typed_failure"}, Bound: 3,
		},
		Verifier:      "operation postconditions",
		ContextPolicy: "structured_result",
	}
}

func schemaBounds(parameters map[string]interface{}) map[string]string {
	out := map[string]string{}
	if required, ok := parameters["required"].([]interface{}); ok {
		var names []string
		for _, value := range required {
			if name, ok := value.(string); ok {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		out["required"] = strings.Join(names, ",")
	}
	if additional, ok := parameters["additionalProperties"].(bool); ok && !additional {
		out["additional_properties"] = "forbidden"
	}
	return out
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
