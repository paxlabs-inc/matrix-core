// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"matrix/executor/runtime"
	"matrix/mcl/ir"
	"matrix/mcl/llm"
	"matrix/mcl/mtx/interpreter"
)

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

// fakeLLM is a deterministic interpreter.LLM stub. Records every call
// and returns Response (or Err) without touching the network. Used to
// exercise step_handler routing without booting a real provider.
type fakeLLM struct {
	mu       sync.Mutex
	Response string
	Err      error
	Calls    int
	LastSys  string
	LastUser string
}

func (f *fakeLLM) Decode(ctx context.Context, msgs []interpreter.Message, grammar string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	for _, m := range msgs {
		switch m.Role {
		case "system":
			f.LastSys = m.Content
		case "user":
			f.LastUser = m.Content
		}
	}
	if f.Err != nil {
		return "", f.Err
	}
	return f.Response, nil
}

// streamingFakeLLM extends fakeLLM with a Stream() method that emits
// the configured Chunks one at a time through the onDelta callback,
// concatenating to the same Response semantics as Decode. Tests use
// this to exercise the streaming hot path of llmStepHandler.HandleStep
// without touching the network.
//
// Default behaviour when Chunks is nil: the streaming impl falls back
// to single-shot delivery of Response (for tests that want streaming
// capability detection but don't care about the delta cadence).
type streamingFakeLLM struct {
	fakeLLM
	Chunks      []string // ordered content fragments; "" entries are skipped by the handler
	StreamCalls int
}

func (f *streamingFakeLLM) Stream(ctx context.Context, msgs []interpreter.Message,
	grammar string, onDelta func(delta string)) (string, error) {
	f.mu.Lock()
	f.StreamCalls++
	f.Calls++
	for _, m := range msgs {
		switch m.Role {
		case "system":
			f.LastSys = m.Content
		case "user":
			f.LastUser = m.Content
		}
	}
	chunks := f.Chunks
	resp := f.Response
	err := f.Err
	f.mu.Unlock()

	if err != nil {
		return "", err
	}
	if len(chunks) == 0 {
		// Single-shot fallback: emit Response as one delta then return.
		if onDelta != nil && resp != "" {
			onDelta(resp)
		}
		return resp, nil
	}
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(c)
		if onDelta != nil {
			onDelta(c)
		}
	}
	return sb.String(), nil
}

// newCapturingTranscript returns a transcript wired to a bytes.Buffer
// so tests can inspect every Event() call. mirror is io.Discard so
// stderr stays clean during go test.
func newCapturingTranscript() (*transcript, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &transcript{
		enc:    json.NewEncoder(buf),
		mirror: io.Discard,
	}, buf
}

// decodeEvents parses the captured JSONL stream into typed records for
// assertion. Returns the parsed events in emission order.
type capturedEvent struct {
	Seq    uint64                 `json:"seq"`
	Phase  string                 `json:"phase"`
	Type   string                 `json:"type"`
	Fields map[string]interface{} `json:"fields"`
}

func decodeEvents(t *testing.T, buf *bytes.Buffer) []capturedEvent {
	t.Helper()
	var out []capturedEvent
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var e capturedEvent
		if err := dec.Decode(&e); err != nil {
			t.Fatalf("decode transcript event: %v", err)
		}
		out = append(out, e)
	}
	return out
}

// stepHandlerWithFakes builds a fully-wired llmStepHandler pre-populated
// with a per-kind fakeLLM in the cache. Bypasses llm.New entirely so
// tests run offline and stay fast.
func stepHandlerWithFakes(t *testing.T, fakes map[llm.StepKind]*fakeLLM) (*llmStepHandler, *bytes.Buffer) {
	t.Helper()
	tr, buf := newCapturingTranscript()
	h := &llmStepHandler{
		registry: llm.DefaultRegistry(),
		clients:  make(map[llm.RouteKey]interpreter.LLM),
		skillURI: "matrix://skill/test@1.0.0",
		skillMD:  []byte("# Test skill\n\nFollow the user."),
		t:        tr,
	}
	for k, f := range fakes {
		h.clients[llm.RouteKey{Slot: llm.SlotExecutor, Kind: k}] = f
	}
	return h, buf
}

// stepHandlerWithStreamingFakes mirrors stepHandlerWithFakes but seeds
// the cache with streamingFakeLLM instances that satisfy the
// interpreter.StreamingLLM capability, so HandleStep takes the
// streaming code path. Used by P3a delta tests.
func stepHandlerWithStreamingFakes(t *testing.T, fakes map[llm.StepKind]*streamingFakeLLM) (*llmStepHandler, *bytes.Buffer) {
	t.Helper()
	tr, buf := newCapturingTranscript()
	h := &llmStepHandler{
		registry: llm.DefaultRegistry(),
		clients:  make(map[llm.RouteKey]interpreter.LLM),
		skillURI: "matrix://skill/test@1.0.0",
		skillMD:  []byte("# Test skill\n\nFollow the user."),
		t:        tr,
	}
	for k, f := range fakes {
		h.clients[llm.RouteKey{Slot: llm.SlotExecutor, Kind: k}] = f
	}
	return h, buf
}

