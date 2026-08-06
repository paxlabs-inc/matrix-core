// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPartialResearchSynthesizesSuccessfulEvidenceInsteadOfRegressing(t *testing.T) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	var (
		mu   sync.Mutex
		step int
	)
	gateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var decoded gatewayRequest
		_ = json.Unmarshal(body, &decoded)
		if handleCapabilityCanary(writer, decoded) {
			return
		}
		for _, message := range decoded.Messages {
			if strings.Contains(message.Content, "Tool execution is finished for this turn") {
				writeSSEText(writer, "Enterprises report that AI governance, data quality, privacy, integration cost, and unreliable outputs remain valid operational concerns. The completed evidence confirmed the governance finding; one additional probe failed, so that unresolved item should be treated as a limitation rather than silently discarded.")
				return
			}
		}
		mu.Lock()
		step++
		current := step
		mu.Unlock()
		switch current {
		case 1:
			writeSSETool(writer, "research-success", "exec__shell", map[string]interface{}{
				"command": "printf governance", "cwd": workdir,
				"expect": "prints governance",
			})
		case 2:
			writeSSETool(writer, "research-failure", "exec__shell", map[string]interface{}{
				"command": "sh -c 'printf unavailable >&2; exit 7'", "cwd": workdir,
				"expect": "returns another enterprise concern",
			})
		default:
			writeSSETool(writer, "research-over-limit", "exec__shell", map[string]interface{}{
				"command": "printf unnecessary", "cwd": workdir,
				"expect": "prints unnecessary",
			})
		}
	}))
	t.Cleanup(gateway.Close)

	const turnID = "partial-research-synthesis"
	const request = "Research what enterprises complain about regarding AI and give me a report."
	store := realTurnStore(t, turnID, request)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2", MaxToolCalls: 2,
			IdleTimeout: 30 * time.Second,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := runtimeLoop.Turn(t.Context(), request)
	if turnErr != nil {
		t.Fatalf("partial research must deliver a synthesis: %v", turnErr)
	}
	if !response.HonestPartial {
		t.Fatalf("response must retain its partial epistemic status: %+v", response)
	}
	if !strings.Contains(response.Content, "Enterprises report") ||
		strings.Contains(response.Content, "completed action") ||
		strings.Contains(response.Content, "could not produce a reliable") {
		t.Fatalf("response did not synthesize the preserved evidence: %q", response.Content)
	}
	if len(response.ToolEvents) != 2 || response.ToolEvents[0].Error != "" ||
		response.ToolEvents[1].Error == "" {
		t.Fatalf("successful and failed sibling evidence was not preserved: %+v", response.ToolEvents)
	}
}
