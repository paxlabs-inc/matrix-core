// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"centra/agents/neo/internal/channelgateway"
	"centra/agents/neo/internal/machinemailsettings"
	"centra/agents/neo/internal/tools"
)

func machineMailOnlyManifest(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	data, err := os.ReadFile(filepath.Clean(filepath.Join(filepath.Dir(source), "../../../agents/neo.json")))
	if err != nil {
		t.Fatalf("read production manifest: %v", err)
	}
	var manifest struct {
		SchemaVersion      int               `json:"schema_version"`
		Agent              string            `json:"agent"`
		Description        string            `json:"description"`
		AllowedSideEffects []string          `json:"allowed_side_effects"`
		Servers            []json.RawMessage `json:"servers"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode production manifest: %v", err)
	}
	var selected json.RawMessage
	for _, raw := range manifest.Servers {
		var identity struct {
			Alias string `json:"alias"`
		}
		if json.Unmarshal(raw, &identity) == nil && identity.Alias == "machine-mail" {
			selected = raw
			break
		}
	}
	if len(selected) == 0 {
		t.Fatal("production MachineMail server is absent")
	}
	manifest.Servers = []json.RawMessage{selected}
	filtered, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "machinemail-agent.json")
	if err := os.WriteFile(path, filtered, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMachineMailProductionAdapterBindsCommonGateway(t *testing.T) {
	apiKey := "mm_ABCDEFGHIJKLMNOPQRSTUVWXYZ1234"
	protocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/mailboxes" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
			t.Errorf("authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]string{{"id": "mailbox-1"}}})
	}))
	defer protocol.Close()
	t.Setenv("MACHINEMAIL_API_URL", protocol.URL+"/v1")

	ctx := context.Background()
	manager, err := tools.Spawn(ctx, tools.Options{ManifestPath: machineMailOnlyManifest(t)})
	if err != nil {
		t.Fatalf("spawn production MachineMail MCP: %v", err)
	}
	defer manager.Close()
	gateway, err := channelgateway.Open(ctx, t.TempDir(), nil, "did:matrix:alice")
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	settings := machinemailsettings.Open("")
	bridge := newMachineMailBridge(settings, manager, gateway, "did:matrix:alice")
	status, err := bridge.Configure(ctx, apiKey)
	if err != nil || !status.Configured {
		t.Fatalf("configure = %+v, %v", status, err)
	}
	address := channelgateway.Address{Channel: channelgateway.ChannelMachineMail, AccountID: "did:matrix:alice", ConversationID: "primary", Scope: channelgateway.ScopeDirect}
	if resolved, ok, err := gateway.Resolve(ctx, address); err != nil || !ok || resolved != "machinemail" {
		t.Fatalf("binding = %q, %v, %v", resolved, ok, err)
	}
	if err := gateway.Unbind(ctx, channelgateway.ChannelMachineMail, "did:matrix:alice", "primary"); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Restore(ctx); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if resolved, ok, err := gateway.Resolve(ctx, address); err != nil || !ok || resolved != "machinemail" {
		t.Fatalf("restored binding = %q, %v, %v", resolved, ok, err)
	}
	if err := bridge.Disconnect(ctx); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if _, ok, err := gateway.Resolve(ctx, address); err != nil || ok {
		t.Fatalf("binding survived disconnect: ok=%v err=%v", ok, err)
	}
}
