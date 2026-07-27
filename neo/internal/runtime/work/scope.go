// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package work

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"matrix/neo/internal/runtime/loop"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/tools"
)

type ManifestToolMetadata struct {
	Manager *tools.Manager
}

func (metadata ManifestToolMetadata) SideEffectClass(
	name string,
) (string, bool) {
	if metadata.Manager == nil {
		return "", false
	}
	return metadata.Manager.ToolSideEffectClass(name)
}

type ScopedToolManager struct {
	parent   ToolManager
	packet   TaskPacket
	allowed  map[string]struct{}
	metadata ToolMetadata
}

func NewScopedToolManager(
	parent ToolManager,
	packet TaskPacket,
	metadata ToolMetadata,
) (*ScopedToolManager, error) {
	if parent == nil || metadata == nil {
		return nil, fmt.Errorf(
			"runtime work: tools and manifest metadata are required",
		)
	}
	if err := verifyPacketDigest(packet); err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(packet.Tools))
	for _, name := range packet.Tools {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("runtime work: empty scoped tool")
		}
		allowed[name] = struct{}{}
	}
	return &ScopedToolManager{
		parent: parent, packet: packet, allowed: allowed, metadata: metadata,
	}, nil
}

func (manager *ScopedToolManager) Surface(
	ctx context.Context,
) []protocol.ToolDefinition {
	source := manager.parent.Surface(ctx)
	result := make([]protocol.ToolDefinition, 0, len(source))
	for _, definition := range source {
		if _, ok := manager.allowed[definition.Name]; ok {
			result = append(result, definition)
		}
	}
	return result
}

func (manager *ScopedToolManager) Execute(
	ctx context.Context,
	call protocol.NormalizedToolCall,
	idempotencyKey string,
) (loop.ToolResult, error) {
	if _, ok := manager.allowed[call.Name]; !ok {
		return loop.ToolResult{}, fmt.Errorf(
			"%w: tool %q is outside the immutable task packet",
			ErrAuthorityDenied, call.Name,
		)
	}
	class, found := manager.metadata.SideEffectClass(call.Name)
	if !found || class != manager.packet.ToolClasses[call.Name] {
		return loop.ToolResult{}, fmt.Errorf(
			"%w: tool %q manifest authority changed",
			ErrAuthorityDenied, call.Name,
		)
	}
	if err := authorizeClass(manager.packet.Scope, class); err != nil {
		return loop.ToolResult{}, fmt.Errorf(
			"%w: %s: %v", ErrAuthorityDenied, call.Name, err,
		)
	}
	return manager.parent.Execute(ctx, call, idempotencyKey)
}

func (manager *ScopedToolManager) Reconcile(
	ctx context.Context,
	idempotencyKey string,
) (loop.ReconcileResult, error) {
	return manager.parent.Reconcile(ctx, idempotencyKey)
}

func authorizeClass(scope AuthorityScope, class string) error {
	switch strings.TrimSpace(class) {
	case "read":
		if len(scope.ReadFiles) == 0 && len(scope.Services) == 0 &&
			len(scope.NetworkHosts) == 0 {
			return fmt.Errorf("read authority was not granted")
		}
		return nil
	case "write":
		if len(scope.WriteFiles) == 0 || !scope.ExternalEffects {
			return fmt.Errorf("write authority was not granted")
		}
	case "shell":
		if len(scope.Services) == 0 || !scope.ExternalEffects {
			return fmt.Errorf("process authority was not granted")
		}
	case "network":
		if len(scope.NetworkHosts) == 0 || !scope.ExternalEffects {
			return fmt.Errorf("network authority was not granted")
		}
	default:
		if !scope.ExternalEffects {
			return fmt.Errorf("external-effect authority was not granted")
		}
	}
	return nil
}

func normalizeScope(scope AuthorityScope) AuthorityScope {
	scope.ReadFiles = trimUnique(scope.ReadFiles)
	scope.WriteFiles = trimUnique(scope.WriteFiles)
	scope.Services = trimUnique(scope.Services)
	scope.EnvironmentKeys = trimUnique(scope.EnvironmentKeys)
	scope.NetworkHosts = trimUnique(scope.NetworkHosts)
	return scope
}

func scopeSubset(child AuthorityScope, parent AuthorityScope) bool {
	child, parent = normalizeScope(child), normalizeScope(parent)
	return valuesSubset(child.ReadFiles, parent.ReadFiles, "workspace") &&
		valuesSubset(child.WriteFiles, parent.WriteFiles, "workspace") &&
		valuesSubset(child.Services, parent.Services, "*") &&
		valuesSubset(
			child.EnvironmentKeys, parent.EnvironmentKeys, "*",
		) &&
		valuesSubset(child.NetworkHosts, parent.NetworkHosts, "*") &&
		(!child.ExternalEffects || parent.ExternalEffects)
}

func valuesSubset(child []string, parent []string, wildcard string) bool {
	allowed := make(map[string]struct{}, len(parent))
	for _, value := range parent {
		allowed[value] = struct{}{}
	}
	if _, ok := allowed[wildcard]; ok {
		return true
	}
	for _, value := range child {
		if _, ok := allowed[value]; !ok {
			return false
		}
	}
	return true
}

func trimUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func verifyPacketDigest(packet TaskPacket) error {
	expected := packet.Digest
	packet.Digest = [32]byte{}
	raw, err := json.Marshal(packet)
	if err != nil {
		return err
	}
	if digestPacket(raw) != expected {
		return fmt.Errorf("runtime work: immutable task packet changed")
	}
	return nil
}
