package loop

import (
	"encoding/json"
	"testing"

	"centra/agents/neo/internal/runtime/protocol"
)

func TestMissingReadOnlyProbeExpectationIsInferredWithoutBlockingExecution(t *testing.T) {
	expectations, calls, err := extractExpectations([]protocol.NormalizedToolCall{{
		ID: "fetch-without-expect", Name: "fetch__fetch",
		Arguments: json.RawMessage(`{"url":"https://example.com/source"}`),
	}})
	if err != nil {
		t.Fatalf("controller metadata blocked a safe probe: %v", err)
	}
	if len(expectations) != 1 || expectations[0] != "returns the requested resource content or a structured unavailable outcome" {
		t.Fatalf("inferred expectation = %#v", expectations)
	}
	if len(calls) != 1 || string(calls[0].Arguments) != `{"url":"https://example.com/source"}` {
		t.Fatalf("normalized call = %+v", calls)
	}
}

func TestExplicitProbeExpectationRemainsAuthoritativeAndIsNotDispatched(t *testing.T) {
	expectations, calls, err := extractExpectations([]protocol.NormalizedToolCall{{
		ID: "search-with-expect", Name: "web-search__web_search",
		Arguments: json.RawMessage(`{"query":"MCP 2026","expect":"returns official 2026 sources"}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(expectations) != 1 || expectations[0] != "returns official 2026 sources" {
		t.Fatalf("expectation = %#v", expectations)
	}
	if string(calls[0].Arguments) != `{"query":"MCP 2026"}` {
		t.Fatalf("expect leaked to tool dispatch: %s", calls[0].Arguments)
	}
}
