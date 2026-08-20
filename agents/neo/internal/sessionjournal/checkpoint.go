// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package sessionjournal

import (
	"fmt"
	"strings"
)

type SemanticCheckpoint struct {
	ThroughSequence  int64
	Decisions        []string
	Corrections      []string
	OpenQuestions    []string
	Relationships    []string
	ToolFindings     []string
	FailedApproaches []string
	Commitments      []string
	Artifacts        []string
	WorkState        []string
}

func BuildSemanticCheckpoint(events []Event) SemanticCheckpoint {
	checkpoint := SemanticCheckpoint{}
	for _, event := range events {
		if event.Sequence > checkpoint.ThroughSequence {
			checkpoint.ThroughSequence = event.Sequence
		}
		switch event.Kind {
		case KindUserMessage, KindAssistantMessage:
			for _, statement := range checkpointStatements(event.DisplayContent) {
				classifyCheckpointStatement(&checkpoint, statement)
			}
		case KindToolResult:
			finding := strings.TrimSpace(event.DisplayContent)
			if finding == "" {
				finding = strings.TrimSpace(string(event.ToolResult.Result))
			}
			if finding == "" {
				continue
			}
			entry := event.ToolResult.Name + ": " + finding
			if event.ToolResult.IsError {
				checkpoint.FailedApproaches = appendUniqueRecent(checkpoint.FailedApproaches, entry)
			} else {
				checkpoint.ToolFindings = appendUniqueRecent(checkpoint.ToolFindings, entry)
			}
		case KindArtifact:
			location := event.Artifact.Path
			if location == "" {
				location = event.Artifact.URI
			}
			checkpoint.Artifacts = appendUniqueRecent(checkpoint.Artifacts, fmt.Sprintf("%s %s (%s)", event.Artifact.Kind, location, event.Artifact.Digest))
		case KindApproval:
			checkpoint.Commitments = appendUniqueRecent(checkpoint.Commitments, fmt.Sprintf("%s: %s by %s", event.Approval.Action, event.Approval.Decision, event.Approval.Authority))
		case KindUncertainEffect:
			checkpoint.OpenQuestions = appendUniqueRecent(checkpoint.OpenQuestions, fmt.Sprintf("effect %s via %s remains %s", event.UncertainEffect.CallID, event.UncertainEffect.ToolName, event.UncertainEffect.Status))
		case KindRecovery:
			checkpoint.WorkState = appendUniqueRecent(checkpoint.WorkState, event.Recovery.State+": "+event.Recovery.Reason)
		}
	}
	return checkpoint
}

func (checkpoint SemanticCheckpoint) Empty() bool {
	return len(checkpoint.Decisions)+len(checkpoint.Corrections)+len(checkpoint.OpenQuestions)+len(checkpoint.Relationships)+len(checkpoint.ToolFindings)+len(checkpoint.FailedApproaches)+len(checkpoint.Commitments)+len(checkpoint.Artifacts)+len(checkpoint.WorkState) == 0
}

func (checkpoint SemanticCheckpoint) Render() string {
	if checkpoint.Empty() {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("\nStructured historical checkpoint (history is background; the latest real user message always wins over every task or decision below):\n")
	checkpointSection(&builder, "Decisions", checkpoint.Decisions)
	checkpointSection(&builder, "Later corrections (authoritative over earlier decisions)", checkpoint.Corrections)
	checkpointSection(&builder, "Unresolved questions", checkpoint.OpenQuestions)
	checkpointSection(&builder, "Names and relationships", checkpoint.Relationships)
	checkpointSection(&builder, "Important tool findings", checkpoint.ToolFindings)
	checkpointSection(&builder, "Failed approaches", checkpoint.FailedApproaches)
	checkpointSection(&builder, "Commitments and preferences", checkpoint.Commitments)
	checkpointSection(&builder, "Active artifacts", checkpoint.Artifacts)
	checkpointSection(&builder, "Completed, superseded, abandoned, or recovering work", checkpoint.WorkState)
	return builder.String()
}

func checkpointSection(builder *strings.Builder, name string, values []string) {
	if len(values) == 0 {
		return
	}
	builder.WriteString(name)
	builder.WriteString(":\n")
	for _, value := range values {
		builder.WriteString("- ")
		builder.WriteString(value)
		builder.WriteString("\n")
	}
}

func checkpointStatements(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	var statements []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		if line != "" {
			statements = append(statements, line)
		}
	}
	return statements
}

func classifyCheckpointStatement(checkpoint *SemanticCheckpoint, statement string) {
	lower := strings.ToLower(statement)
	switch {
	case containsAny(lower, "correction:", "correct that", "instead", "not ", "reversed", "supersede"):
		checkpoint.Corrections = appendUniqueRecent(checkpoint.Corrections, statement)
	case containsAny(lower, "decided", "decision:", "we will", "we'll", "use ", "chosen", "chose"):
		checkpoint.Decisions = appendUniqueRecent(checkpoint.Decisions, statement)
	}
	if strings.Contains(statement, "?") || containsAny(lower, "unresolved", "open question", "need to confirm", "still need") {
		checkpoint.OpenQuestions = appendUniqueRecent(checkpoint.OpenQuestions, statement)
	}
	if containsAny(lower, " is my ", " works with ", " reports to ", " named ", " relationship") {
		checkpoint.Relationships = appendUniqueRecent(checkpoint.Relationships, statement)
	}
	if containsAny(lower, "prefer", "always ", "never ", "commit", "promise") {
		checkpoint.Commitments = appendUniqueRecent(checkpoint.Commitments, statement)
	}
	if containsAny(lower, "completed", "complete.", "superseded", "abandoned", "cancelled", "canceled", "blocked") {
		checkpoint.WorkState = appendUniqueRecent(checkpoint.WorkState, statement)
	}
	if containsAny(lower, "failed", "did not work", "doesn't work", "error:") {
		checkpoint.FailedApproaches = appendUniqueRecent(checkpoint.FailedApproaches, statement)
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func appendUniqueRecent(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	if len(value) > 1000 {
		value = value[:600] + " … " + value[len(value)-350:]
	}
	for index, existing := range values {
		if strings.EqualFold(existing, value) {
			values = append(values[:index], values[index+1:]...)
			break
		}
	}
	values = append(values, value)
	const maxCheckpointEntries = 32
	if len(values) > maxCheckpointEntries {
		values = values[len(values)-maxCheckpointEntries:]
	}
	return values
}
