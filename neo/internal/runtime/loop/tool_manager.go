// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"matrix/neo/internal/runtime/artifacts"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/turnstate"
	"matrix/neo/internal/tools"
)

type EffectReconciler interface {
	ReconcileEffect(context.Context, string) (ReconcileResult, error)
}

type EffectLifecycle interface {
	EffectReconciler
	BeginEffect(
		context.Context,
		string,
		string,
		json.RawMessage,
		bool,
	) error
	CompleteEffect(context.Context, string, ToolResult) error
}

type EffectPreparer interface {
	PrepareEffect(
		context.Context,
		protocol.NormalizedToolCall,
		string,
	) error
}

type EffectMetadata struct {
	SideEffectClass       string
	IdempotencyStrategy   string
	RequiredEvidence      string
	RetryStrategy         string
	ReconciliationHandler string
	RetrySafe             bool
}

type EffectMetadataProvider interface {
	EffectMetadata(protocol.NormalizedToolCall) (EffectMetadata, error)
}

type ToolManagerAdapter struct {
	manager    *tools.Manager
	reconciler EffectReconciler
	artifacts  *artifacts.Store
}

func (adapter *ToolManagerAdapter) SetArtifactStore(store *artifacts.Store) {
	adapter.artifacts = store
}

func NewToolManagerAdapter(
	manager *tools.Manager,
	reconciler EffectReconciler,
) (*ToolManagerAdapter, error) {
	if manager == nil {
		return nil, fmt.Errorf("runtime loop: tool manager is required")
	}
	return &ToolManagerAdapter{
		manager: manager, reconciler: reconciler,
	}, nil
}

func (adapter *ToolManagerAdapter) Surface(
	context.Context,
) []protocol.ToolDefinition {
	schemas := adapter.manager.Schemas()
	result := make([]protocol.ToolDefinition, 0, len(schemas))
	for _, schema := range schemas {
		parameters, err := json.Marshal(schema.Function.Parameters)
		if err != nil {
			continue
		}
		definition := protocol.ToolDefinition{
			Name:        schema.Function.Name,
			Description: schema.Function.Description,
			Parameters:  parameters,
		}
		if injectExpect(&definition) == nil {
			result = append(result, definition)
		}
	}
	return result
}

func (adapter *ToolManagerAdapter) Execute(
	ctx context.Context,
	call protocol.NormalizedToolCall,
	idempotencyKey string,
) (ToolResult, error) {
	var arguments map[string]interface{}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
		return ToolResult{}, fmt.Errorf(
			"runtime loop: decode %s arguments: %w", call.Name, err,
		)
	}
	if err := adapter.manager.ValidateAndResolve(call.Name, arguments); err != nil {
		return structuredToolFailure(
			"validation", false, "not_started", err.Error(),
			"Correct the arguments or choose another available operation; no dispatch occurred.",
		), nil
	}
	lifecycle, durable := adapter.reconciler.(EffectLifecycle)
	if durable {
		sideEffect, found := adapter.manager.ToolSideEffectClass(call.Name)
		if !found {
			return ToolResult{}, fmt.Errorf(
				"runtime loop: effect metadata unavailable for %s",
				call.Name,
			)
		}
		if err := lifecycle.BeginEffect(
			ctx,
			idempotencyKey,
			call.Name,
			call.Arguments,
			sideEffect == "read",
		); err != nil {
			return ToolResult{}, err
		}
	}
	content, _, isError, failureClass, retryable, failureMessage, err :=
		adapter.manager.DispatchMediaClassified(ctx, call.Name, arguments)
	result := ToolResult{
		Content:        normalizeToolResult(content),
		IsError:        isError,
		FailureClass:   string(failureClass),
		Retryable:      retryable,
		FailureMessage: strings.TrimSpace(failureMessage),
	}
	contentForFailure := content
	if adapter.artifacts != nil && len(content) > 32<<10 {
		mime := "text/plain"
		if json.Valid([]byte(strings.TrimSpace(content))) {
			mime = "application/json"
		}
		status := "completed"
		if isError {
			status = "failed"
		}
		_, projection, artifactErr := adapter.artifacts.Put(context.WithoutCancel(ctx), artifacts.Metadata{
			LogicalTurnID: turnstate.LogicalTurnFromContext(ctx), CycleIdentity: "current",
			CallIdentity: call.ID, Tool: call.Name, NormalizedArgs: call.Arguments,
			MIME: mime, EffectStatus: status,
		}, []byte(content))
		if artifactErr != nil {
			return ToolResult{}, fmt.Errorf("runtime loop: externalize tool result: %w", artifactErr)
		}
		encodedProjection, artifactErr := json.Marshal(projection)
		if artifactErr != nil {
			return ToolResult{}, artifactErr
		}
		result.Content = encodedProjection
		contentForFailure = string(encodedProjection)
	}
	if result.IsError && err == nil {
		layer := strings.TrimSpace(result.FailureClass)
		if layer == "" {
			layer = "application"
		}
		evidence := strings.TrimSpace(contentForFailure)
		if result.FailureMessage != "" {
			evidence = result.FailureMessage + ": " + evidence
		}
		recovery := "Change the arguments or choose another available operation."
		if result.Retryable {
			recovery = "Retry later with backoff or continue with an independent approach."
		}
		result = structuredToolFailure(
			layer, result.Retryable, "completed", evidence, recovery,
		)
		result.FailureClass = layer
	} else if err == nil {
		result = structuredToolSuccess(result.Content)
	}
	if err == nil && durable {
		if persistErr := lifecycle.CompleteEffect(
			context.WithoutCancel(ctx), idempotencyKey, result,
		); persistErr != nil {
			return ToolResult{}, persistErr
		}
	}
	return result, err
}

