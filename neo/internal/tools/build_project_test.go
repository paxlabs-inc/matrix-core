package tools

import (
	"context"
	"strings"
	"testing"
)

func TestBuildProjectToolAdvertisesOnlyWhenWiredAndReturnsAccepted(t *testing.T) {
	m := &Manager{}
	if m.BuildProjectEnabled() {
		t.Fatal("Build tool enabled without a durable dispatcher")
	}
	for _, schema := range m.Schemas() {
		if schema.Function.Name == BuildProjectTool {
			t.Fatal("unwired Build tool was advertised")
		}
	}
	var received BuildRequest
	m.SetBuildProject(func(_ context.Context, request BuildRequest) (string, error) {
		received = request
		return "Build job neo_build_1 accepted.", nil
	})
	if !m.BuildProjectEnabled() {
		t.Fatal("Build tool remained disabled after wiring")
	}
	found := false
	for _, schema := range m.Schemas() {
		if schema.Function.Name == BuildProjectTool {
			found = true
		}
	}
	if !found {
		t.Fatal("wired Build tool was not advertised")
	}
	content, isError, err := m.Dispatch(context.Background(), BuildProjectTool, map[string]interface{}{
		"request":             "Build a responsive inventory app",
		"acceptance_criteria": []interface{}{"items can be created", "tests pass"},
		"constraints":         []interface{}{"do not deploy"},
	})
	if err != nil || isError || !strings.Contains(content, "neo_build_1") {
		t.Fatalf("dispatch = (%q, %v, %v)", content, isError, err)
	}
	if received.Request != "Build a responsive inventory app" || len(received.AcceptanceCriteria) != 2 || len(received.Constraints) != 1 {
		t.Fatalf("received request = %#v", received)
	}
}

func TestBuildProjectToolRejectsEmptyBrief(t *testing.T) {
	m := &Manager{}
	m.SetBuildProject(func(context.Context, BuildRequest) (string, error) {
		t.Fatal("empty brief reached dispatcher")
		return "", nil
	})
	content, isError, err := m.Dispatch(context.Background(), BuildProjectTool, map[string]interface{}{})
	if err != nil || !isError || content != "request is required" {
		t.Fatalf("dispatch = (%q, %v, %v)", content, isError, err)
	}
}
