// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"matrix/cortex"
	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	nmemory "matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
	"matrix/neo/internal/writeback"
)

func TestClassifyEpisodicMessage(t *testing.T) {
	tests := []struct {
		name  string
		msg   llm.Message
		fired bool
		class string
	}{
		{name: "remember when", msg: llm.UserMessage("Remember when we shipped Zephyr?"), fired: true, class: "remembrance"},
		{name: "remember how", msg: llm.UserMessage("Do you remember how we fixed that race?"), fired: true, class: "remembrance"},
		{name: "you said", msg: llm.UserMessage("Earlier you said the store was append-only."), fired: true, class: "shared_past"},
		{name: "we discussed", msg: llm.UserMessage("What did we discuss about the pager?"), fired: true, class: "shared_past"},
		{name: "last time", msg: llm.UserMessage("Use the approach from last time."), fired: true, class: "shared_past"},
		{name: "temporal", msg: llm.UserMessage("What happened last week with Zephyr?"), fired: true, class: "temporal"},
		{name: "month", msg: llm.UserMessage("Find the deploy issue back in June."), fired: true, class: "temporal"},
		{name: "prospective remember to", msg: llm.UserMessage("Remember to buy milk."), fired: false},
		{name: "prospective remember how to", msg: llm.UserMessage("Remember how to rotate the token."), fired: false},
		{name: "reminder", msg: llm.UserMessage("Remind me to call Sam."), fired: false},
		{name: "ordinary", msg: llm.UserMessage("Please inspect the pager."), fired: false},
		{name: "assistant", msg: llm.AssistantMessage("Remember when we shipped it?"), fired: false},
		{name: "guidance", msg: llm.GuidanceMessage("Remember when the tool failed; retry it."), fired: false},
		{name: "heartbeat", msg: llm.UserMessage(HeartbeatWakeMessage), fired: false},
		{name: "automatrix", msg: llm.UserMessage(AutomatrixWakeMessage), fired: false},
		{name: "morning brief", msg: llm.UserMessage(MorningBriefWakeMessage), fired: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyEpisodicMessage(tc.msg)
			if got.Fired != tc.fired || got.Class != tc.class {
				t.Fatalf("classifyEpisodicMessage(%q) = %+v, want fired=%v class=%q", tc.msg.Content, got, tc.fired, tc.class)
			}
		})
	}
}

func newEpisodicClient(t *testing.T, handler http.Handler) *llm.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	return client
}

func writeEpisodicSSE(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n", content)
	fmt.Fprint(w, "data: [DONE]\n")
}

func TestExtractEpisodicRealClient(t *testing.T) {
	var calls atomic.Int32
	client := newEpisodicClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "past-exchange search target") {
			t.Errorf("request did not carry the episodic extraction system prompt")
		}
		writeEpisodicSSE(w, `{"referent":"Zephyr deploy pipeline","time_hint":"last week","scope_hint":"our deployment discussion"}`)
	}))

	a := New(Options{Cheap: client})
	now := time.Date(2026, time.July, 16, 15, 0, 0, 0, time.UTC)
	got := a.extractEpisodicAt(context.Background(), "Remember when we fixed Zephyr last week?", now)
	if calls.Load() != 1 {
		t.Fatalf("extraction calls = %d, want exactly 1", calls.Load())
	}
	if got.Referent != "Zephyr deploy pipeline" || got.TimeHint != "last week" || got.ScopeHint != "our deployment discussion" {
		t.Fatalf("extraction = %+v", got)
	}
	if !got.Window.bounded() || got.Window.From != time.Date(2026, time.July, 6, 0, 0, 0, 0, time.UTC) || got.Window.Until != time.Date(2026, time.July, 13, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("last-week window = %+v", got.Window)
	}
}

func TestExtractEpisodicTimeoutFallsBackWithinDeadline(t *testing.T) {
	client := newEpisodicClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		writeEpisodicSSE(w, `{"referent":"too late","time_hint":"","scope_hint":""}`)
	}))
	a := New(Options{Cheap: client})
	const raw = "Remember when we debugged the Zephyr deploy?"
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	got := a.extractEpisodic(ctx, raw)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("deadline-bounded extraction took %v", elapsed)
	}
	if got.Referent != raw || got.TimeHint != "" || got.ScopeHint != "" || got.Window.bounded() {
		t.Fatalf("timeout fallback = %+v", got)
	}
}

func TestExtractEpisodicMalformedResponseFallsBack(t *testing.T) {
	client := newEpisodicClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEpisodicSSE(w, "not json")
	}))
	a := New(Options{Cheap: client})
	const raw = "You mentioned a distinctive allocator bug."
	got := a.extractEpisodic(context.Background(), raw)
	if got.Referent != raw || got.TimeHint != "" || got.ScopeHint != "" {
		t.Fatalf("malformed-response fallback = %+v", got)
	}
}

func TestParseEpisodicTimeHint(t *testing.T) {
	now := time.Date(2026, time.July, 16, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		hint      string
		from, til time.Time
	}{
		{hint: "yesterday", from: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC), til: time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)},
		{hint: "last month", from: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), til: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{hint: "in June", from: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), til: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{hint: "in December", from: time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC), til: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range tests {
		t.Run(tc.hint, func(t *testing.T) {
			got := parseEpisodicTimeHint(tc.hint, now)
			if got.From != tc.from || got.Until != tc.til {
				t.Fatalf("parseEpisodicTimeHint(%q) = %+v", tc.hint, got)
			}
		})
	}
	if got := parseEpisodicTimeHint("when we were working on it", now); got.bounded() {
		t.Fatalf("unresolved hint must leave the default horizon, got %+v", got)
	}
}