func minimalPlan() *ir.PlanTree {
	return &ir.PlanTree{
		ID:       "01HZ000000000000000000PLAN",
		Version:  "mcl/0.1",
		IntentID: "01HZ00000000000000000INTNT",
		SkillRef: "matrix://skill/test@1.0.0",
		Root:     ir.PlanNode{ID: "n0", Kind: ir.NodeSequential, Children: []ir.PlanNode{}},
	}
}

func stepNode(id, kind string) *ir.PlanNode {
	return &ir.PlanNode{
		ID:   id,
		Kind: ir.NodeStep,
		Step: &ir.StepPayload{
			PromptName: "test_step",
			Inputs:     map[string]string{"x": "y"},
			Kind:       kind,
		},
	}
}

// ---------------------------------------------------------------------------
// cfgFor — pure config resolution
// ---------------------------------------------------------------------------

func TestStepHandler_cfgFor_DefaultsPerKind(t *testing.T) {
	h := &llmStepHandler{registry: llm.DefaultRegistry()}

	tests := []struct {
		kind      llm.StepKind
		modelFrag string
	}{
		{llm.KindReason, "glm-5p1-fast"},
		{llm.KindCode, "Qwen3-Coder"},
		{llm.KindSummarize, "deepseek-v4-flash"},
		{llm.KindWrite, "kimi-k2p7-code"},
		{llm.KindTransform, "gpt-oss-20b"},
		{llm.KindClassify, "gpt-oss-20b"},
		{llm.KindHardReason, "deepseek-v4-pro"},
	}
	for _, tt := range tests {
		t.Run(tt.kind.String(), func(t *testing.T) {
			cfg := h.cfgFor(llm.RouteKey{Slot: llm.SlotExecutor, Kind: tt.kind})
			if !strings.Contains(strings.ToLower(cfg.Model), strings.ToLower(tt.modelFrag)) {
				t.Errorf("kind=%s model=%q, want fragment %q", tt.kind, cfg.Model, tt.modelFrag)
			}
			// Step decode is always free-form — registry's GrammarJSONSchema
			// for classify is intentionally stripped at handler level
			// because in-skill prompts use raw text.
			if cfg.GrammarMode != llm.GrammarNone {
				t.Errorf("kind=%s grammar mode = %v, want GrammarNone (handler strips grammar)", tt.kind, cfg.GrammarMode)
			}
		})
	}
}

func TestStepHandler_cfgFor_ModelOverride(t *testing.T) {
	h := &llmStepHandler{
		registry:      llm.DefaultRegistry(),
		overrideModel: "test/override-model-v1",
	}
	// Override must win for every kind — CLI --model is a hard pin.
	for _, kind := range llm.AllStepKinds {
		cfg := h.cfgFor(llm.RouteKey{Slot: llm.SlotExecutor, Kind: kind})
		if cfg.Model != "test/override-model-v1" {
			t.Errorf("kind=%s model=%q, want override to win", kind, cfg.Model)
		}
	}
}

func TestStepHandler_cfgFor_BaseURLOverride(t *testing.T) {
	h := &llmStepHandler{
		registry:        llm.DefaultRegistry(),
		overrideBaseURL: "http://localhost:9090",
	}
	cfg := h.cfgFor(llm.RouteKey{Slot: llm.SlotExecutor, Kind: llm.KindReason})
	if cfg.Endpoint != "http://localhost:9090/v1/chat/completions" {
		t.Errorf("endpoint = %q, want localhost gateway path", cfg.Endpoint)
	}
}

func TestStepHandler_cfgFor_BaseURLOverrideStripsTrailingSlash(t *testing.T) {
	h := &llmStepHandler{
		registry:        llm.DefaultRegistry(),
		overrideBaseURL: "http://localhost:9090/",
	}
	cfg := h.cfgFor(llm.RouteKey{Slot: llm.SlotExecutor, Kind: llm.KindReason})
	if cfg.Endpoint != "http://localhost:9090/v1/chat/completions" {
		t.Errorf("endpoint = %q, want trailing slash stripped", cfg.Endpoint)
	}
}

func TestStepHandler_cfgFor_SeedOverride(t *testing.T) {
	h := &llmStepHandler{
		registry:     llm.DefaultRegistry(),
		overrideSeed: 9999,
	}
	cfg := h.cfgFor(llm.RouteKey{Slot: llm.SlotExecutor, Kind: llm.KindReason})
	if cfg.Seed != 9999 {
		t.Errorf("seed = %d, want override 9999", cfg.Seed)
	}
}

