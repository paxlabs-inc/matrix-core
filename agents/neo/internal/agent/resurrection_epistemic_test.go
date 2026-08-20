// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"centra/agents/neo/internal/runtime/belief"
)

type epistemicRequest struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []json.RawMessage `json:"tools"`
}

func (request epistemicRequest) text(index int) string {
	raw := request.Messages[index].Content
	var decoded string
	if json.Unmarshal(raw, &decoded) == nil {
		return decoded
	}
	return string(raw)
}

func (request epistemicRequest) latest() string {
	return request.text(len(request.Messages) - 1)
}

// The runtime's epistemic core has to be REACHABLE from the production
// construction site, not merely present in the tree. A guessed identifier that
// fails on contact must land in the durable belief ledger as a refuted premise
// and must arm the silent voice into a tools-stripped revision step; and
// retrieved memory must never occupy the user role, where it stands as the most
// recent user turn and gets read as the live request. Both seams were nil in
// production, which is what let a fabricated hostname be narrated as an
// environment limit and let recalled text from another session redirect a turn.
func TestResurrectionEpistemicCoreRefutesAGuessAndKeepsMemoryOutOfTheUserRole(
	t *testing.T,
) {
	const userTurn = "Check the sequencer balance and report what the evidence shows."
	const prediction = "the sequencer returns the balance JSON"
	workdir := t.TempDir()

	var mu sync.Mutex
	var seen []epistemicRequest
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			var decoded epistemicRequest
			if json.Unmarshal(body, &decoded) != nil ||
				len(decoded.Messages) == 0 {
				http.Error(
					writer, "invalid request", http.StatusBadRequest,
				)
				return
			}
			latest := decoded.latest()
			switch {
			case strings.Contains(latest, "Reply with READY"):
				writeCutoverText(writer, "READY")
				return
			case strings.Contains(
				latest, "matrix_runtime_capability_echo",
			):
				writeCutoverTool(
					writer, "capability-call",
					"matrix_runtime_capability_echo",
					map[string]interface{}{
						"value": "READY", "expect": "returns READY",
					},
				)
				return
			}
			mu.Lock()
			seen = append(seen, decoded)
			attempt := len(seen)
			mu.Unlock()
			switch attempt {
			case 1:
				// A grounded step first: the Cassandra cadence keeps the
				// silent voice quiet below min_step, so the mismatch has to
				// land on a later attempt to exercise the controller at all.
				writeCutoverTool(
					writer, "ready-call", "exec__shell",
					map[string]interface{}{
						"command": "echo ready",
						"cwd":     workdir,
						"expect":  "prints ready",
					},
				)
			case 2:
				writeCutoverTool(
					writer, "sequencer-call", "exec__shell",
					map[string]interface{}{
						"command": "printf 'could not resolve host\\n' >&2; " +
							"exit 7",
						"cwd":    workdir,
						"expect": prediction,
					},
				)
			default:
				writeCutoverText(
					writer,
					"The host I used was my own guess and it never resolved, "+
						"so I have no balance to report.",
				)
			}
		},
	))
	t.Cleanup(gateway.Close)

	manager := cutoverProbeManager(t)
	cfg, pager, runtime := openCutoverRuntime(t, gateway.URL, manager)
	reporter := &cutoverReporter{}
	agent := New(Options{
		Config: cfg, Tools: manager, Pager: pager,
		Reporter: reporter, Runtime: runtime,
	})
	if err := agent.Chat(t.Context(), userTurn); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	mu.Lock()
	requests := append([]epistemicRequest(nil), seen...)
	mu.Unlock()
	if len(requests) < 3 {
		t.Fatalf("provider attempts = %d, want at least 3", len(requests))
	}

	// The mismatch arms the silent voice, and the revision step runs with the
	// tools stripped so the model must revise the plan instead of dispatching
	// another sibling call against the same guess.
	revision := requests[2]
	folded := false
	for index := range revision.Messages {
		if strings.Contains(
			revision.text(index), "not what I predicted",
		) {
			folded = true
		}
	}
	if !folded {
		t.Fatalf(
			"revision attempt carries no doubt line: %+v", revision.Messages,
		)
	}
	if len(revision.Tools) != 0 {
		t.Fatalf(
			"revision attempt kept %d tools, want a tools-stripped step",
			len(revision.Tools),
		)
	}

	// Retrieved memory rides the system role in every attempt. The only
	// user-role message in the window is the turn the user actually sent.
	for index, request := range requests {
		for position := range request.Messages {
			if request.Messages[position].Role != "user" {
				continue
			}
			if content := request.text(position); content != userTurn {
				t.Fatalf(
					"attempt %d put non-user content in the user role: %q",
					index+1, content,
				)
			}
		}
	}

	// The refutation is durable: it survives the turn in the belief ledger, so
	// the next turn's measured context still knows the premise is unsupported.
	raw, err := runtime.store.LoadCognitionState(t.Context(), "cli")
	if err != nil {
		t.Fatalf("load cognition state: %v", err)
	}
	var cognition belief.DurableState
	if err := json.Unmarshal(raw, &cognition); err != nil {
		t.Fatalf("decode cognition state: %v", err)
	}
	refuted := false
	for _, premise := range cognition.Premises {
		if premise.Status == belief.PremiseRefuted &&
			premise.Statement == prediction {
			refuted = true
		}
	}
	if !refuted {
		t.Fatalf(
			"belief ledger holds no refuted premise for the guess: %+v",
			cognition.Premises,
		)
	}
	if len(cognition.Predictions) < 2 {
		t.Fatalf(
			"belief ledger recorded %d predictions, want both dispatches",
			len(cognition.Predictions),
		)
	}
}
