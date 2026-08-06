package loop

import (
	"context"
	"encoding/json"
	"testing"

	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/turnstate"
)

type milestoneReporter struct {
	deltas     []recordedDelta
	milestones []string
}

func (reporter *milestoneReporter) Delta(turn int, channel, text string) {
	reporter.deltas = append(reporter.deltas, recordedDelta{Turn: turn, Channel: channel, Text: text})
}

func (reporter *milestoneReporter) Progress(text string) {
	reporter.milestones = append(reporter.milestones, text)
}

func TestReporterObserverSuppressesToolNarrationAndEmitsMilestone(t *testing.T) {
	reporter := &milestoneReporter{}
	observer := NewReporterObserver(reporter, 0)
	if err := observer.ContentDelta(context.Background(), "Continuing from where I left off."); err != nil {
		t.Fatal(err)
	}
	if err := observer.CommitToolAttempt(context.Background(), []protocol.NormalizedToolCall{{
		ID: "write-1", Name: "patch_files", Arguments: json.RawMessage(`{"changes":[]}`),
	}}); err != nil {
		t.Fatal(err)
	}
	if len(reporter.deltas) != 2 || reporter.deltas[0].Channel != "content" ||
		reporter.deltas[0].Text == "" || reporter.deltas[1].Channel != "retraction" {
		t.Fatalf("provisional narration was not immediately emitted then retracted: %+v", reporter.deltas)
	}
	if len(reporter.milestones) != 1 || reporter.milestones[0] != "Updating project files." {
		t.Fatalf("milestones = %#v", reporter.milestones)
	}
}

func TestCodingCheckpointTracksMutationAndVerification(t *testing.T) {
	runtimeLoop := &Loop{config: Config{ProjectRoot: "/workspace/project"}}
	checkpoint := turnstate.Checkpoint{}
	runtimeLoop.initializeCodingCheckpoint(&checkpoint, "Build the dispatcher and verify restart recovery.")
	runtimeLoop.updateCodingCheckpoint(&checkpoint, protocol.NormalizedToolCall{
		ID: "patch-1", Name: "patch_files",
		Arguments: json.RawMessage(`{"changes":[{"path":"a.go","content":"package a"}]}`),
	}, ToolResult{Content: json.RawMessage(`{"applied":true,"files":[{"path":"a.go"},{"path":"b.go"}]}`)})
	if checkpoint.Coding == nil || len(checkpoint.Coding.FilesChanged) != 2 || checkpoint.Coding.NextAction != "Run the relevant project verification." {
		t.Fatalf("mutation checkpoint = %+v", checkpoint.Coding)
	}
	runtimeLoop.updateCodingCheckpoint(&checkpoint, protocol.NormalizedToolCall{
		ID: "verify-1", Name: "shell",
		Arguments: json.RawMessage(`{"command":"go test ./..."}`),
	}, ToolResult{Content: json.RawMessage(`{"cwd":"/workspace/project","exit_code":1,"timed_out":false,"stdout":{"text":""},"stderr":{"text":"worker test failed"}}`)})
	if checkpoint.Coding.LastVerification == nil || checkpoint.Coding.LastVerification.Succeeded ||
		len(checkpoint.Coding.CurrentFailures) != 1 || checkpoint.Coding.CurrentFailures[0] != "worker test failed" ||
		checkpoint.Coding.NextAction != "Correct the reported verification failures." {
		t.Fatalf("verification checkpoint = %+v", checkpoint.Coding)
	}
	messages := appendCodingCheckpoint([]protocol.Message{{Role: protocol.RoleUser, Content: "do it"}}, checkpoint.Coding)
	if len(messages) != 2 || messages[0].Role != protocol.RoleSystem || messages[1].Role != protocol.RoleUser {
		t.Fatalf("checkpoint authority ordering = %+v", messages)
	}
}

func TestRuntimeExpectSchemaIsProbeOnly(t *testing.T) {
	for _, test := range []struct {
		name string
		want bool
	}{
		{name: "web-search__web_search", want: true},
		{name: "fetch__fetch", want: true},
		{name: "shell", want: false},
		{name: "write_file", want: false},
		{name: "search_files", want: false},
	} {
		definition := protocol.ToolDefinition{Name: test.name, Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}
		if err := injectExpect(&definition); err != nil {
			t.Fatal(err)
		}
		var schema map[string]interface{}
		if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		properties := schema["properties"].(map[string]interface{})
		_, has := properties["expect"]
		if has != test.want {
			t.Fatalf("%s expect presence = %v, want %v", test.name, has, test.want)
		}
	}
}