func TestEpisodicFeedRealChatLoop(t *testing.T) {
	var generationBody []byte
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "past-exchange search target") {
			writeEpisodicSSE(w, `{"referent":"Zephyr Buildkite token","time_hint":"","scope_hint":""}`)
			return
		}
		if strings.Contains(string(body), "memory consolidator") {
			writeEpisodicSSE(w, `{"facts":["Zephyr recovered after rotating the Buildkite token"],"user_facts":[],"preferences":[],"corrections":[],"patterns":[],"opportunities":[],"outcome":null}`)
			return
		}
		generationBody = append([]byte(nil), body...)
		writeEpisodicSSE(w, "I remember the exact fix.")
	}))
	t.Cleanup(model.Close)

	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.FirstTurnRelevancePush = false
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "episodic-feed-test"
	pager, err := nmemory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pager.Close() })
	convA := "conversation-a"
	var lo, hi uint64
	for i, msg := range []cortex.Message{
		{ConversationID: convA, Role: cortex.RoleUser, Content: "Zephyr failed after the Buildkite token expired."},
		{ConversationID: convA, Role: cortex.RoleAssistant, Content: "We rotated the Buildkite token and the canary passed."},
	} {
		uri, aerr := pager.AppendMessage(msg)
		if aerr != nil {
			t.Fatal(aerr)
		}
		_, seq, ok := cortex.ParseSessionURI(uri)
		if !ok {
			t.Fatalf("bad session URI %q", uri)
		}
		if i == 0 {
			lo = seq
		}
		hi = seq
	}
	client := windowLawClient(t, model.URL)
	if _, err := pager.SetMemoryConsent(context.Background(), true, "test user"); err != nil {
		t.Fatal(err)
	}
	consolidator := writeback.New(client, client, pager, cfg)
	consolidator.ConsolidateSync(context.Background(), "USER: Zephyr failed after the Buildkite token expired.\nASSISTANT: We rotated the Buildkite token and the canary passed.", convA, lo, hi)
	deadline := time.Now().Add(2 * time.Second)
	for {
		hits := pager.EpisodicRetrieve(context.Background(), "Zephyr Buildkite token", nmemory.EpisodicTimeWindow{}, nmemory.EpisodicBudget{Hits: 4, Tokens: 200}, nil)
		if len(hits) > 0 && len(hits[0].RelatedMemories) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("consolidated memory never reached the semantic episodic lane: %+v", hits)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var events []MemoryEvent
	a := New(Options{Config: cfg, Main: client, Cheap: client, Tools: &tools.Manager{}, Pager: pager, ConvID: "conversation-b", MemoryObserver: func(ev MemoryEvent) { events = append(events, ev) }})
	if err := a.Chat(context.Background(), "Remember when we fixed Zephyr?"); err != nil {
		t.Fatal(err)
	}
	msgs := decodeWireMessages(t, generationBody)
	if len(msgs) < 3 {
		t.Fatalf("generation window has %d messages", len(msgs))
	}
	tail := msgs[len(msgs)-1].Content
	for _, want := range []string{"Auto-recalled past exchange", "conversation conversation-a", "rotated the Buildkite token", "Related memory:"} {
		if !strings.Contains(tail, want) {
			t.Fatalf("generation tail missing %q:\n%s", want, tail)
		}
	}
	if a.activationAssemblies != 1 || a.windowAssemblies != 1 {
		t.Fatalf("assembly counts activation=%d window=%d", a.activationAssemblies, a.windowAssemblies)
	}
	if a.turn.episodicPending {
		t.Fatal("grounded episodic turn remained pending")
	}
	if len(events) != 1 || events[0].TriggerClass != "remembrance" || len(events[0].Excerpts) != 1 || events[0].Excerpts[0].ConversationID != convA {
		t.Fatalf("memory event = %+v", events)
	}
}

func TestEpisodicFeedDeadCheapLaneDoesNotBlockTurn(t *testing.T) {
	var generationBody []byte
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		generationBody, _ = io.ReadAll(r.Body)
		writeEpisodicSSE(w, "I could not find that past exchange.")
	}))
	t.Cleanup(main.Close)
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.FirstTurnRelevancePush = false
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "episodic-dead-cheap-test"
	pager, err := nmemory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pager.Close() })
	a := New(Options{Config: cfg, Main: windowLawClient(t, main.URL), Cheap: windowLawClient(t, deadURL), Tools: &tools.Manager{}, Pager: pager, ConvID: "conversation-b"})
	if err := a.Chat(context.Background(), "Remember when we fixed the missing incident?"); err != nil {
		t.Fatal(err)
	}
	msgs := decodeWireMessages(t, generationBody)
	if len(msgs) == 0 {
		t.Fatal("main generation did not run after the cheap lane failed")
	}
	if strings.Contains(msgs[len(msgs)-1].Content, "Auto-recalled past exchange") {
		t.Fatalf("dead cheap lane injected an episodic block:\n%s", msgs[len(msgs)-1].Content)
	}
}
