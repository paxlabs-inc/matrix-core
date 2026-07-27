// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package belief

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"matrix/cortex"
	cortexstore "matrix/cortex/store"
	"matrix/neo/internal/runtime/loop"
	"matrix/neo/internal/runtime/turnstate"
	"matrix/vault"
)

// The liveness policy is derived from MEASURED context, not configuration. Real
// failing evidence in the belief state — refuted premises and predictions that
// produced no growth — tightens the next turn's bounds, and the tightened bound
// is then mechanically enforced on the real loop: the repeated strategy stops
// two dispatches earlier than it would have under the healthy baseline.
func TestMeasuredBeliefEvidenceTightensTheNextTurnsEnforcedBounds(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: root,
		UserDID: "did:matrix:liveness-context-test",
		KEKHex:  hex.EncodeToString(bytes.Repeat([]byte{0x44}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	journalStore, err := cortexstore.Open(
		filepath.Join(root, "cortex"), "liveness-context-test", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	journalStore.SetVault(session, "did:matrix:liveness-context-test")
	t.Cleanup(func() { _ = journalStore.Close() })
	cx := cortex.New(journalStore)

	firstTurn := "liveness-context-turn-1"
	userContent := "Probe the service and report what the evidence shows."
	turns := openTurnStore(t, session, root, firstTurn, userContent)
	state, err := New("liveness-context-session", cx, turns)
	if err != nil {
		t.Fatal(err)
	}
	if measured := state.MeasuredContext(); measured.UnsupportedPremises != 0 ||
		measured.ActionsWithoutGrowth != 0 {
		t.Fatalf("a fresh belief state is not the healthy baseline: %+v",
			measured)
	}

	workdir := t.TempDir()
	manager := execManager(t)
	var (
		mu   sync.Mutex
		step int
	)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayProbe
			_ = json.Unmarshal(body, &decoded)
			if answerCapabilityCanary(writer, decoded) {
				return
			}
			mu.Lock()
			step++
			current := step
			mu.Unlock()
			switch current {
			case 1, 2:
				// No command: the bridge's own argument validation returns a
				// real is_error result with a real failure class, so the
				// prediction is genuinely refuted by real evidence.
				writeToolFrame(
					writer, "unsupported-probe", "exec__shell",
					map[string]interface{}{
						"cwd":    workdir,
						"expect": "prints the probe payload",
					},
				)
			default:
				writeTextFrame(
					writer,
					"Both probe attempts failed, so nothing about the service "+
						"is reported as verified.",
				)
			}
		},
	))
	t.Cleanup(gateway.Close)

	tools, err := loop.NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	journal := &loop.CortexToolJournal{
		Cortex: cx, CreatedBy: "did:matrix:liveness-context-test",
	}
	firstLoop, err := loop.New(
		realGenerator(t, gateway.URL), tools, turns,
		loop.Config{
			TurnID: firstTurn, ConversationID: "liveness-context-conversation",
			Model: "mimo-v2", IdleTimeout: 20 * time.Second,
		},
		loop.Dependencies{
			EvidenceJournal:  journal,
			EvidenceObserver: state,
			Liveness:         state,
			Subgoals: callSubgoals{
				"unsupported-probe": "probe-subgoal",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstLoop.Turn(ctx, userContent)
	if err != nil {
		t.Fatalf("first turn = %+v err=%v", first, err)
	}
	// The first turn ran under the healthy baseline: the measured context was
	// clean when it started.
	if first.Liveness.Policy.SameStrategyRetries != 3 ||
		first.Liveness.Policy.VerificationDepth != 2 {
		t.Fatalf("first-turn policy = %+v", first.Liveness.Policy)
	}
	if len(first.ToolEvents) != 2 {
		t.Fatalf("first-turn evidence = %d events", len(first.ToolEvents))
	}
	for _, event := range first.ToolEvents {
		if event.Error == "" ||
			event.MatchVerdict != cortex.ToolMatchMismatched {
			t.Fatalf("expected real refuting evidence, got %+v", event)
		}
	}

	measured := state.MeasuredContext()
	if measured.UnsupportedPremises != 1 || measured.ActionsWithoutGrowth != 2 {
		t.Fatalf("measured context = %+v", measured)
	}

	// Second turn, same belief state. The bounds tighten from the evidence.
	secondTurn := "liveness-context-turn-2"
	repeatContent := "Confirm the marker."
	if err := turns.CreateTurnState(ctx, turnstate.TurnState{
		TurnID: secondTurn, ActorID: "liveness-context-test",
		SessionID: "liveness-context-session",
		Content:   repeatContent, Status: turnstate.StatusRunning,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	repeatGateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayProbe
			_ = json.Unmarshal(body, &decoded)
			if answerCapabilityCanary(writer, decoded) {
				return
			}
			mu.Lock()
			step++
			current := step
			mu.Unlock()
			writeToolFrame(
				writer, "repeat-"+string(rune('a'+current%26)), "exec__shell",
				map[string]interface{}{
					"command": "printf same-marker",
					"cwd":     workdir,
					"expect":  "prints same-marker",
				},
			)
		},
	))
	t.Cleanup(repeatGateway.Close)

	secondLoop, err := loop.New(
		realGenerator(t, repeatGateway.URL), tools, turns,
		loop.Config{
			TurnID: secondTurn, ConversationID: "liveness-context-conversation",
			Model: "mimo-v2", IdleTimeout: 20 * time.Second,
		},
		loop.Dependencies{
			EvidenceJournal:  journal,
			EvidenceObserver: state,
			Liveness:         state,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	second, secondErr := secondLoop.Turn(ctx, repeatContent)

	policy := second.Liveness.Policy
	if policy.SameStrategyRetries != 1 || policy.VerificationDepth != 3 ||
		!policy.AskForHelp {
		t.Fatalf("second-turn policy did not tighten from evidence: %+v",
			policy)
	}
	// Deeper required verification buys MORE action budget to gather the proof
	// with, while the retry bound tightens: the runtime spends its actions on
	// new strategies rather than on repeating a refuted one.
	if policy.ToolCallBudget <= first.Liveness.Policy.ToolCallBudget {
		t.Fatalf("verification depth did not move the action budget: %d vs %d",
			policy.ToolCallBudget, first.Liveness.Policy.ToolCallBudget)
	}
	if !policy.RequiredVerification ||
		policy.SafetyInvariant != first.Liveness.Policy.SafetyInvariant {
		t.Fatalf("the safety floor moved with the bounds: %+v", policy)
	}
	causes := make(map[string]bool)
	for _, cause := range policy.Causes {
		causes[cause.Code] = true
	}
	if !causes["evidence_depth"] || !causes["strategy_revision"] {
		t.Fatalf("tightened policy carried no provenance: %+v", policy.Causes)
	}

	// And the tightened bound is enforced, not advertised: the repeated
	// strategy stops after retries+1 dispatches instead of the baseline four.
	var incomplete *loop.Incomplete
	if !errors.As(secondErr, &incomplete) ||
		incomplete.Phase != "tool_loop" ||
		!strings.Contains(incomplete.RecoveryAdvice, "change_strategy") {
		t.Fatalf("tightened retry bound was not enforced: %+v err=%v",
			second, secondErr)
	}
	if len(second.ToolEvents) != policy.SameStrategyRetries+1 {
		t.Fatalf("dispatched %d repeats under a bound of %d",
			len(second.ToolEvents), policy.SameStrategyRetries)
	}
}
