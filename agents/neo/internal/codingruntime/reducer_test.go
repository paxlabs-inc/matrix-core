package codingruntime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"centra/agents/neo/internal/codingworker"
)

func TestReduceEventAgentCoreFixtures(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		lifecycle Lifecycle
		terminal  Lifecycle
		wake      WakeKind
		kinds     []EvidenceKind
	}{
		{
			name: "turn starts running",
			fixture: `{
				"type":"message.start",
				"session_id":"runtime-session-1"
			}`,
			lifecycle: LifecycleRunning,
		},
		{
			name: "bounded lifecycle status",
			fixture: `{
				"type":"status.update",
				"session_id":"runtime-session-1",
				"payload":{"kind":"lifecycle","text":"Analyzing the project structure"}
			}`,
			kinds: []EvidenceKind{EvidenceStatus},
		},
		{
			name: "terminal command starts",
			fixture: `{
				"type":"tool.start",
				"session_id":"runtime-session-1",
				"payload":{"tool_id":"tool-1","name":"terminal","context":"pnpm test"}
			}`,
			kinds: []EvidenceKind{EvidenceStatus},
		},
		{
			name: "terminal command and verification complete",
			fixture: `{
				"type":"tool.complete",
				"session_id":"runtime-session-1",
				"payload":{
					"tool_id":"tool-1",
					"name":"terminal",
					"args":{"command":"pnpm test -- --runInBand"},
					"result":{"output":"12 tests passed","exit_code":0},
					"summary":"Finished in 1.2s",
					"duration_s":1.2
				}
			}`,
			kinds: []EvidenceKind{EvidenceCommand, EvidenceVerification},
		},
		{
			name: "patch produces changed file and inline diff",
			fixture: `{
				"type":"tool.complete",
				"session_id":"runtime-session-1",
				"payload":{
					"tool_id":"tool-2",
					"name":"patch",
					"args":{"path":"/workspace/example/src/components/App.tsx"},
					"result":{"success":true},
					"inline_diff":"@@ -1 +1 @@\n-old\n+new"
				}
			}`,
			kinds: []EvidenceKind{EvidenceFileChange, EvidenceDiff},
		},
		{
			name: "todo produces bounded plan state",
			fixture: `{
				"type":"tool.complete",
				"session_id":"runtime-session-1",
				"payload":{
					"tool_id":"tool-3",
					"name":"todo",
					"args":{"todos":[{"id":"ignored-start-id","content":"partial","status":"pending"}]},
					"result":{"todos":[{"id":"todo-1","content":"Build the page","status":"completed"}]},
					"todos":[
						{"id":"todo-1","content":"Build the page","status":"completed"},
						{"id":"todo-2","content":"Run browser checks","status":"in_progress"}
					]
				}
			}`,
			kinds: []EvidenceKind{EvidenceStatus},
		},
		{
			name: "clarification waits and wakes Neo",
			fixture: `{
				"type":"clarify.request",
				"session_id":"runtime-session-1",
				"payload":{"question":"Which database should I use?","choices":["Postgres","SQLite"],"request_id":"clarify-private-1"}
			}`,
			lifecycle: LifecycleNeedsInput,
			wake:      WakeInputRequired,
			kinds:     []EvidenceKind{EvidenceQuestion},
		},
		{
			name: "approval waits and wakes Neo",
			fixture: `{
				"type":"approval.request",
				"session_id":"runtime-session-1",
				"payload":{
					"command":"pnpm add zod",
					"description":"Install the requested dependency",
					"allow_permanent":false,
					"choices":["once","session","deny"],
					"smart_denied":false
				}
			}`,
			lifecycle: LifecycleNeedsApproval,
			wake:      WakeApprovalRequired,
			kinds:     []EvidenceKind{EvidenceApproval},
		},
		{
			name: "preview opens on safe web URL",
			fixture: `{
				"type":"preview.open",
				"session_id":"runtime-session-1",
				"payload":{"url":"http://localhost:3000/dashboard?private=discarded","label":"Dashboard"}
			}`,
			kinds: []EvidenceKind{EvidencePreview},
		},
		{
			name: "blocked status wakes Neo",
			fixture: `{
				"type":"status.update",
				"session_id":"runtime-session-1",
				"payload":{"kind":"blocked","text":"Blocked: the migration needs a schema decision"}
			}`,
			lifecycle: LifecycleBlocked,
			wake:      WakeBlocked,
			kinds:     []EvidenceKind{EvidenceBlocker},
		},
		{
			name: "successful terminal turn wakes verified",
			fixture: `{
				"type":"message.complete",
				"session_id":"runtime-session-1",
				"payload":{
					"text":"Implemented the dashboard and all requested checks pass.",
					"status":"verified",
					"usage":{"model":"mimo-private-model","total":9001},
					"reasoning":"private chain of thought",
					"billing":{"provider":"private-provider"},
					"rendered":"private renderer output"
				}
			}`,
			terminal: LifecycleVerified,
			wake:     WakeVerified,
			kinds:    []EvidenceKind{EvidenceStatus},
		},
		{
			name: "completion without attested verification blocks",
			fixture: `{
				"type":"message.complete",
				"session_id":"runtime-session-1",
				"payload":{"text":"Implemented the requested change.","status":"complete"}
			}`,
			lifecycle: LifecycleBlocked,
			wake:      WakeBlocked,
			kinds:     []EvidenceKind{EvidenceBlocker},
		},
		{
			name: "interrupted terminal turn wakes interrupted",
			fixture: `{
				"type":"message.complete",
				"session_id":"runtime-session-1",
				"payload":{"text":"Stopped at the user's request.","status":"interrupted"}
			}`,
			terminal: LifecycleInterrupted,
			wake:     WakeInterrupted,
			kinds:    []EvidenceKind{EvidenceStatus},
		},
		{
			name: "failed terminal turn wakes failed",
			fixture: `{
				"type":"message.complete",
				"session_id":"runtime-session-1",
				"payload":{"text":"Error: build failed","status":"error","error":"build failed","recoverable":true}
			}`,
			terminal: LifecycleFailed,
			wake:     WakeFailed,
			kinds:    []EvidenceKind{EvidenceError},
		},
		{
			name: "gateway terminal error wakes failed",
			fixture: `{
				"type":"error",
				"session_id":"runtime-session-1",
				"payload":{"message":"agent init failed: workspace unavailable"}
			}`,
			terminal: LifecycleFailed,
			wake:     WakeFailed,
			kinds:    []EvidenceKind{EvidenceError},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := decodeAgentCoreFixture(t, test.fixture)
			batch, emitted, err := ReduceEvent(event)
			if err != nil {
				t.Fatalf("ReduceEvent: %v", err)
			}
			if !emitted {
				t.Fatal("expected a delivery")
			}
			if batch.DeliveryID == "" || len(batch.SourceEventIDs) != 1 || batch.SourceEventIDs[0] == "" {
				t.Fatalf("missing stable delivery identity: %#v", batch)
			}
			if err := validateDelivery(batch); err != nil {
				t.Fatalf("reducer produced invalid delivery: %v\n%#v", err, batch)
			}
			if batch.NextLifecycle != test.lifecycle {
				t.Fatalf("lifecycle = %q, want %q", batch.NextLifecycle, test.lifecycle)
			}
			if test.terminal == "" {
				if batch.Terminal != nil {
					t.Fatalf("unexpected terminal: %#v", batch.Terminal)
				}
			} else if batch.Terminal == nil || batch.Terminal.Lifecycle != test.terminal {
				t.Fatalf("terminal = %#v, want %q", batch.Terminal, test.terminal)
			}
			if test.wake == "" {
				if batch.Wake != nil {
					t.Fatalf("unexpected wake: %#v", batch.Wake)
				}
			} else if batch.Wake == nil || batch.Wake.Kind != test.wake || batch.Wake.DedupKey == "" || batch.Wake.SourceEventID != batch.SourceEventIDs[0] {
				t.Fatalf("wake = %#v, want %q", batch.Wake, test.wake)
			}
			if got := evidenceKinds(batch.Evidence); !equalEvidenceKinds(got, test.kinds) {
				t.Fatalf("evidence kinds = %v, want %v", got, test.kinds)
			}
			for _, evidence := range batch.Evidence {
				if evidence.SourceEventID != batch.SourceEventIDs[0] {
					t.Fatalf("evidence source = %q, want %q", evidence.SourceEventID, batch.SourceEventIDs[0])
				}
			}
		})
	}
}

