package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/tools"
)

// TestTurnIsolation_EpistemicStateDoesNotLeakAcrossChats proves the reified
// turn (MORPHEUS req.2.3, task 2.1) on the real loop: two sequential Chat
// calls share no epistemic run state. Turn 1 seeds the premise ledger (a
// deterministic self-claim in the plan) and the task graph (a dispatched tool
// call); turn 2 must start from a fresh turn — no ledger, no tails, no graph,
// no revision bookkeeping — and its outgoing window must carry no premise
// ledger from turn 1.
func TestTurnIsolation_EpistemicStateDoesNotLeakAcrossChats(t *testing.T) {
	const scaffold = `{"index":0,"id":"scaffold","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"bridge.py\",\"content\":\"scaffold\"}"}}`
	var (
		mu        sync.Mutex
		bodies    []string
		mainCalls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(body, "load-bearing FACTUAL PREMISES") {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"[]"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mainCalls++
		call := mainCalls
		mu.Unlock()
		switch call {
		case 1:
			plan := "Plan: I can wire the other agent directly through my own OpenAI-compatible API. Scaffolding now."
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q,\"tool_calls\":[%s]}}]}\n", plan, scaffold)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		case 2:
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		default:
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	t.Cleanup(srv.Close)
	client := revisionTestClient(t, srv.URL)
	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.EpistemicPremises = true
	a := New(Options{Config: cfg, Main: client, Cheap: client, Tools: &tools.Manager{}})

	if err := a.Chat(context.Background(), "bridge me with the arena agent"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	t1 := a.turn
	if t1 == nil {
		t.Fatal("turn 1 left no turn handle")
	}
	if t1.ledger == nil || len(t1.ledger.items) == 0 {
		t.Fatal("turn 1 must seed the premise ledger from the self-claim plan")
	}
	if t1.premiseTail == "" {
		t.Fatal("turn 1 must render the premise tail")
	}
	if t1.graph == nil {
		t.Fatal("turn 1's dispatched call must seed the task graph")
	}

	if err := a.Chat(context.Background(), "say hi"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	t2 := a.turn
	if t2 == t1 {
		t.Fatal("Chat entry must construct a FRESH turn — the handle was reused")
	}
	if t2.ledger != nil || t2.premiseTail != "" || t2.graph != nil || t2.graphTail != "" {
		t.Fatalf("turn 1 epistemic state leaked into turn 2: ledger=%v premiseTail=%q graph=%v graphTail=%q",
			t2.ledger, t2.premiseTail, t2.graph, t2.graphTail)
	}
	if t2.mismatchMeter != nil || t2.hypotheses != nil || t2.revisionPending != "" || t2.revisionsThisTurn != 0 {
		t.Fatal("turn 1 revision/prediction state leaked into turn 2")
	}
	// Mid-turn mutations stayed on turn 1's struct — the state moved with the
	// turn, it was not zeroed in place.
	if t1.ledger == nil || len(t1.ledger.items) == 0 || t1.premiseTail == "" {
		t.Fatal("turn 1's own state must survive on its turn struct")
	}

	// The observable no-leak proof: turn 2's outgoing window carries no
	// premise ledger section from turn 1.
	mu.Lock()
	defer mu.Unlock()
	if mainCalls < 3 {
		t.Fatalf("expected 3 main model calls, got %d", mainCalls)
	}
	if !strings.Contains(bodies[1], "Premise ledger") {
		t.Fatal("turn 1's second window must carry its own premise ledger tail")
	}
	if strings.Contains(bodies[2], "Premise ledger") {
		t.Fatal("turn 2's window must NOT carry turn 1's premise ledger tail")
	}
}

// TestTurnIsolation_LoopStateDoesNotLeakAcrossChats proves the completed turn
// struct (MORPHEUS req.2.4, task 2.2) on the real loop: a turn that DIES in a
// no-progress stall fills the turn's stall bookkeeping, loop snapshot, and
// death capture — and the next Chat starts from a fresh turn sharing none of
// it. The key behavioral read: LastDeath reports nothing after a clean turn
// that followed a dead one (no supervisor misattribution), with zero manual
// field zeroing at Chat entry.
func TestTurnIsolation_LoopStateDoesNotLeakAcrossChats(t *testing.T) {
	const batch = `{"index":0,"id":"same","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"a.txt\",\"content\":\"x\"}"}}`
	var (
		mu    sync.Mutex
		turn2 bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		mu.Lock()
		second := turn2
		mu.Unlock()
		if second {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", batch)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	client := revisionTestClient(t, srv.URL)
	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.EpistemicPremises = false
	cfg.EpistemicPredictions = false
	cfg.MaxGuidanceNudges = 0 // let the pure-repeat stall own this death
	cfg.MaxRetriesPerTool = 0
	a := New(Options{Config: cfg, Main: client, Cheap: client, Tools: &tools.Manager{}})

	err := a.Chat(context.Background(), "spiral turn")
	if err == nil || !strings.Contains(err.Error(), "without progress") {
		t.Fatalf("turn 1 must die in the no-progress stall, got %v", err)
	}
	t1 := a.turn
	if d, ok := a.LastDeath(); !ok || d.Reason != DeathReasonStall {
		t.Fatalf("turn 1 must record a stall death, got ok=%v d=%+v", ok, d)
	}
	if t1.repeats < cfg.NoProgressStall || len(t1.recentSigs) == 0 || t1.prevSig == "" || len(t1.distinctToolSet) == 0 {
		t.Fatalf("turn 1 must fill the stall bookkeeping: repeats=%d sigs=%d prevSig=%q tools=%d",
			t1.repeats, len(t1.recentSigs), t1.prevSig, len(t1.distinctToolSet))
	}
	if t1.curLoop.lastTool != "write_file" {
		t.Fatalf("turn 1's loop snapshot must capture the real last tool, got %q", t1.curLoop.lastTool)
	}

	mu.Lock()
	turn2 = true
	mu.Unlock()
	if err := a.Chat(context.Background(), "say hi"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	t2 := a.turn
	if t2 == t1 {
		t.Fatal("Chat entry must construct a FRESH turn")
	}
	if _, ok := a.LastDeath(); ok {
		t.Fatal("a clean turn after a dead one must NOT report the old death (supervisor misattribution)")
	}
	if a.LastFailureClass() != 0 {
		t.Fatalf("turn 2 must start with a clean failure class, got %v", a.LastFailureClass())
	}
	if t2.repeats != 0 || len(t2.recentSigs) != 0 || t2.prevSig != "" || t2.prevCalls != nil ||
		len(t2.distinctToolSet) != 0 || t2.finalSynthesisRetries != 0 ||
		t2.emptyAnswerNudges != 0 || t2.identityNudges != 0 || t2.convergeNudged {
		t.Fatalf("turn 1 stall bookkeeping leaked into turn 2: %+v", t2)
	}
	if t2.casModsThisTurn != 0 || t2.casCooldown != nil || t2.casRecord != nil ||
		t2.autoshotCount != 0 || t2.overflow != nil || t2.lastDeath != nil {
		t.Fatalf("turn-scoped controller/overflow/death state leaked into turn 2: %+v", t2)
	}
	if t2.curLoop.lastTool != "" || t2.curLoop.repeats != 0 || t2.curLoop.recentSigs != nil || t2.curLoop.step != 0 {
		t.Fatalf("turn 1's loop snapshot leaked into turn 2: %+v", t2.curLoop)
	}
	// Turn 1's own record survives on ITS struct (moved, not zeroed): the
	// supervisor could still have read it between the turns.
	if t1.lastDeath == nil || t1.repeats < cfg.NoProgressStall {
		t.Fatal("turn 1's own captures must survive on its turn struct")
	}
}
