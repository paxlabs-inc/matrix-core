// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// side_channel_d11_test.go — the load-bearing D11 proof for the Construct OS
// Shell (task 8.2, Property 5: Side-channel non-perturbation; Requirements
// 11.1, 11.2, 11.3).
//
// The shell's persistence layer (packages/construct/surfacestore) and the surface stream
// it tees are a pure VIEW/observability side-channel. This test proves that
// turning the shell + surface store ON cannot change what the agent records as
// its REPLAYABLE INPUTS — the user-message + tool-result transcript that is the
// agent's replayable state ("the conversation transcript IS the state",
// agents/neo/internal/llm). The only agent feedback the shell introduces is a
// validated Ask answer, and it enters through the SAME recorded-input path as a
// user message (a tool result), carrying no answered/settled metadata: that
// metadata rides ONLY the side-channel surface patch.
//
// Everything under test is REAL — no fakes/mocks:
//
//   • the real engine Ask back-channel (respondAsk: emits the Ask surface +
//     ask.awaiting, parks the construct_render tool call, on answer patches the
//     surface to its settled state via backchannel.Answered and returns the
//     human response as the tool result — the recorded INPUT),
//   • the real session waiter store (registerAsk / pendingAsk / answerAsk),
//   • the real HTTP receiver (POST /intents/{id}/asks/{ask_id}/answer →
//     handleAskAnswer → backchannel.ValidateResponse),
//   • the real tools.Manager construct_render dispatch (surface + ask),
//   • the real backchannel contract (ValidateResponse / Summarize / Answered),
//   • the real durable store construct/surfacestore.Store, fed through Neo's
//     REAL production broker tap (broker.setTap — the same seam the F3 trace
//     uses) with the SAME construct-phase surface filter the daemon tee applies
//     (daemon_surfacestore.go:recordSurfaceFrame).
//
// The shell is toggled purely by whether that tap + durable store is wired:
// ACTIVE = a real /data-style store recording every construct.surface[.patch]
// frame; INACTIVE = a disabled (empty-dir) no-op store and no tap. The run
// itself — the user message, the rendered surface, the parked Ask, the posted
// answer — is byte-for-byte the same drive in both cases.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"centra/packages/construct/backchannel"
	"centra/packages/construct/projection"
	"centra/packages/construct/schema"
	"centra/packages/construct/schema/primitives"
	"centra/packages/construct/surfacestore"
	"centra/packages/construct/transport"
	"centra/agents/neo/internal/llm"
	"centra/agents/neo/internal/tools"
)

// D11 run fixture — identical, deterministic values for BOTH the shell-active
// and shell-inactive drives so the ONLY difference between the two runs is the
// presence of the surface-store side-channel.
const (
	d11ConvID        = "conv_d11"
	d11IntentID      = "neo_d11_run"
	d11UserMessage   = "Deploy the build for me, and ask me where."
	d11SurfaceID     = "surf_status_1"
	d11SurfaceCallID = "call_surface_1"
	d11AskID         = "ask_choose_deploy"
	d11AskCallID     = "call_ask_1"
)

// d11NarrationArgs builds a valid non-ask construct_render call (a Narration
// status surface). Driving a regular surface BEFORE the Ask makes the recorded
// replayable-input sequence richer than just the Ask answer: it is
// [user message, tool result for the surface render, tool result for the Ask
// answer] — so byte-identity across the shell toggle is a stronger claim.
func d11NarrationArgs() map[string]interface{} {
	return map[string]interface{}{
		"kind": "narration",
		"id":   d11SurfaceID,
		"payload": map[string]interface{}{
			"text": "Starting the deployment now.",
		},
	}
}

