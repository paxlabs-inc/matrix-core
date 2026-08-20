// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"centra/agents/neo/internal/runtime/protocol"
	"centra/agents/neo/internal/runtime/records"
)

type Reporter interface {
	Delta(turn int, channel, text string)
}

type ReporterObserver struct {
	mu       sync.Mutex
	reporter Reporter
	attempt  int
}

type observerStreamSink struct {
	observer GenerationObserver
}

func (sink observerStreamSink) Provisional(ctx context.Context, output records.StreamedOutput) error {
	if output.Channel == records.StreamReasoning {
		return sink.observer.ReasoningDelta(ctx, output.Content)
	}
	return sink.observer.ContentDelta(ctx, output.Content)
}

func (sink observerStreamSink) Commit(ctx context.Context, _ string) error {
	return sink.observer.CommitAttempt(ctx)
}

func (sink observerStreamSink) Retract(ctx context.Context, _ string) error {
	return sink.observer.Reset(ctx)
}

func NewReporterObserver(reporter Reporter, turn int) *ReporterObserver {
	return &ReporterObserver{reporter: reporter, attempt: turn}
}

func (observer *ReporterObserver) ContentDelta(
	_ context.Context,
	content string,
) error {
	observer.emit("content", content)
	return nil
}

func (observer *ReporterObserver) ReasoningDelta(
	_ context.Context,
	content string,
) error {
	observer.emit("reasoning", content)
	return nil
}

func (observer *ReporterObserver) Reset(context.Context) error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.reporter != nil {
		observer.reporter.Delta(observer.attempt, "retraction", "")
	}
	observer.attempt++
	return nil
}

func (observer *ReporterObserver) CommitAttempt(context.Context) error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.reporter != nil {
		observer.reporter.Delta(observer.attempt, "commit", "")
	}
	observer.attempt++
	return nil
}

// CommitToolAttempt drops all model-authored tool-call prose and emits one
// deterministic runtime milestone derived from the actual operation.
func (observer *ReporterObserver) CommitToolAttempt(_ context.Context, calls []protocol.NormalizedToolCall) error {
	if err := observer.Reset(context.Background()); err != nil {
		return err
	}
	observer.ObserveToolAttempt(calls)
	return nil
}

func (observer *ReporterObserver) ObserveToolAttempt(calls []protocol.NormalizedToolCall) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.reporter != nil {
		if progress, ok := observer.reporter.(interface{ Progress(string) }); ok {
			if milestone := runtimeMilestone(calls); milestone != "" {
				progress.Progress(milestone)
			}
		}
	}
}

// ObserveToolResult emits only deterministic failure milestones. It never
// includes model prose, command output, paths, or recovery language.
func (observer *ReporterObserver) ObserveToolResult(_ context.Context, call protocol.NormalizedToolCall, result ToolResult) {
	if !result.IsError && !shellResultFailed(result.Content) {
		return
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if progress, ok := observer.reporter.(interface{ Progress(string) }); ok {
		if runtimeToolClass(call) == "verify" {
			progress.Progress("Verification found issues; correcting them.")
		} else {
			progress.Progress("The last operation failed; adjusting the approach.")
		}
	}
}

func (observer *ReporterObserver) emit(channel, content string) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if content == "" {
		return
	}
	if observer.reporter != nil {
		observer.reporter.Delta(observer.attempt, channel, content)
	}
}

func runtimeMilestone(calls []protocol.NormalizedToolCall) string {
	classes := make(map[string]bool, len(calls))
	for _, call := range calls {
		classes[runtimeToolClass(call)] = true
	}
	for _, candidate := range []struct{ class, line string }{
		{"build", "Starting a durable project build."},
		{"verify", "Running project verification."},
		{"write", "Updating project files."},
		{"command", "Running a project command."},
		{"inspect", "Inspecting the project."},
		{"research", "Gathering source evidence."},
		{"act", "Applying the requested operation."},
	} {
		if classes[candidate.class] {
			return candidate.line
		}
	}
	return ""
}

func runtimeToolClass(call protocol.NormalizedToolCall) string {
	base := strings.ToLower(call.Name)
	if index := strings.LastIndex(base, "__"); index >= 0 {
		base = base[index+2:]
	}
	if base == "build_project" {
		return "build"
	}
	if base == "shell" || strings.Contains(base, "exec") || base == "run" || strings.Contains(base, "command") {
		var args map[string]interface{}
		_ = json.Unmarshal(call.Arguments, &args)
		command, _ := args["command"].(string)
		command = strings.ToLower(command)
		for _, marker := range []string{"go test", "go vet", "go build", " test", " lint", "typecheck", " check"} {
			if strings.Contains(" "+command, marker) {
				return "verify"
			}
		}
		return "command"
	}
	for _, marker := range []string{"write", "edit", "patch", "create_directory", "move_file", "delete", "remove"} {
		if strings.Contains(base, marker) {
			return "write"
		}
	}
	for _, marker := range []string{"read", "list", "tree", "search_files", "file_info", "git_", "shell_output"} {
		if strings.Contains(base, marker) {
			return "inspect"
		}
	}
	for _, marker := range []string{"web_search", "web_news", "fetch", "exa_", "navigate", "browser"} {
		if strings.Contains(base, marker) {
			return "research"
		}
	}
	if base == "todo" || base == "read_overflow" {
		return ""
	}
	return "act"
}

func shellResultFailed(content json.RawMessage) bool {
	var envelope map[string]interface{}
	if json.Unmarshal(content, &envelope) != nil {
		return false
	}
	if timedOut, _ := envelope["timed_out"].(bool); timedOut {
		return true
	}
	if exit, ok := envelope["exit_code"].(float64); ok && exit != 0 {
		return true
	}
	return false
}