// ---------------------------------------------------------------------------
// clientFor — caching + normalization
// ---------------------------------------------------------------------------

func TestStepHandler_clientFor_CacheHit(t *testing.T) {
	fakes := map[llm.StepKind]*fakeLLM{
		llm.KindReason: {Response: "reason-out"},
	}
	h, _ := stepHandlerWithFakes(t, fakes)

	// Two lookups should return the SAME interface value (cache hit).
	c1, _, err := h.clientFor(llm.RouteKey{Slot: llm.SlotExecutor, Kind: llm.KindReason})
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	c2, _, err := h.clientFor(llm.RouteKey{Slot: llm.SlotExecutor, Kind: llm.KindReason})
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if c1 != c2 {
		t.Fatalf("cache miss: got different clients on identical RouteKey")
	}
}

func TestStepHandler_clientFor_UnspecifiedNormalizesToReason(t *testing.T) {
	fakes := map[llm.StepKind]*fakeLLM{
		llm.KindReason: {Response: "reason-out"},
	}
	h, _ := stepHandlerWithFakes(t, fakes)

	// Pre-populated under KindReason; an Unspecified lookup must hit the
	// same cache entry (normalization rule in clientFor).
	c, _, err := h.clientFor(llm.RouteKey{Slot: llm.SlotExecutor, Kind: llm.KindUnspecified})
	if err != nil {
		t.Fatalf("unspecified lookup: %v", err)
	}
	if c != fakes[llm.KindReason] {
		t.Fatalf("KindUnspecified did not normalize to KindReason cache slot")
	}
}

// ---------------------------------------------------------------------------
// HandleStep — routing per node.Step.Kind
// ---------------------------------------------------------------------------

func TestStepHandler_HandleStep_RoutesPerKind(t *testing.T) {
	fakes := map[llm.StepKind]*fakeLLM{
		llm.KindReason: {Response: "REASON_OUTPUT"},
		llm.KindCode:   {Response: "CODE_OUTPUT"},
		llm.KindWrite:  {Response: "WRITE_OUTPUT"},
	}
	h, buf := stepHandlerWithFakes(t, fakes)
	plan := minimalPlan()

	tests := []struct {
		nodeKind   string
		wantText   string
		wantKindEv string
	}{
		{"reason", "REASON_OUTPUT", "reason"},
		{"code", "CODE_OUTPUT", "code"},
		{"write", "WRITE_OUTPUT", "write"},
	}
	for _, tt := range tests {
		t.Run(tt.nodeKind, func(t *testing.T) {
			res, err := h.HandleStep(context.Background(), plan, stepNode("n1-"+tt.nodeKind, tt.nodeKind))
			if err != nil {
				t.Fatalf("HandleStep(%s): %v", tt.nodeKind, err)
			}
			if res.Text != tt.wantText {
				t.Errorf("text = %q, want %q (wrong fake LLM was invoked)", res.Text, tt.wantText)
			}
		})
	}

	// Per-fake call count: each kind invoked exactly once.
	for kind, f := range fakes {
		if f.Calls != 1 {
			t.Errorf("fake[%s].Calls = %d, want 1", kind, f.Calls)
		}
	}

	// Every decode emits a step.llm.decode event with the resolved kind.
	events := decodeEvents(t, buf)
	gotKinds := map[string]bool{}
	for _, e := range events {
		if e.Type != "step.llm.decode" {
			continue
		}
		if k, ok := e.Fields["kind"].(string); ok {
			gotKinds[k] = true
		}
	}
	for _, want := range []string{"reason", "code", "write"} {
		if !gotKinds[want] {
			t.Errorf("step.llm.decode event missing kind=%q (got %v)", want, gotKinds)
		}
	}
}

func TestStepHandler_HandleStep_EmptyKindRoutesToReason(t *testing.T) {
	fakes := map[llm.StepKind]*fakeLLM{
		llm.KindReason: {Response: "reason-default"},
	}
	h, buf := stepHandlerWithFakes(t, fakes)
	plan := minimalPlan()

	// Step with no kind set (the bulk-converted skills case — 159 of them).
	res, err := h.HandleStep(context.Background(), plan, stepNode("n-empty", ""))
	if err != nil {
		t.Fatalf("HandleStep(empty kind): %v", err)
	}
	if res.Text != "reason-default" {
		t.Fatalf("text = %q, want %q (empty kind must route to reason)", res.Text, "reason-default")
	}

	// And the audit event MUST record kind="reason" so the latency
	// histogram bucket is consistent across explicit + implicit calls.
	events := decodeEvents(t, buf)
	var saw bool
	for _, e := range events {
		if e.Type != "step.llm.decode" {
			continue
		}
		if k, ok := e.Fields["kind"].(string); ok && k == "reason" {
			saw = true
			if m, ok := e.Fields["model"].(string); !ok || !strings.Contains(strings.ToLower(m), "glm-5p1-fast") {
				t.Errorf("event model = %v, want glm-5p1-fast from DefaultRegistry", e.Fields["model"])
			}
		}
	}
	if !saw {
		t.Errorf("no step.llm.decode event with kind=reason emitted")
	}
}

