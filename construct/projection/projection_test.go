// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package projection

import (
	"testing"

	"matrix/construct/schema"
)

// --- PASSIVE tier: ProjectEvent ---------------------------------------------

func TestProjectStepText(t *testing.T) {
	got := ProjectEvent(Event{
		Type: "step.text",
		Seq:  7,
		Fields: map[string]interface{}{
			"node_id": "n3",
			"text":    "  Working out the answer.  ",
		},
	})
	if len(got) != 1 {
		t.Fatalf("want 1 surface, got %d", len(got))
	}
	s := got[0]
	if s.Kind != schema.KindNarration {
		t.Fatalf("want narration, got %q", s.Kind)
	}
	if s.ID != "step:n3" {
		t.Errorf("id = %q, want step:n3", s.ID)
	}
	if s.Seq != 7 {
		t.Errorf("seq = %d, want 7", s.Seq)
	}
	if s.Narration.Text != "Working out the answer." {
		t.Errorf("text = %q (should be trimmed)", s.Narration.Text)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("projected surface invalid: %v", err)
	}
}

func TestProjectStepTextEmptyIsSilent(t *testing.T) {
	if got := ProjectEvent(Event{Type: "step.text", Fields: map[string]interface{}{"text": "   "}}); got != nil {
		t.Fatalf("empty step text should project nothing, got %d", len(got))
	}
}

func TestProjectToolResultObjectToEntity(t *testing.T) {
	got := ProjectEvent(Event{
		Type: "plan.tool.result",
		Seq:  2,
		Fields: map[string]interface{}{
			"node_id":        "n1",
			"tool":           "matrix://tool/mcp/paxeer-net/chain_info@0.1.0",
			"is_error":       false,
			"result_preview": `{"blockNumber":19042906,"chainId":125,"ok":true}`,
		},
	})
	if len(got) != 1 || got[0].Kind != schema.KindEntity {
		t.Fatalf("want 1 entity, got %+v", got)
	}
	e := got[0].Entity
	if got[0].ID != "tool:n1" {
		t.Errorf("id = %q, want tool:n1", got[0].ID)
	}
	if e.Label != "chain info" {
		t.Errorf("label = %q, want 'chain info'", e.Label)
	}
	// Fields are sorted by key: blockNumber, chainId, ok.
	if len(e.Fields) != 3 || e.Fields[0].Key != "blockNumber" {
		t.Fatalf("fields = %+v", e.Fields)
	}
	if e.Fields[0].Value != "19042906" {
		t.Errorf("blockNumber rendered as %q, want 19042906 (no float noise)", e.Fields[0].Value)
	}
	if err := got[0].Validate(); err != nil {
		t.Errorf("invalid: %v", err)
	}
}

func TestProjectToolResultArrayToStructure(t *testing.T) {
	got := ProjectEvent(Event{
		Type: "plan.tool.result",
		Fields: map[string]interface{}{
			"node_id":        "n2",
			"tool":           "list",
			"result_preview": `["alpha","beta","gamma"]`,
		},
	})
	if len(got) != 1 || got[0].Kind != schema.KindStructure {
		t.Fatalf("want structure, got %+v", got)
	}
	st := got[0].Structure
	if st.Shape != "list" || len(st.Records) != 3 || st.Records[1].Label != "beta" {
		t.Fatalf("records = %+v", st.Records)
	}
}

func TestProjectToolResultNumericToMetric(t *testing.T) {
	got := ProjectEvent(Event{
		Type:   "plan.tool.result",
		Fields: map[string]interface{}{"node_id": "n4", "tool": "balance", "result_preview": "42.5"},
	})
	if len(got) != 1 || got[0].Kind != schema.KindMetric {
		t.Fatalf("want metric, got %+v", got)
	}
	if got[0].Metric.Value != "42.5" {
		t.Errorf("value = %q", got[0].Metric.Value)
	}
}

