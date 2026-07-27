// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResurrectionIncidentMiMoTagLeak_RecoveredCallMarkupNeverPersists(
	t *testing.T,
) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	var step int
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayRequest
			_ = json.Unmarshal(body, &decoded)
			if handleCapabilityCanary(writer, decoded) {
				return
			}
			step++
			if step == 1 {
				writeSSEText(
					writer,
					"Checking.<tool_call><function=exec__shell>"+
						"<parameter=command>printf tag-clean</parameter>"+
						"<parameter=cwd>"+workdir+"</parameter>"+
						"<parameter=expect>prints tag-clean</parameter>"+
						"</function></tool_call>",
				)
				return
			}
			writeSSEText(
				writer,
				"The MiMo textual call ran and printed tag-clean.",
			)
		},
	))
	t.Cleanup(gateway.Close)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	turnID := "incident-mimo-tag-leak"
	store := realTurnStore(
		t, turnID, "Run the tag-clean check and report it.",
	)
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			IdleTimeout: 20 * time.Second,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Turn(
		t.Context(), "Run the tag-clean check and report it.",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.ToolEvents) != 1 ||
		response.ToolEvents[0].Call.Name != "exec__shell" ||
		response.ToolEvents[0].Error != "" {
		t.Fatalf("recovered MiMo dispatch = %+v", response.ToolEvents)
	}
	encoded, err := json.Marshal(response.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{
		"<tool_call>", "<function=", "<parameter=",
	} {
		if strings.Contains(string(encoded), leak) ||
			strings.Contains(response.Content, leak) {
			t.Fatalf("MiMo tag %q leaked: %s", leak, encoded)
		}
	}
}