func TestStepHandler_HandleStep_UnknownKindFallsBackToReason(t *testing.T) {
	// Validator at ir.ValidatePlan rejects unknown kinds before we get
	// here, but defense-in-depth: if a malformed plan somehow reaches
	// the handler (e.g. tests, third-party planner) the routing falls
	// back to reason rather than crashing.
	fakes := map[llm.StepKind]*fakeLLM{
		llm.KindReason: {Response: "reason-fallback"},
	}
	h, _ := stepHandlerWithFakes(t, fakes)
	plan := minimalPlan()

	res, err := h.HandleStep(context.Background(), plan, stepNode("n-bad", "this_is_not_a_real_kind"))
	if err != nil {
		t.Fatalf("HandleStep(unknown kind): %v", err)
	}
	if res.Text != "reason-fallback" {
		t.Fatalf("text = %q, want %q (unknown kind must fall back to reason)", res.Text, "reason-fallback")
	}
}

func TestStepHandler_HandleStep_NilStepRejected(t *testing.T) {
	h, _ := stepHandlerWithFakes(t, map[llm.StepKind]*fakeLLM{llm.KindReason: {}})
	plan := minimalPlan()
	bad := &ir.PlanNode{ID: "n-broken", Kind: ir.NodeStep, Step: nil}
	_, err := h.HandleStep(context.Background(), plan, bad)
	if err == nil || !strings.Contains(err.Error(), "nil step body") {
		t.Fatalf("want nil-step body error, got %v", err)
	}
}

func TestStepHandler_HandleStep_LLMErrorPropagatesWithLatency(t *testing.T) {
	wantErr := errors.New("provider 503")
	fakes := map[llm.StepKind]*fakeLLM{
		llm.KindReason: {Err: wantErr},
	}
	h, buf := stepHandlerWithFakes(t, fakes)
	plan := minimalPlan()
	res, err := h.HandleStep(context.Background(), plan, stepNode("n-err", "reason"))

	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapping %v", err, wantErr)
	}
	if res == nil || res.LatencyMs < 0 {
		t.Fatalf("partial StepResult missing LatencyMs: %+v", res)
	}

	// Even on error, the decode event must record the kind+model+error
	// so per-route failure rates are observable.
	events := decodeEvents(t, buf)
	var saw bool
	for _, e := range events {
		if e.Type != "step.llm.decode" {
			continue
		}
		saw = true
		if e.Fields["error"] != "provider 503" {
			t.Errorf("event error = %v, want 'provider 503'", e.Fields["error"])
		}
		if e.Fields["kind"] != "reason" {
			t.Errorf("event kind = %v, want reason", e.Fields["kind"])
		}
	}
	if !saw {
		t.Errorf("no step.llm.decode event emitted on error path")
	}
}

func TestStepHandler_HandleStep_TextSurfacedToTranscript(t *testing.T) {
	fakes := map[llm.StepKind]*fakeLLM{
		llm.KindReason: {Response: "user-visible response body"},
	}
	h, buf := stepHandlerWithFakes(t, fakes)
	plan := minimalPlan()

	_, err := h.HandleStep(context.Background(), plan, stepNode("n-text", "reason"))
	if err != nil {
		t.Fatalf("HandleStep: %v", err)
	}
	events := decodeEvents(t, buf)
	var sawText bool
	for _, e := range events {
		if e.Type != "step.text" {
			continue
		}
		sawText = true
		if e.Fields["text"] != "user-visible response body" {
			t.Errorf("step.text text = %v, want LLM response body", e.Fields["text"])
		}
		// P2 added kind to step.text so SSE consumers can color-code by
		// routing tier.
		if e.Fields["kind"] != "reason" {
			t.Errorf("step.text kind = %v, want reason", e.Fields["kind"])
		}
	}
	if !sawText {
		t.Errorf("no step.text event emitted")
	}
}

// ---------------------------------------------------------------------------
// Drift guard: ir.StepKindNames must match llm.AllStepKindNames
// ---------------------------------------------------------------------------

