// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package transport

import (
	"bytes"
	"encoding/json"
	"testing"

	"centra/packages/construct/schema"
	"centra/packages/construct/schema/primitives"
)

// capturedEvent mirrors the daemon sseEvent{Type,Phase,Fields} shape.
type capturedEvent struct {
	Type   string
	Phase  string
	Fields map[string]interface{}
}

// fakeSink records events exactly like the daemon transcript's Event sink.
type fakeSink struct{ events []capturedEvent }

func (f *fakeSink) Event(eventType, phase string, fields map[string]interface{}) {
	f.events = append(f.events, capturedEvent{Type: eventType, Phase: phase, Fields: fields})
}

// wireEvent mirrors executor/cmd/mcl-execute daemon_sse.go sseEvent JSON.
type wireEvent struct {
	Seq    uint64                 `json:"seq"`
	TS     string                 `json:"ts"`
	Phase  string                 `json:"phase"`
	Type   string                 `json:"type"`
	Fields map[string]interface{} `json:"fields,omitempty"`
}

// throughWire serialises a captured event to the SSE/JSONL wire and back,
// proving the surface survives the transport exactly as the daemon would push
// it (and as /events/replay/:id would re-read it).
func throughWire(t *testing.T, ce capturedEvent, seq uint64) wireEvent {
	t.Helper()
	on := wireEvent{Seq: seq, TS: "2026-06-17T00:00:00Z", Phase: ce.Phase, Type: ce.Type, Fields: ce.Fields}
	b, err := json.Marshal(on)
	if err != nil {
		t.Fatalf("wire marshal: %v", err)
	}
	var back wireEvent
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("wire unmarshal: %v", err)
	}
	return back
}

func sampleMetric() *schema.Surface {
	return schema.NewMetric("blk", &primitives.Metric{Label: "Block height", Value: "1234567", Unit: "block"})
}

func TestEmitSurfaceArrivesAndDecodesInProcess(t *testing.T) {
	sink := &fakeSink{}
	s := sampleMetric()
	if err := EmitSurface(sink, "intent-1", "conv-1", s); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(sink.events))
	}
	ev := sink.events[0]
	if ev.Type != EventSurface || ev.Phase != Phase {
		t.Fatalf("unexpected event header: %s/%s", ev.Type, ev.Phase)
	}
	if ev.Fields[FieldIntentID] != "intent-1" || ev.Fields[FieldConversationID] != "conv-1" {
		t.Fatalf("intent/conversation not threaded: %+v", ev.Fields)
	}
	got, err := SurfaceFromEvent(ev.Fields)
	if err != nil {
		t.Fatalf("decode in-process: %v", err)
	}
	if got.Kind != schema.KindMetric || got.Metric == nil || got.Metric.Value != "1234567" {
		t.Fatalf("decoded surface wrong: %+v", got)
	}
}

func TestWireRoundTripReplaysIdentically(t *testing.T) {
	sink := &fakeSink{}
	s := sampleMetric().WithSeq(3).WithAttributes(&schema.Attributes{Stakes: schema.StakesFact})
	if err := EmitSurface(sink, "intent-1", "", s); err != nil {
		t.Fatalf("emit: %v", err)
	}
	// Push through the wire (live tap) and again (replay) — both must decode to
	// byte-identical surface JSON.
	live := throughWire(t, sink.events[0], 1)
	replay := throughWire(t, sink.events[0], 1)

	want, _ := s.Marshal()
	for _, w := range []wireEvent{live, replay} {
		got, err := SurfaceFromEvent(w.Fields)
		if err != nil {
			t.Fatalf("decode wire: %v", err)
		}
		gotB, _ := got.Marshal()
		if !bytes.Equal(gotB, want) {
			t.Fatalf("surface not byte-identical across the wire\n have %s\n want %s", gotB, want)
		}
	}
}

func TestIntentFiltering(t *testing.T) {
	sink := &fakeSink{}
	_ = EmitSurface(sink, "A", "", sampleMetric())
	_ = EmitSurface(sink, "B", "", sampleMetric())
	// Emulate daemon_sse.go sseFilter.allows: fields["intent_id"] == target.
	count := 0
	for _, ev := range sink.events {
		if got, _ := ev.Fields[FieldIntentID].(string); got == "A" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("intent filter: want 1 match for A, got %d", count)
	}
}

func TestEmitRejectsInvalidSurface(t *testing.T) {
	sink := &fakeSink{}
	bad := &schema.Surface{Kind: schema.KindMetric, ID: "x"} // no payload
	if err := EmitSurface(sink, "i", "", bad); err == nil {
		t.Fatal("expected emit to reject an invalid surface")
	}
	if len(sink.events) != 0 {
		t.Fatal("invalid surface must not be emitted")
	}
}