func TestProjectToolResultError(t *testing.T) {
	got := ProjectEvent(Event{
		Type: "plan.tool.result",
		Fields: map[string]interface{}{
			"node_id":        "n5",
			"tool":           "deploy",
			"is_error":       true,
			"result_preview": "revert: insufficient funds",
		},
	})
	if len(got) != 1 || got[0].Entity.Type != "tool_error" {
		t.Fatalf("want tool_error entity, got %+v", got)
	}
	if got[0].Entity.Fields[0].Key != "error" || got[0].Entity.Fields[0].Value != "true" {
		t.Errorf("error marker missing: %+v", got[0].Entity.Fields)
	}
}

func TestProjectUnknownEventIsSilent(t *testing.T) {
	if got := ProjectEvent(Event{Type: "lifecycle.transition", Fields: map[string]interface{}{"to": "executing"}}); got != nil {
		t.Fatalf("unknown event should project nothing, got %d", len(got))
	}
}

// Determinism: the same event projects byte-identically (replay safety).
func TestProjectEventDeterministic(t *testing.T) {
	ev := Event{
		Type: "plan.tool.result",
		Seq:  9,
		Fields: map[string]interface{}{
			"node_id":        "n9",
			"tool":           "x",
			"result_preview": `{"b":2,"a":1,"c":3}`,
		},
	}
	a := ProjectEvent(ev)
	b := ProjectEvent(ev)
	ab, _ := a[0].Marshal()
	bb, _ := b[0].Marshal()
	if string(ab) != string(bb) {
		t.Fatalf("projection not deterministic:\n%s\n%s", ab, bb)
	}
}

// --- ACTIVE tier: ParseRender -----------------------------------------------

func TestParseRenderMetric(t *testing.T) {
	s, err := ParseRender(map[string]interface{}{
		"kind":    "metric",
		"id":      "m1",
		"payload": map[string]interface{}{"label": "Block height", "value": "19042906", "unit": "block"},
	})
	if err != nil {
		t.Fatalf("ParseRender: %v", err)
	}
	if s.Kind != schema.KindMetric || s.ID != "m1" || s.Metric.Label != "Block height" {
		t.Fatalf("surface = %+v", s)
	}
}

func TestParseRenderAttributesDecorate(t *testing.T) {
	s, err := ParseRender(map[string]interface{}{
		"kind":       "entity",
		"payload":    map[string]interface{}{"type": "tx", "identity": "0xabc"},
		"attributes": map[string]interface{}{"stakes": "irreversible", "cost": map[string]interface{}{"amount": 0.17, "unit": "PAX"}},
	})
	if err != nil {
		t.Fatalf("ParseRender: %v", err)
	}
	if s.Attributes == nil || s.Attributes.Stakes != schema.StakesIrreversible {
		t.Fatalf("attributes not applied: %+v", s.Attributes)
	}
	if s.Attributes.Cost == nil || s.Attributes.Cost.Amount != 0.17 {
		t.Errorf("cost not applied: %+v", s.Attributes.Cost)
	}
}

func TestParseRenderInvalidKindRejected(t *testing.T) {
	if _, err := ParseRender(map[string]interface{}{"kind": "widget", "payload": map[string]interface{}{}}); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestParseRenderInvalidPayloadRejected(t *testing.T) {
	// A metric with no label/value must fail Validate (i2: malformed rejected).
	if _, err := ParseRender(map[string]interface{}{"kind": "ask", "payload": map[string]interface{}{"ask_kind": "telepathy", "prompt": "x"}}); err == nil {
		t.Fatal("expected error for invalid ask kind")
	}
}

func TestParseRenderAutoIDStable(t *testing.T) {
	args := map[string]interface{}{"kind": "narration", "payload": map[string]interface{}{"text": "hello"}}
	a, err := ParseRender(args)
	if err != nil {
		t.Fatalf("ParseRender: %v", err)
	}
	b, _ := ParseRender(args)
	if a.ID == "" || a.ID != b.ID {
		t.Fatalf("auto id not stable: %q vs %q", a.ID, b.ID)
	}
}

func TestRenderToolsContract(t *testing.T) {
	tools := RenderTools()
	if len(tools) != 1 || tools[0].Name != ConstructRenderTool {
		t.Fatalf("want one construct_render tool, got %+v", tools)
	}
	if _, ok := tools[0].Params["properties"]; !ok {
		t.Fatalf("params missing properties: %+v", tools[0].Params)
	}
}