func TestReduceEventMultiFilePatchUsesAgentCoreResultPaths(t *testing.T) {
	event := decodeAgentCoreFixture(t, `{
		"type":"tool.complete",
		"session_id":"runtime-session-1",
		"payload":{
			"tool_id":"tool-v4a",
			"name":"patch",
			"args":{"mode":"patch","patch":"private full patch input is not product evidence"},
			"result":{
				"success":true,
				"diff":"--- a/src/App.tsx\n+++ b/src/App.tsx\n@@ -1 +1 @@\n-old\n+new",
				"files_modified":["/workspace/example/src/App.tsx"],
				"files_created":["/workspace/example/src/new.ts"],
				"files_deleted":["/workspace/example/src/old.ts"]
			}
		}
	}`)

	batch, emitted, err := ReduceEvent(event)
	if err != nil || !emitted {
		t.Fatalf("ReduceEvent = %v, %v", emitted, err)
	}
	if got, want := evidenceKinds(batch.Evidence), []EvidenceKind{EvidenceFileChange, EvidenceFileChange, EvidenceFileChange, EvidenceDiff}; !equalEvidenceKinds(got, want) {
		t.Fatalf("evidence kinds = %v, want %v", got, want)
	}
	paths := []string{batch.Evidence[0].Path, batch.Evidence[1].Path, batch.Evidence[2].Path}
	for _, want := range []string{"example/src/App.tsx", "example/src/new.ts", "example/src/old.ts"} {
		found := false
		for _, path := range paths {
			if path == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing changed path %q in %v", want, paths)
		}
	}
	encoded, _ := json.Marshal(batch)
	if strings.Contains(string(encoded), "private full patch input") || strings.Contains(string(encoded), "tool-v4a") || strings.Contains(string(encoded), "runtime-session-1") {
		t.Fatalf("private tool input or identity leaked in %s", encoded)
	}
}