func TestStreamOpenAppendClose(t *testing.T) {
	base := schema.NewStream("log", &primitives.Stream{Source: "terminal", Chunks: []primitives.StreamChunk{{Seq: 1, Text: "boot\n"}}})

	// Append two chunks, including a DUPLICATE of seq 1 (replay idempotency).
	p1 := schema.NewStream("log", &primitives.Stream{Chunks: []primitives.StreamChunk{
		{Seq: 1, Text: "boot\n"}, // duplicate -> dropped
		{Seq: 2, Text: "run\n"},
	}})
	got, err := ApplyPatch(base, p1)
	if err != nil {
		t.Fatalf("apply append: %v", err)
	}
	if len(got.Stream.Chunks) != 2 {
		t.Fatalf("want 2 unique chunks, got %d", len(got.Stream.Chunks))
	}

	// Close the stream.
	p2 := schema.NewStream("log", &primitives.Stream{Closed: true, Chunks: []primitives.StreamChunk{{Seq: 3, Text: "done\n"}}})
	got, err = ApplyPatch(got, p2)
	if err != nil {
		t.Fatalf("apply close: %v", err)
	}
	if len(got.Stream.Chunks) != 3 || !got.Stream.Closed {
		t.Fatalf("stream not closed/ordered: %+v", got.Stream)
	}
	// Base is untouched (no mutation).
	if len(base.Stream.Chunks) != 1 {
		t.Fatalf("ApplyPatch mutated base: %+v", base.Stream)
	}
}

func TestTimelineUpsert(t *testing.T) {
	base := schema.NewTimeline("plan", &primitives.Timeline{Steps: []primitives.TimelineStep{
		{ID: "a", Label: "compile", Status: primitives.StepRunning},
	}})
	patch := schema.NewTimeline("plan", &primitives.Timeline{Steps: []primitives.TimelineStep{
		{ID: "a", Label: "compile", Status: primitives.StepDone},   // upsert status
		{ID: "b", Label: "deploy", Status: primitives.StepPending}, // new
	}})
	got, err := ApplyPatch(base, patch)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(got.Timeline.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(got.Timeline.Steps))
	}
	if got.Timeline.Steps[0].Status != primitives.StepDone {
		t.Fatalf("step a not upserted to done: %+v", got.Timeline.Steps[0])
	}
	// Idempotent re-apply.
	again, _ := ApplyPatch(got, patch)
	if len(again.Timeline.Steps) != 2 {
		t.Fatalf("re-apply not idempotent: %d steps", len(again.Timeline.Steps))
	}
}

func TestApplyPatchReplaceAttachesAskResponse(t *testing.T) {
	base := schema.NewAsk("a1", &primitives.Ask{AskKind: primitives.AskConfirm, Prompt: "Proceed?", Required: true})
	confirmed := true
	answered := schema.NewAsk("a1", &primitives.Ask{
		AskKind:  primitives.AskConfirm,
		Prompt:   "Proceed?",
		Required: true,
		Response: &primitives.AskResponse{Confirmed: &confirmed, AnsweredAt: "2026-06-17T00:00:01Z"},
	})
	got, err := ApplyPatch(base, answered)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got.Ask.Response == nil || got.Ask.Response.Confirmed == nil || !*got.Ask.Response.Confirmed {
		t.Fatalf("ask response not attached: %+v", got.Ask)
	}
}

func TestApplyPatchKindMismatch(t *testing.T) {
	if _, err := ApplyPatch(sampleMetric(), schema.NewNarration("blk", &primitives.Narration{Text: "x"})); err == nil {
		t.Fatal("expected kind-mismatch error")
	}
}

func TestPatchSurfaceEmitsTargetedEvent(t *testing.T) {
	sink := &fakeSink{}
	delta := schema.NewStream("log", &primitives.Stream{Chunks: []primitives.StreamChunk{{Seq: 2, Text: "more\n"}}})
	if err := PatchSurface(sink, "intent-1", "", "log", delta); err != nil {
		t.Fatalf("patch: %v", err)
	}
	ev := sink.events[0]
	if ev.Type != EventSurfacePatch {
		t.Fatalf("want %s, got %s", EventSurfacePatch, ev.Type)
	}
	if ev.Fields[FieldID] != "log" {
		t.Fatalf("patch target id not set: %+v", ev.Fields)
	}
}