// d11Run is one full, real drive of the representative run. teeActive toggles
// the shell + surface store on/off. It returns the agent's recorded replayable-
// input sequence (serialized to canonical JSON bytes), the frames the store
// persisted (nil when inactive), and the store directory (empty when inactive).
func d11Run(t *testing.T, teeActive bool) (inputBytes []byte, persisted []schema.Frame, storeDir string) {
	t.Helper()

	e := &Engine{broker: newBroker(), runs: map[string]*run{}}
	sess := &session{id: d11ConvID, engine: e, asks: map[string]*askWaiter{}}
	r := &run{id: d11IntentID, convID: d11ConvID, sess: sess}
	e.registerRun(r)
	e.broker.ensure(d11IntentID)

	srv, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Wire the REAL Construct active tier: emit surfaces + answer Asks exactly
	// as NewEngine wires them in production.
	m := &tools.Manager{}
	m.SetSurfaceEmitter(e.emitConstructSurface)
	m.SetAskResponder(e.respondAsk)

	// Toggle the shell side-channel. ACTIVE installs the REAL production broker
	// tap (the F3 seam) feeding a REAL durable surface store, with the SAME
	// construct-phase surface filter the daemon tee applies. INACTIVE leaves a
	// disabled no-op store and no tap, so the broker fans out to nobody durable.
	var store *surfacestore.Store
	if teeActive {
		storeDir = t.TempDir()
		store = surfacestore.Open(storeDir)
		if !store.Enabled() {
			t.Fatal("surface store should be enabled for a non-empty /data-style dir")
		}
		e.broker.setTap(func(id string, ev Event) {
			// Mirror daemon_surfacestore.go:recordSurfaceFrame exactly: record
			// ONLY construct-phase surface frames; ignore every other event.
			if ev.Phase != transport.Phase {
				return
			}
			if ev.Type != transport.EventSurface && ev.Type != transport.EventSurfacePatch {
				return
			}
			store.Record(d11ConvID, schema.Frame{
				Seq:    ev.Seq,
				Ts:     ev.Ts,
				Phase:  ev.Phase,
				Type:   ev.Type,
				Fields: ev.Fields,
			})
		})
	} else {
		store = surfacestore.Open("") // disabled: every method is a no-op
		if store.Enabled() {
			t.Fatal("an empty-dir store must be disabled (no persistence)")
		}
	}
	t.Cleanup(store.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// The agent records its replayable inputs into its working transcript as
	// llm.UserMessage (the user's turn) and llm.ToolResult (each tool's result)
	// — see neo/internal/agent.runToolCalls. We assemble that exact sequence
	// from the REAL tool dispatch returns below, in the order the agent appends
	// them, so inputBytes IS the run's recorded replayable-input set.
	var inputs []llm.Message
	inputs = append(inputs, llm.UserMessage(d11UserMessage))

	// 1) A regular (non-ask) surface render. Synchronous: emits one
	//    construct.surface frame (teed when active) and returns its ack as the
	//    recorded tool-result INPUT.
	surfaceContent, isErr, derr := m.Dispatch(withRun(ctx, r), tools.ConstructRenderTool, d11NarrationArgs())
	if derr != nil {
		t.Fatalf("surface render dispatch error: %v", derr)
	}
	if isErr {
		t.Fatalf("surface render returned in-band error: %q", surfaceContent)
	}
	inputs = append(inputs, llm.ToolResult(d11SurfaceCallID, tools.ConstructRenderTool, surfaceContent))

	// 2) An Ask render: parks the tool call on the real back-channel waiter.
	//    Driven on a goroutine exactly as the agent loop would invoke it.
	resultCh := make(chan askResult, 1)
	go func() {
		content, isErr, derr := m.Dispatch(withRun(ctx, r), tools.ConstructRenderTool, askArgs(d11AskID))
		resultCh <- askResult{content: content, isErr: isErr, err: derr}
	}()

	// Block until the run is genuinely parked (a waiter is registered).
	waitParked(t, sess, d11AskID)

	// Answer ONCE over the REAL HTTP back-channel with a valid choose response.
	if code := postAnswer(t, ts.URL, d11IntentID, d11AskID, map[string]interface{}{"choice": "prod"}); code != http.StatusOK {
		t.Fatalf("valid answer: want 200, got %d", code)
	}

	var got askResult
	select {
	case got = <-resultCh:
	case <-time.After(3 * time.Second):
		t.Fatal("parked run did not resume after a valid answer")
	}
	if got.err != nil {
		t.Fatalf("ask dispatch transport error: %v", got.err)
	}
	if got.isErr {
		t.Fatalf("a valid answer must resume cleanly, got in-band error: %q", got.content)
	}
	// The Ask answer enters as the recorded tool-result INPUT — the same path a
	// user message takes, on the same footing (R11.3).
	inputs = append(inputs, llm.ToolResult(d11AskCallID, tools.ConstructRenderTool, got.content))

	b, err := json.Marshal(inputs)
	if err != nil {
		t.Fatalf("marshal recorded inputs: %v", err)
	}

	if teeActive {
		store.Flush() // drain the async writer so every teed frame is on disk
		persisted = store.Load(d11ConvID)
	}
	return b, persisted, storeDir
}

// TestSideChannelNonPerturbation_D11 is the Property 5 proof: a run executed
// WITH the shell + surface store active produces a recorded replayable-input
// sequence byte-identical to the same run executed WITHOUT them, the Ask answer
// enters as a recorded INPUT carrying no replay-altering metadata, and the
// side-channel's only effect is an append to the durable surface JSONL.
func TestSideChannelNonPerturbation_D11(t *testing.T) {
	activeInputs, persisted, storeDir := d11Run(t, true)
	inactiveInputs, _, _ := d11Run(t, false)

	// --- R11.2: byte-identical replayable inputs with vs without the shell ---
	if !bytes.Equal(activeInputs, inactiveInputs) {
		t.Fatalf("REPLAYABLE-INPUT PERTURBATION (D11 violation): the recorded input "+
			"sequence differs with the shell active vs inactive.\n  active:   %s\n  inactive: %s",
			activeInputs, inactiveInputs)
	}

	// The comparison must not be vacuous: the shell genuinely recorded the
	// surface stream this run (two surfaces + the settled patch), yet the inputs
	// above were unchanged.
	if len(persisted) == 0 {
		t.Fatal("the active run persisted no surface frames — the side-channel was not genuinely exercised")
	}
	var sawAskSurface, sawAskPatch bool
	var settledPatch *schema.Frame
	for i := range persisted {
		fr := persisted[i]
		if fr.Phase != transport.Phase {
			t.Fatalf("persisted a non-construct frame (phase=%q) — the tee must record ONLY construct-phase frames", fr.Phase)
		}
		// A full surface carries its id inside the nested surface payload; a
		// patch additionally stamps it as FieldID. Read it the wire-portable way.
		s, err := transport.SurfaceFromEvent(fr.Fields)
		if err != nil {
			t.Fatalf("persisted frame %d carries no decodable surface: %v", i, err)
		}
		switch fr.Type {
		case transport.EventSurface:
			if s.ID == d11AskID {
				sawAskSurface = true
			}
		case transport.EventSurfacePatch:
			if s.ID == d11AskID {
				sawAskPatch = true
				settledPatch = &persisted[i]
			}
		default:
			t.Fatalf("persisted a non-surface frame (type=%q)", fr.Type)
		}
	}
	if !sawAskSurface || !sawAskPatch {
		t.Fatalf("expected the Ask surface AND its settled patch to be persisted on the side-channel (surface=%v patch=%v)", sawAskSurface, sawAskPatch)
	}

	// --- R11.3: the Ask answer is a plain recorded INPUT; the answered/settled
	// metadata rides ONLY the side-channel surface patch, never the input. ---
	var inputMsgs []llm.Message
	if err := json.Unmarshal(activeInputs, &inputMsgs); err != nil {
		t.Fatalf("decode recorded inputs: %v", err)
	}
	if len(inputMsgs) != 3 {
		t.Fatalf("expected 3 recorded inputs [user, surface result, ask answer], got %d", len(inputMsgs))
	}
	askInput := inputMsgs[len(inputMsgs)-1]
	if askInput.Role != llm.RoleTool {
		t.Errorf("the Ask answer must be recorded as a tool-result INPUT (role=tool), got role=%q", askInput.Role)
	}
	if len(askInput.ToolCalls) != 0 {
		t.Error("a recorded tool-result INPUT must carry no tool calls (it is an input, not a plan/walk mutation)")
	}
	// The answer the agent reads is exactly backchannel.Summarize — a plain
	// human-readable line, the same KIND of content a user message carries.
	ask := d11ParseAsk(t)
	wantSummary := backchannel.Summarize(ask, &primitives.AskResponse{Choice: "prod"})
	if askInput.Content != wantSummary {
		t.Errorf("recorded Ask INPUT mismatch:\n  want %q\n  got  %q", wantSummary, askInput.Content)
	}

	// The recorded INPUT must carry NO answered/settled metadata: it is byte-for
	// -byte a {role, tool_call_id, name, content} message — structurally the
	// same footing as a user message — with no Ask.Response, answered_at, or
	// settled fields. That metadata exists ONLY on the side-channel patch.
	rawAskInput, err := json.Marshal(askInput)
	if err != nil {
		t.Fatalf("marshal ask input: %v", err)
	}
	for _, banned := range []string{"response", "answered", "settled", "confirmed", "signature"} {
		if bytes.Contains(bytes.ToLower(rawAskInput), []byte(banned)) {
			t.Errorf("the recorded Ask INPUT leaks replay-altering metadata %q (must ride only the side-channel patch): %s", banned, rawAskInput)
		}
	}

	// Conversely, the side-channel settled patch DOES carry the answered
	// metadata (Ask.Response) — proving it is the patch, not the recorded input,
	// that bears it.
	if settledPatch != nil {
		s, err := transport.SurfaceFromEvent(settledPatch.Fields)
		if err != nil {
			t.Fatalf("decode settled patch surface: %v", err)
		}
		if s.Ask == nil || s.Ask.Response == nil {
			t.Error("the side-channel settled patch must carry the answered Ask.Response metadata")
		}
	}

	// --- R11.1: the side-channel's ONLY effect is the durable surface JSONL.
	// No signed envelope, no Neocortex segment, no plan/walk artifact. The engine
	// under test wires only the broker + store, so the tap is structurally
	// incapable of mutating Neocortex/plan/walk or signing. ---
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatalf("read store dir: %v", err)
	}
	wantFile := d11ConvID + ".surfaces.jsonl"
	if len(entries) != 1 || entries[0].Name() != wantFile {
		var names []string
		for _, en := range entries {
			names = append(names, en.Name())
		}
		t.Fatalf("store dir contains %v — the only side effect must be the surface JSONL %q "+
			"(no signing / Neocortex / plan / walk artifacts)", names, wantFile)
	}
	if _, err := os.Stat(filepath.Join(storeDir, wantFile)); err != nil {
		t.Fatalf("expected surface JSONL at %s: %v", filepath.Join(storeDir, wantFile), err)
	}
}

// d11ParseAsk parses the test's Ask surface the same way the real tool path does
// (projection.ParseRender), so the expected recorded INPUT (Summarize) is
// computed from the SAME Ask the run emitted.
func d11ParseAsk(t *testing.T) *primitives.Ask {
	t.Helper()
	surface, err := projection.ParseRender(askArgs(d11AskID))
	if err != nil {
		t.Fatalf("ParseRender: %v", err)
	}
	if surface.Kind != schema.KindAsk || surface.Ask == nil {
		t.Fatalf("expected an ask surface, got kind=%q", surface.Kind)
	}
	return surface.Ask
}