func TestReduceEventDropsPrivateAgentCoreFields(t *testing.T) {
	event := decodeAgentCoreFixture(t, `{
		"type":"message.complete",
		"session_id":"runtime-session-secret-77",
		"payload":{
			"text":"Finished runtime-session-secret-77 using token=super-private-token-123 and Bearer abcdefghijklmnop",
			"status":"verified",
			"provider":"internal-mimo-provider",
			"model":"internal-model-id",
			"base_url":"https://internal-provider.invalid/v1",
			"usage":{"total":42},
			"reasoning":"unbounded private reasoning",
			"reasoning_details":{"encrypted":"private-reasoning-details"},
			"billing":{"provider":"internal-billing-provider"},
			"failure_reason":"internal-routing-reason"
		}
	}`)

	batch, emitted, err := ReduceEvent(event)
	if err != nil || !emitted {
		t.Fatalf("ReduceEvent = emitted %v, err %v", emitted, err)
	}
	encoded, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(encoded)
	for _, private := range []string{
		"runtime-session-secret-77", "super-private-token-123", "abcdefghijklmnop",
		"internal-mimo-provider", "internal-model-id", "internal-provider.invalid",
		"unbounded private reasoning", "private-reasoning-details", "internal-billing-provider", "internal-routing-reason",
	} {
		if strings.Contains(serialized, private) {
			t.Fatalf("private value %q leaked in %s", private, serialized)
		}
	}
	if !strings.Contains(serialized, "[private id]") || !strings.Contains(serialized, "[redacted]") {
		t.Fatalf("expected explicit redaction in %s", serialized)
	}
	if strings.Contains(serialized, "session_id") || strings.Contains(serialized, "runtime_id") {
		t.Fatalf("private identity field leaked in %s", serialized)
	}
}