// The kind enum lives in two places: ir.StepKindNames (validates plan
// canonical JSON without depending on llm) and llm.AllStepKindNames
// (drives routing). They MUST stay in lock-step. This test fails loudly
// if anyone updates one without the other.
func TestKindEnumLockstep_IR_vs_LLM(t *testing.T) {
	if len(ir.StepKindNames) != len(llm.AllStepKindNames) {
		t.Fatalf("ir.StepKindNames (%d) != llm.AllStepKindNames (%d) — closed enum drift",
			len(ir.StepKindNames), len(llm.AllStepKindNames))
	}
	for i, want := range ir.StepKindNames {
		if llm.AllStepKindNames[i] != want {
			t.Errorf("position %d: ir=%q llm=%q", i, want, llm.AllStepKindNames[i])
		}
	}
}

// ---------------------------------------------------------------------------
// concurrency sanity: parallel HandleStep on different kinds is race-free
// ---------------------------------------------------------------------------

func TestStepHandler_HandleStep_ConcurrentDifferentKinds(t *testing.T) {
	fakes := map[llm.StepKind]*fakeLLM{
		llm.KindReason: {Response: "r"},
		llm.KindCode:   {Response: "c"},
		llm.KindWrite:  {Response: "w"},
	}
	h, _ := stepHandlerWithFakes(t, fakes)
	plan := minimalPlan()

	var wg sync.WaitGroup
	var ok int32
	for _, k := range []string{"reason", "code", "write", "reason", "code", "write"} {
		wg.Add(1)
		go func(kind string) {
			defer wg.Done()
			_, err := h.HandleStep(context.Background(), plan, stepNode("n-"+kind, kind))
			if err == nil {
				atomic.AddInt32(&ok, 1)
			}
		}(k)
	}
	wg.Wait()
	if ok != 6 {
		t.Fatalf("only %d/6 concurrent HandleStep succeeded", ok)
	}
}

// ---------------------------------------------------------------------------
// Session 31c · streaming hot path (P3a)
// ---------------------------------------------------------------------------

// collectDeltas pulls every step.text.delta event in emission order
// and returns the parsed delta strings (so tests can assert ordering
// + concatenation invariants).
func collectDeltas(events []capturedEvent) []capturedEvent {
	var out []capturedEvent
	for _, e := range events {
		if e.Type == "step.text.delta" {
			out = append(out, e)
		}
	}
	return out
}

func TestStepHandler_Streaming_CapabilityDetected(t *testing.T) {
	// When the cached client implements interpreter.StreamingLLM, the
	// handler MUST take the streaming path (StreamCalls > 0) rather
	// than calling Decode (Decode-path is exercised by the existing
	// fakeLLM-only tests).
	sf := &streamingFakeLLM{
		fakeLLM: fakeLLM{Response: "single-shot fallback content"},
	}
	h, _ := stepHandlerWithStreamingFakes(t, map[llm.StepKind]*streamingFakeLLM{
		llm.KindReason: sf,
	})
	plan := minimalPlan()

	res, err := h.HandleStep(context.Background(), plan, stepNode("n-cap", "reason"))
	if err != nil {
		t.Fatalf("HandleStep: %v", err)
	}
	if sf.StreamCalls != 1 {
		t.Errorf("StreamCalls = %d, want 1 (capability detection failed)", sf.StreamCalls)
	}
	if res.Text != "single-shot fallback content" {
		t.Errorf("text = %q, want single-shot Response", res.Text)
	}
}

