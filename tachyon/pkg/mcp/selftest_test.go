package mcp

import "testing"

func TestSelftest(t *testing.T) {
	if err := Selftest(); err != nil {
		t.Fatal(err)
	}
}

func TestToolNamesMatchTools(t *testing.T) {
	tools := Tools()
	if len(tools) != len(ToolNames) {
		t.Fatalf("len mismatch: tools=%d names=%d", len(tools), len(ToolNames))
	}
}