func TestReduceEventStableIDsAndRequestSeparation(t *testing.T) {
	first := decodeAgentCoreFixture(t, `{
		"type":"clarify.request",
		"session_id":"runtime-a",
		"payload":{"choices":["A","B"],"request_id":"request-1","question":"Pick one"}
	}`)
	semanticDuplicate := decodeAgentCoreFixture(t, `{
		"payload":{"question":"Pick one","request_id":"request-1","choices":["A","B"]},
		"session_id":"runtime-b",
		"type":"clarify.request"
	}`)
	secondRequest := decodeAgentCoreFixture(t, `{
		"type":"clarify.request",
		"session_id":"runtime-b",
		"payload":{"question":"Pick one","choices":["A","B"],"request_id":"request-2"}
	}`)

	batchA, ok, err := ReduceEvent(first)
	if err != nil || !ok {
		t.Fatalf("first ReduceEvent = %v, %v", ok, err)
	}
	batchDuplicate, ok, err := ReduceEvent(semanticDuplicate)
	if err != nil || !ok {
		t.Fatalf("duplicate ReduceEvent = %v, %v", ok, err)
	}
	batchB, ok, err := ReduceEvent(secondRequest)
	if err != nil || !ok {
		t.Fatalf("second request ReduceEvent = %v, %v", ok, err)
	}
	if batchA.DeliveryID != batchDuplicate.DeliveryID || batchA.SourceEventIDs[0] != batchDuplicate.SourceEventIDs[0] {
		t.Fatalf("semantic duplicate IDs differ: %#v %#v", batchA, batchDuplicate)
	}
	if batchA.DeliveryID == batchB.DeliveryID || batchA.SourceEventIDs[0] == batchB.SourceEventIDs[0] {
		t.Fatalf("distinct blocking requests collapsed: %#v %#v", batchA, batchB)
	}
	for _, batch := range []DeliveryBatch{batchA, batchB} {
		encoded, _ := json.Marshal(batch)
		if strings.Contains(string(encoded), "request-") || strings.Contains(string(encoded), "runtime-") {
			t.Fatalf("private request/runtime id leaked in %s", encoded)
		}
	}
}

func TestReduceEventIgnoresReasoningAndUnknownEvents(t *testing.T) {
	for _, fixture := range []string{
		`{"type":"reasoning.delta","session_id":"private","payload":{"text":"private reasoning"}}`,
		`{"type":"message.delta","session_id":"private","payload":{"text":"unbounded token stream"}}`,
		`{"type":"session.info","session_id":"private","payload":{"model":"private-model","provider":"private-provider"}}`,
		`{"type":"plugin.internal","session_id":"private","payload":{"anything":"private"}}`,
	} {
		batch, emitted, err := ReduceEvent(decodeAgentCoreFixture(t, fixture))
		if err != nil || emitted || batch.DeliveryID != "" {
			t.Fatalf("ignored event yielded %#v, emitted %v, err %v", batch, emitted, err)
		}
	}
}

func TestReduceEventRejectsMalformedKnownPayload(t *testing.T) {
	_, emitted, err := ReduceEvent(codingworker.Event{Type: "clarify.request", Payload: json.RawMessage(`[]`)})
	if emitted || !errors.Is(err, ErrInvalidWorkerEvent) {
		t.Fatalf("expected ErrInvalidWorkerEvent, got emitted %v err %v", emitted, err)
	}
}

func TestReduceEventCapsProductEvidence(t *testing.T) {
	longQuestion := strings.Repeat("界", maxSummaryRunes+100)
	choices := make([]string, maxChoiceCount+10)
	for index := range choices {
		choices[index] = strings.Repeat("x", maxChoiceRunes+10)
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"question":   longQuestion,
		"choices":    choices,
		"request_id": "request-caps",
	})
	batch, emitted, err := ReduceEvent(codingworker.Event{Type: "clarify.request", SessionID: "private", Payload: payload})
	if err != nil || !emitted {
		t.Fatalf("ReduceEvent = %v, %v", emitted, err)
	}
	if got := len([]rune(batch.Evidence[0].Question)); got != maxSummaryRunes {
		t.Fatalf("question rune count = %d, want %d", got, maxSummaryRunes)
	}
	gotChoices, ok := batch.Evidence[0].Details["choices"].([]string)
	if !ok || len(gotChoices) != maxChoiceCount {
		t.Fatalf("choices = %#v", batch.Evidence[0].Details["choices"])
	}
	for _, choice := range gotChoices {
		if len([]rune(choice)) != maxChoiceRunes {
			t.Fatalf("choice rune count = %d, want %d", len([]rune(choice)), maxChoiceRunes)
		}
	}
}

func decodeAgentCoreFixture(t *testing.T, fixture string) codingworker.Event {
	t.Helper()
	var event codingworker.Event
	if err := json.Unmarshal([]byte(fixture), &event); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return event
}

func evidenceKinds(evidence []EvidenceRecord) []EvidenceKind {
	kinds := make([]EvidenceKind, len(evidence))
	for index := range evidence {
		kinds[index] = evidence[index].Kind
	}
	return kinds
}

func equalEvidenceKinds(left, right []EvidenceKind) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