func TestStepHandler_Streaming_DeltasEmittedAndConcatenate(t *testing.T) {
	// Long-enough chunks force size-trigger flushes; small ones with
	// newlines force newline-trigger flushes; the final tail forces a
	// "final" flush. Across all of them: concatenated delta payloads
	// MUST equal the final step.text payload.
	chunks := []string{
		strings.Repeat("a", 220),   // size flush
		"short newline boundary\n", // newline flush
		"trailing without newline", // final flush
	}
	want := strings.Join(chunks, "")

	sf := &streamingFakeLLM{
		fakeLLM: fakeLLM{Response: want},
		Chunks:  chunks,
	}
	h, buf := stepHandlerWithStreamingFakes(t, map[llm.StepKind]*streamingFakeLLM{
		llm.KindReason: sf,
	})
	plan := minimalPlan()

	res, err := h.HandleStep(context.Background(), plan, stepNode("n-deltas", "reason"))
	if err != nil {
		t.Fatalf("HandleStep: %v", err)
	}
	if res.Text != want {
		t.Errorf("res.Text = %q, want %q", res.Text, want)
	}

	events := decodeEvents(t, buf)
	deltas := collectDeltas(events)
	if len(deltas) == 0 {
		t.Fatal("no step.text.delta events emitted in streaming mode")
	}

	// 1) Concatenation: deltas joined == final text.
	var rebuilt strings.Builder
	prevSeq := uint64(0)
	for _, e := range deltas {
		seq, ok := e.Fields["seq"].(float64)
		if !ok {
			t.Errorf("delta event missing numeric seq: %+v", e.Fields)
		}
		if uint64(seq) <= prevSeq {
			t.Errorf("seq did not increase: prev=%d cur=%v", prevSeq, seq)
		}
		prevSeq = uint64(seq)
		d, _ := e.Fields["delta"].(string)
		rebuilt.WriteString(d)
		// reason must be one of the documented values.
		reason, _ := e.Fields["reason"].(string)
		switch reason {
		case "size", "newline", "final":
		default:
			t.Errorf("unexpected delta reason: %q", reason)
		}
		// kind must be carried so the UI can color-code by tier.
		if k, _ := e.Fields["kind"].(string); k != "reason" {
			t.Errorf("delta kind = %v, want reason", e.Fields["kind"])
		}
	}
	if rebuilt.String() != want {
		t.Errorf("delta concatenation = %q, want %q (delta integrity broken)", rebuilt.String(), want)
	}

	// 2) The final step.text MUST also carry streamed=true so consumers
	//    can distinguish replay sources.
	var sawStepText bool
	for _, e := range events {
		if e.Type != "step.text" {
			continue
		}
		sawStepText = true
		if streamed, _ := e.Fields["streamed"].(bool); !streamed {
			t.Errorf("step.text streamed flag = %v, want true", e.Fields["streamed"])
		}
		if txt, _ := e.Fields["text"].(string); txt != want {
			t.Errorf("step.text.text = %q, want %q", txt, want)
		}
	}
	if !sawStepText {
		t.Error("no terminal step.text event emitted")
	}

	// 3) step.llm.decode MUST also reflect streamed=true.
	var sawDecode bool
	for _, e := range events {
		if e.Type != "step.llm.decode" {
			continue
		}
		sawDecode = true
		if streamed, _ := e.Fields["streamed"].(bool); !streamed {
			t.Errorf("step.llm.decode streamed = %v, want true", e.Fields["streamed"])
		}
	}
	if !sawDecode {
		t.Error("no step.llm.decode event emitted")
	}
}

func TestStepHandler_Streaming_NewlineFlush(t *testing.T) {
	// A single chunk under the size threshold but containing a newline
	// MUST flush immediately rather than wait for the final tail —
	// this is the brainstorming UX (one question per line lands as
	// soon as the model emits the line).
	sf := &streamingFakeLLM{
		fakeLLM: fakeLLM{Response: "a\nb\nc"},
		Chunks:  []string{"a\n", "b\n", "c"},
	}
	h, buf := stepHandlerWithStreamingFakes(t, map[llm.StepKind]*streamingFakeLLM{
		llm.KindReason: sf,
	})
	plan := minimalPlan()
	if _, err := h.HandleStep(context.Background(), plan, stepNode("n-nl", "reason")); err != nil {
		t.Fatalf("HandleStep: %v", err)
	}

	events := decodeEvents(t, buf)
	deltas := collectDeltas(events)
	// Three flushes minimum: "a\n" (newline), "b\n" (newline), "c" (final).
	// We accept >= 3 to allow the final flush to be its own event.
	if len(deltas) < 3 {
		t.Fatalf("got %d deltas, want at least 3 (newline + final flushes): %+v", len(deltas), deltas)
	}
	// First two deltas must end in newline.
	for i := 0; i < 2; i++ {
		d, _ := deltas[i].Fields["delta"].(string)
		if !strings.HasSuffix(d, "\n") {
			t.Errorf("delta[%d] = %q, want suffix '\\n' (newline flush expected)", i, d)
		}
	}
}

func TestStepHandler_Streaming_SizeFlush(t *testing.T) {
	// One chunk over the size threshold MUST trigger a size flush
	// even without a newline.
	big := strings.Repeat("x", deltaFlushBytes+50)
	sf := &streamingFakeLLM{
		fakeLLM: fakeLLM{Response: big},
		Chunks:  []string{big},
	}
	h, buf := stepHandlerWithStreamingFakes(t, map[llm.StepKind]*streamingFakeLLM{
		llm.KindReason: sf,
	})
	plan := minimalPlan()
	if _, err := h.HandleStep(context.Background(), plan, stepNode("n-size", "reason")); err != nil {
		t.Fatalf("HandleStep: %v", err)
	}

	events := decodeEvents(t, buf)
	deltas := collectDeltas(events)
	if len(deltas) < 1 {
		t.Fatal("no deltas; size threshold flush did not fire")
	}
	// First delta MUST carry reason=size.
	r, _ := deltas[0].Fields["reason"].(string)
	if r != "size" {
		t.Errorf("first delta reason = %q, want size", r)
	}
}