// PrepareEffect establishes durable effect identity before a PendingCall can
// enter the turn checkpoint. BeginEffect is idempotent, so Execute repeats this
// guard without opening a second effect.
func (adapter *ToolManagerAdapter) PrepareEffect(
	ctx context.Context,
	call protocol.NormalizedToolCall,
	idempotencyKey string,
) error {
	metadata, err := adapter.EffectMetadata(call)
	if err != nil {
		return err
	}
	lifecycle, durable := adapter.reconciler.(EffectLifecycle)
	if !durable {
		return nil
	}
	return lifecycle.BeginEffect(
		ctx, idempotencyKey, call.Name, call.Arguments, metadata.RetrySafe,
	)
}

func (adapter *ToolManagerAdapter) EffectMetadata(
	call protocol.NormalizedToolCall,
) (EffectMetadata, error) {
	var arguments map[string]interface{}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
		return EffectMetadata{}, fmt.Errorf(
			"runtime loop: decode %s arguments: %w", call.Name, err,
		)
	}
	if err := adapter.manager.ValidateAndResolve(call.Name, arguments); err != nil {
		return EffectMetadata{}, err
	}
	registered, found := adapter.manager.ToolEffectMetadata(call.Name)
	if !found {
		return EffectMetadata{}, fmt.Errorf(
			"runtime loop: effect metadata unavailable for %s", call.Name,
		)
	}
	return EffectMetadata{
		SideEffectClass:       registered.SideEffectClass,
		IdempotencyStrategy:   registered.IdempotencyStrategy,
		RequiredEvidence:      registered.RequiredEvidence,
		RetryStrategy:         registered.RetryStrategy,
		ReconciliationHandler: registered.ReconciliationHandler,
		RetrySafe:             registered.SideEffectClass == "read-only",
	}, nil
}

func (adapter *ToolManagerAdapter) Reconcile(
	ctx context.Context,
	idempotencyKey string,
) (ReconcileResult, error) {
	if adapter.reconciler == nil {
		return ReconcileResult{Status: ReconcileUnknown}, nil
	}
	return adapter.reconciler.ReconcileEffect(ctx, idempotencyKey)
}

func injectExpect(definition *protocol.ToolDefinition) error {
	if definition == nil {
		return fmt.Errorf("runtime loop: tool definition is required")
	}
	if !runtimeUncertainProbe(definition.Name) {
		return nil
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		return err
	}
	properties, _ := schema["properties"].(map[string]interface{})
	if properties == nil {
		properties = map[string]interface{}{}
		schema["properties"] = properties
	}
	properties["expect"] = map[string]interface{}{
		"type":        "string",
		"minLength":   1,
		"description": "One-line prediction of what this tool will return.",
	}
	required := stringSlice(schema["required"])
	if !containsString(required, "expect") {
		required = append(required, "expect")
	}
	schema["required"] = required
	encoded, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	definition.Parameters = encoded
	return nil
}

func normalizeToolResult(content string) json.RawMessage {
	trimmed := strings.TrimSpace(content)
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	encoded, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return json.RawMessage(`{"error":"tool result encoding failed"}`)
	}
	return encoded
}

func stringSlice(value interface{}) []string {
	switch current := value.(type) {
	case []string:
		return append([]string(nil), current...)
	case []interface{}:
		result := make([]string, 0, len(current))
		for _, item := range current {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
