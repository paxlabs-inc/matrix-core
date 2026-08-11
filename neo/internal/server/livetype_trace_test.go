// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"testing"

	"matrix/neo/internal/agent"
)

// NEO-WORKBENCH task 1.2 (req 3.2, 6.1, 3.4): live-typing deltas are a LIVE
// channel only — subscribers see them, but the durable trace replays final
// step states exclusively, so a reopened workbench rebuilds from complete
// statuses, never a mid-flight fragment.
func TestLiveTypingDelta_LiveButNeverDurable(t *testing.T) {
	e := NewEngine(EngineOptions{
		ConversationDir: t.TempDir(),
		TraceDir:        t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(e.Close)
	if !e.trace.Enabled() {
		t.Fatal("trace store must be enabled")
	}

	const intentID = "neo_livetype_run"
	r := &run{id: intentID, convID: "conv_livetype"}
	e.registerRun(r)
	e.broker.ensure(intentID)
	replayBefore, ch, cancel := e.broker.subscribe(intentID, 0)
	defer cancel()
	_ = replayBefore

	// The real event sequence a streamed write produces: start (running),
	// two mid-flight deltas, end (done).
	e.surfaceTool(r, agent.ToolEvent{
		ID: "w1", Name: "fs__write_file", Phase: agent.ToolStart,
		Args: map[string]interface{}{"path": "src/app.go", "content": "package main\n"},
	})
	e.surfaceTool(r, agent.ToolEvent{
		ID: "w1", Name: "fs__write_file", Phase: agent.ToolStream,
		StreamPath: "src/app.go", StreamDelta: "package ", StreamOffset: 0,
	})
	e.surfaceTool(r, agent.ToolEvent{
		ID: "w1", Name: "fs__write_file", Phase: agent.ToolStream,
		StreamPath: "src/app.go", StreamDelta: "main\n", StreamOffset: 8,
	})
	e.surfaceTool(r, agent.ToolEvent{
		ID: "w1", Name: "fs__write_file", Phase: agent.ToolEnd,
		Args:   map[string]interface{}{"path": "src/app.go", "content": "package main\n"},
		Result: "wrote src/app.go",
	})
	e.trace.Flush()

	// Live subscribers received the deltas in order with real payloads.
	var live []Event
	for len(live) < 4 {
		select {
		case ev := <-ch:
			live = append(live, ev)
		default:
			t.Fatalf("expected 4 live events, got %d", len(live))
		}
	}
	deltas := 0
	for _, ev := range live {
		if ev.Type == "tool.delta" {
			deltas++
			if ev.Fields["path"] != "src/app.go" || ev.Fields["delta"] == "" {
				t.Fatalf("live tool.delta fields = %v", ev.Fields)
			}
		}
	}
	if deltas != 2 {
		t.Fatalf("live deltas = %d, want 2", deltas)
	}

	// The durable trace carries the step transitions but NO deltas.
	events := e.trace.Load(intentID)
	steps, traceDeltas := 0, 0
	finalRunning := true
	for _, ev := range events {
		switch ev.Type {
		case "tool.step":
			steps++
			if run, ok := ev.Fields["running"].(bool); ok {
				finalRunning = run
			}
		case "tool.delta":
			traceDeltas++
		}
	}
	if traceDeltas != 0 {
		t.Fatalf("durable trace must carry no tool.delta events, got %d", traceDeltas)
	}
	if steps != 2 {
		t.Fatalf("durable trace steps = %d, want start+end", steps)
	}
	if finalRunning {
		t.Fatal("the last replayed step must carry the FINAL (not running) status")
	}
}