func TestStepHandler_Streaming_EmptyResponseEmitsNoDeltas(t *testing.T) {
	// An empty model response must not emit any step.text.delta or
	// step.text events (the existing non-stream contract: skip
	// step.text when len(text)==0).
	sf := &streamingFakeLLM{
		fakeLLM: fakeLLM{Response: ""},
		Chunks:  nil,
	}
	h, buf := stepHandlerWithStreamingFakes(t, map[llm.StepKind]*streamingFakeLLM{
		llm.KindReason: sf,
	})
	plan := minimalPlan()
	if _, err := h.HandleStep(context.Background(), plan, stepNode("n-empty", "reason")); err != nil {
		t.Fatalf("HandleStep: %v", err)
	}

	events := decodeEvents(t, buf)
	for _, e := range events {
		if e.Type == "step.text.delta" || e.Type == "step.text" {
			t.Errorf("unexpected %s event for empty response: %+v", e.Type, e.Fields)
		}
	}
}

func TestStepHandler_Streaming_ErrorPropagatesWithPartial(t *testing.T) {
	// Stream error after partial deltas: handler MUST propagate the
	// error AND emit the partial deltas already accumulated, AND the
	// final flush MUST run so the tail is visible.
	wantErr := errors.New("provider 503 mid-stream")
	sf := &streamingFakeLLM{
		fakeLLM: fakeLLM{Err: wantErr, Response: "ignored"},
	}
	h, _ := stepHandlerWithStreamingFakes(t, map[llm.StepKind]*streamingFakeLLM{
		llm.KindReason: sf,
	})
	plan := minimalPlan()
	res, err := h.HandleStep(context.Background(), plan, stepNode("n-streamerr", "reason"))

	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapping %v", err, wantErr)
	}
	if res == nil || res.LatencyMs < 0 {
		t.Fatalf("partial StepResult missing LatencyMs: %+v", res)
	}
}

func TestStepHandler_Streaming_FallbackToDecodeForPlainLLM(t *testing.T) {
	// fakeLLM (non-streaming) → handler must NOT emit any
	// step.text.delta events; it falls back to Decode and only emits
	// step.text + step.llm.decode (with streamed=false).
	plain := &fakeLLM{Response: "non-stream payload"}
	h, buf := stepHandlerWithFakes(t, map[llm.StepKind]*fakeLLM{
		llm.KindReason: plain,
	})
	plan := minimalPlan()
	if _, err := h.HandleStep(context.Background(), plan, stepNode("n-decode", "reason")); err != nil {
		t.Fatalf("HandleStep: %v", err)
	}

	events := decodeEvents(t, buf)
	for _, e := range events {
		if e.Type == "step.text.delta" {
			t.Errorf("unexpected delta event in non-streaming mode: %+v", e.Fields)
		}
	}
	// step.llm.decode MUST carry streamed=false.
	for _, e := range events {
		if e.Type != "step.llm.decode" {
			continue
		}
		if streamed, _ := e.Fields["streamed"].(bool); streamed {
			t.Errorf("step.llm.decode streamed = true for plain Decode path")
		}
	}
}

// ---------------------------------------------------------------------------
// buildSubIntent — sess#37c regression coverage
// ---------------------------------------------------------------------------

// TestBuildSubIntent_NonNilForSynthesizeGuard pins the contract that
// synthesize() requires (sess#37c root cause): the produced *ir.Intent
// MUST be non-nil AND carry the four fields synthesize() reads before
// any LLM call (Skill nil-check + Frame.Verb resolution + ID stamp).
// Without this contract the in-process sub-dispatch path short-circuits
// at synthesize.go:101-103 and the walker bubbles subagent_failed
// before the sub-skill ever gets a chance to plan.
func TestBuildSubIntent_NonNilForSynthesizeGuard(t *testing.T) {
	sk := loadedSkillWithVerbs("matrix://skill/self-map@0.1.0", []string{"analyze"})
	parent := &ir.PlanTree{IntentID: "PARENTINTENT0000000000ABCD"}
	node := &ir.PlanNode{ID: "n02a", Kind: ir.NodeSubDispatch, SubDispatch: &ir.SubDispatchPayload{SkillRef: sk.URI}}
	actor := &actorIdentity{
		UserURI:  "matrix://user/did:matrix:executor:test",
		AgentURI: "matrix://agent/did:matrix:executor:test",
	}

	got := buildSubIntent(sk, parent, node, actor)
	if got == nil {
		t.Fatal("buildSubIntent returned nil; synthesize() guard would fire")
	}
	if got.ID == "" {
		t.Error("Intent.ID empty; synthesize() relies on this for transcript + canonical hash")
	}
	if got.Frame.Verb != "analyze" {
		t.Errorf("Intent.Frame.Verb = %q, want %q (first MclVerbs entry)", got.Frame.Verb, "analyze")
	}
	if got.Parent != "matrix://intent/PARENTINTENT0000000000ABCD" {
		t.Errorf("Intent.Parent = %q, want parent ref", got.Parent)
	}
	if got.Actor != actor.UserURI {
		t.Errorf("Intent.Actor = %q, want %q", got.Actor, actor.UserURI)
	}
	if got.Agent != actor.AgentURI {
		t.Errorf("Intent.Agent = %q, want %q", got.Agent, actor.AgentURI)
	}
	if got.Version != "mcl/0.1" {
		t.Errorf("Intent.Version = %q, want mcl/0.1", got.Version)
	}
	if got.State != ir.StateExecuting {
		t.Errorf("Intent.State = %q, want executing", got.State)
	}
	if got.CreatedAt == "" {
		t.Error("Intent.CreatedAt empty")
	}
	if !strings.Contains(got.Prose, "n02a") {
		t.Errorf("Intent.Prose missing node id; got %q", got.Prose)
	}
	if !strings.Contains(got.Prose, sk.URI) {
		t.Errorf("Intent.Prose missing sub-skill URI; got %q", got.Prose)
	}
}

