// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package work

import (
	"context"
	"fmt"
	"strings"

	neotools "matrix/neo/internal/tools"
)

func (service *Service) StartSubagents(
	ctx context.Context,
	actorID string,
	conversationID string,
	plan ParentPlan,
	specs []neotools.SubagentSpec,
) (SupervisorRun, error) {
	if len(specs) < 1 || len(specs) > maxTasks {
		return SupervisorRun{}, fmt.Errorf(
			"runtime work: one or more bounded subagents are required",
		)
	}
	tasks := make([]TaskSpec, 0, len(specs))
	for index, spec := range specs {
		name := strings.TrimSpace(spec.Name)
		persona := strings.TrimSpace(spec.Persona)
		instruction := strings.TrimSpace(spec.Task)
		if name == "" || instruction == "" {
			return SupervisorRun{}, fmt.Errorf(
				"runtime work: subagent name and task are required",
			)
		}
		id := fmt.Sprintf("subagent-%02d-%s", index+1, slug(name))
		candidate := TaskSpec{
			ID: id, Title: persona + " " + name,
			Prompt: instruction, Criteria: []string{id},
			Scope: normalizeScope(plan.Scope),
		}
		candidate.Scope.EnvironmentKeys = nil
		candidate.Specialist = inferSpecialist(candidate)
		for _, toolName := range trimUnique(plan.Tools) {
			class, ok := service.metadata.SideEffectClass(toolName)
			if !ok || !specialistUsesClass(candidate.Specialist, class) {
				continue
			}
			if authorizeClass(candidate.Scope, class) == nil {
				candidate.Tools = append(candidate.Tools, toolName)
			}
		}
		if len(candidate.Tools) == 0 {
			return SupervisorRun{}, fmt.Errorf(
				"%w: subagent %q has no least-authority tool subset",
				ErrAuthorityDenied, name,
			)
		}
		tasks = append(tasks, candidate)
	}
	return service.Start(
		ctx, actorID,
		StartInput{
			ConversationID: conversationID,
			Plan:           plan,
			Tasks:          tasks,
		},
	)
}

func specialistUsesClass(specialist Specialist, class string) bool {
	switch strings.TrimSpace(class) {
	case "read":
		return true
	case "write":
		return specialist == SpecialistImplementation ||
			specialist == SpecialistFrontend ||
			specialist == SpecialistData ||
			specialist == SpecialistOperations
	case "shell":
		return specialist == SpecialistImplementation ||
			specialist == SpecialistTest ||
			specialist == SpecialistPerformance ||
			specialist == SpecialistOperations
	case "network":
		return specialist == SpecialistDiscovery ||
			specialist == SpecialistExploration ||
			specialist == SpecialistSecurity ||
			specialist == SpecialistOperations
	default:
		return specialist == SpecialistOperations
	}
}

func slug(value string) string {
	var builder strings.Builder
	for _, current := range strings.ToLower(value) {
		switch {
		case current >= 'a' && current <= 'z',
			current >= '0' && current <= '9':
			builder.WriteRune(current)
		case builder.Len() > 0:
			if text := builder.String(); text[len(text)-1] != '-' {
				builder.WriteByte('-')
			}
		}
		if builder.Len() >= 48 {
			break
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "specialist"
	}
	return result
}
