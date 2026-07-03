// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"matrix/cody/internal/llmtest"
)

// TestSDROverrideRoundTrip proves reqs 8.1-8.3 + 12.1 + 17.1/17.2: a greenfield
// Engineer run PAUSES needs_input on the Stack Decision Record; an /answer with
// an OVERRIDE verdict (not the authored pick) resolves it; planning proceeds
// against the override and the run completes for real. The durable resolution
// records the override, and the persisted trace replays BYTE-IDENTICALLY across
// a fresh reload — the durability property the client's rebuild relies on.
func TestSDROverrideRoundTrip(t *testing.T) {
	workspaceRoot := t.TempDir() // greenfield: the SDR gates
	gw := llmtest.NewServer(t, sdrScript(t, true))
	t.Cleanup(gw.Close)
	engine := newEngine(t, workspaceRoot, t.TempDir(), gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(engine.Close)
	srv := httptest.NewServer(New(engine).Handler())
	t.Cleanup(srv.Close)

	out := postChat(t, srv.URL, `{"message": "build a realtime chat app", "conversation_id": "conv-sdr-override"}`)
	runID := out["intent_id"].(string)

	// It gates on the SDR before any plan exists.
	waitAwaiting(t, engine, runID)
	events, _ := engine.trace.load(runID)
	if !hasType(events, "decision.stack") {
		t.Fatalf("no decision.stack before the pause: %v", eventTypes(events))
	}
	if hasType(events, "plan.created") || hasType(events, "task.started") {
		t.Fatalf("wave 1 reachable before the SDR resolved: %v", eventTypes(events))
	}

	// Override with a different stack than the authored pick, carrying a payload.
	override := `{"verdict": {"decision": "override", "payload": "SvelteKit on Node/TS"}}`
	resp, err := http.Post(srv.URL+"/intents/"+runID+"/answer", "application/json", strings.NewReader(override))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("answer: %v %v", resp, err)
	}
	resp.Body.Close()

	if status := waitCompleted(t, srv.URL, runID); status != "completed" {
		t.Fatalf("terminal after override = %q", status)
	}

	// The plan proceeded against the override (deliverables are real).
	for file, want := range map[string]string{"greet.txt": "hello cody\n", "reply.txt": "hello back\n"} {
		data, err := os.ReadFile(workspaceRoot + "/" + file)
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", file, data, err)
		}
	}

	// The durable resolution records the OVERRIDE and folds its payload into the
	// planning addendum — the plan was authored against the override, not the pick.
	sd, ok := engine.loadStackDecision("conv-sdr-override")
	if !ok {
		t.Fatal("stack decision was not persisted durably")
	}
	if sd.Decision != "override" {
		t.Fatalf("persisted SDR decision = %q, want override", sd.Decision)
	}
	if !strings.Contains(sd.Addendum, "SvelteKit") {
		t.Fatalf("override payload did not reach the planning addendum: %q", sd.Addendum)
	}
	if !strings.Contains(strings.ToLower(sd.Addendum), "override") {
		t.Fatalf("addendum does not mark the user override: %q", sd.Addendum)
	}
	final, _ := engine.trace.load(runID)
	if !hasType(final, "decision.resolved") || !hasType(final, "plan.created") || !hasType(final, "plan.completed") {
		t.Fatalf("post-override trace missing resolution/plan/completion: %v", eventTypes(final))
	}

	// Byte-identical trace replay: the persisted trace, reloaded through a FRESH
	// store instance and re-serialized, reproduces the on-disk bytes exactly —
	// the run rebuilt from the durable trace equals the live fold, byte for byte.
	rawBytes, err := os.ReadFile(engine.trace.path(runID))
	if err != nil {
		t.Fatalf("read raw trace: %v", err)
	}
	reloaded, err := newTraceStore(engine.trace.dir)
	if err != nil {
		t.Fatal(err)
	}
	replayEvents, err := reloaded.load(runID)
	if err != nil {
		t.Fatalf("reload trace: %v", err)
	}
	var rebuilt strings.Builder
	for _, ev := range replayEvents {
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("re-marshal event: %v", err)
		}
		rebuilt.Write(data)
		rebuilt.WriteByte('\n')
	}
	if rebuilt.String() != string(rawBytes) {
		t.Fatalf("trace replay is not byte-identical across reload:\n--- on-disk ---\n%s\n--- rebuilt ---\n%s", rawBytes, rebuilt.String())
	}
}