// TestBuildSubIntent_VerbPriorityOrder pins the verb resolution
// hierarchy: planner-provided SubIntent.Verb wins over the sub-skill's
// first declared MclVerb wins over the "analyze" fallback. Sub-skills
// declare the verb their §PROCEDURE on-block matches; getting this
// wrong means the sub-planner generates a plan for the wrong verb
// branch (or no branch at all).
func TestBuildSubIntent_VerbPriorityOrder(t *testing.T) {
	actor := &actorIdentity{UserURI: "u", AgentURI: "a"}

	t.Run("planner_provided_subintent_verb_wins", func(t *testing.T) {
		sk := loadedSkillWithVerbs("matrix://skill/multi@0.1.0", []string{"analyze", "modify"})
		node := &ir.PlanNode{
			ID:          "n01",
			Kind:        ir.NodeSubDispatch,
			SubDispatch: &ir.SubDispatchPayload{SkillRef: sk.URI, SubIntent: &ir.Frame{Verb: "modify"}},
		}
		got := buildSubIntent(sk, &ir.PlanTree{IntentID: "p"}, node, actor)
		if got.Frame.Verb != "modify" {
			t.Errorf("verb = %q, want planner-provided %q", got.Frame.Verb, "modify")
		}
	})

	t.Run("first_mclverbs_when_no_subintent", func(t *testing.T) {
		sk := loadedSkillWithVerbs("matrix://skill/x@0.1.0", []string{"build", "deliver"})
		node := &ir.PlanNode{ID: "n01", Kind: ir.NodeSubDispatch, SubDispatch: &ir.SubDispatchPayload{SkillRef: sk.URI}}
		got := buildSubIntent(sk, &ir.PlanTree{IntentID: "p"}, node, actor)
		if got.Frame.Verb != "build" {
			t.Errorf("verb = %q, want first MclVerb %q", got.Frame.Verb, "build")
		}
	})

	t.Run("analyze_fallback_when_no_verbs", func(t *testing.T) {
		sk := loadedSkillWithVerbs("matrix://skill/empty@0.1.0", nil)
		node := &ir.PlanNode{ID: "n01", Kind: ir.NodeSubDispatch, SubDispatch: &ir.SubDispatchPayload{SkillRef: sk.URI}}
		got := buildSubIntent(sk, &ir.PlanTree{IntentID: "p"}, node, actor)
		if got.Frame.Verb != ir.VerbAnalyze {
			t.Errorf("verb = %q, want fallback %q", got.Frame.Verb, ir.VerbAnalyze)
		}
	})
}

// TestBuildSubIntent_NilInputsDoNotPanic guards against partial-input
// crashes in the v1 sub-dispatch closure. The caller hands in whatever
// the walker has; defensive nil-safety keeps the synthesizer from
// panicking on edge cases (no parent, no node, no actor).
func TestBuildSubIntent_NilInputsDoNotPanic(t *testing.T) {
	got := buildSubIntent(nil, nil, nil, nil)
	if got == nil {
		t.Fatal("buildSubIntent returned nil on all-nil input; should default-fill")
	}
	if got.Frame.Verb == "" {
		t.Error("Frame.Verb empty under all-nil input; analyze fallback should fire")
	}
	if got.ID == "" {
		t.Error("Intent.ID empty under all-nil input")
	}
}

// loadedSkillWithVerbs is a thin helper for synthesizing a
// *runtime.LoadedSkill fixture without booting the loader. The fields
// that matter to buildSubIntent are URI + MclVerbs.
func loadedSkillWithVerbs(uri string, verbs []string) *runtime.LoadedSkill {
	return &runtime.LoadedSkill{URI: uri, MclVerbs: verbs}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
