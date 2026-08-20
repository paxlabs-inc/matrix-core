// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcpcontrol

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	neotools "centra/agents/neo/internal/tools"
	"centra/packages/vault"
)

func TestRealChronosBridgeQualificationActivationAndIsolation(t *testing.T) {
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	bridge := filepath.Join(root, "protocol", "tools", "chronos", "chronos.mjs")
	if _, err := os.Stat(bridge); err != nil {
		t.Fatalf("real Chronos MCP bridge unavailable: %v", err)
	}
	node, err := findExecutable("node")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := digestCommand(node, []string{bridge})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), nil, "did:matrix:test")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := store.Put(ctx, CreateRequest{Config: Config{
		Alias: "test-chronos", DisplayName: "Test Chronos", Transport: "stdio",
		Command: node, Args: []string{bridge}, PackageDigest: digest, Version: "test-real",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if server.State != StateCandidate {
		t.Fatalf("state=%s", server.State)
	}
	server, err = store.Probe(ctx, server.Config.Alias)
	if err != nil {
		t.Fatalf("real MCP probe: %v", err)
	}
	if server.Health != HealthHealthy || len(server.Tools) == 0 {
		t.Fatalf("probe health=%s tools=%d", server.Health, len(server.Tools))
	}
	classifications := make([]Classification, 0, len(server.Tools))
	for _, discovered := range server.Tools {
		effect := "write"
		enabled := false
		if discovered.Name == "alarm_list" {
			effect = "read"
			enabled = true
		}
		classifications = append(classifications, Classification{Name: discovered.Name, EffectClass: effect, Enabled: enabled})
	}
	server, err = store.Classify(ctx, server.Config.Alias, classifications)
	if err != nil {
		t.Fatal(err)
	}
	server, err = store.Enable(ctx, server.Config.Alias, true)
	if err != nil {
		t.Fatal(err)
	}
	if server.State != StatePending {
		t.Fatalf("state=%s", server.State)
	}
	entries, err := store.RuntimeEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || len(entries[0].Manifest.Tools) != len(server.Tools) {
		t.Fatalf("runtime entries=%d manifest tools=%d discovered=%d", len(entries), len(entries[0].Manifest.Tools), len(server.Tools))
	}
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	manifest, _ := json.Marshal(map[string]any{"schema_version": 1, "agent": "neo:test", "servers": []any{}})
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := neotools.Spawn(ctx, neotools.Options{ManifestPath: manifestPath})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	manager.SetDynamicMCPHooks(store.Guard, store.Observe)
	if err := manager.ReloadDynamicMCP(ctx, entries); err != nil {
		t.Fatalf("atomic real MCP reload: %v", err)
	}
	if err := store.MarkApplied(ctx); err != nil {
		t.Fatal(err)
	}
	active, err := store.Get(ctx, server.Config.Alias)
	if err != nil || active.State != StateActive {
		t.Fatalf("active=%+v err=%v", active, err)
	}
	result, err := manager.CallDirect(ctx, "test-chronos__alarm_list", map[string]any{})
	if err != nil {
		t.Fatalf("optional integration failure escaped into runtime: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "scheduler not configured") {
		t.Fatalf("expected structured isolated Chronos outage, got %+v", result)
	}
	for attempt := 0; attempt < 3; attempt++ {
		store.Observe(server.Config.Alias, 0, context.DeadlineExceeded)
	}
	if err := store.Guard(server.Config.Alias); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("circuit guard=%v", err)
	}
	if _, err := store.Put(ctx, CreateRequest{Config: Config{
		Alias: "test-chronos", DisplayName: "Test Chronos update", Transport: "stdio",
		Command: node, Args: []string{bridge}, PackageDigest: digest, Version: "candidate-update",
	}}); err != nil {
		t.Fatal(err)
	}
	restartEntries, err := store.RuntimeEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(restartEntries) != 1 || restartEntries[0].Manifest.Version != "test-real" {
		t.Fatalf("unqualified candidate displaced last applied runtime: %+v", restartEntries)
	}
}

func TestCredentialsAreVaultSealedAndReloadable(t *testing.T) {
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	bridge := filepath.Join(root, "protocol", "tools", "chronos", "chronos.mjs")
	node, err := findExecutable("node")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := digestCommand(node, []string{bridge})
	if err != nil {
		t.Fatal(err)
	}
	user := "did:matrix:mcp-sealed-test"
	vaultDir := t.TempDir()
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: vaultDir, UserDID: user,
		KEKHex: hex.EncodeToString(bytes.Repeat([]byte{0x73}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	controlDir := t.TempDir()
	store, err := Open(context.Background(), controlDir, session, user)
	if err != nil {
		t.Fatal(err)
	}
	secret := "test-secret-value-that-must-not-be-plaintext"
	_, err = store.Put(context.Background(), CreateRequest{
		Config: Config{
			Alias: "sealed-chronos", DisplayName: "Sealed Chronos", Transport: "stdio",
			Command: node, Args: []string{bridge}, EnvKeys: []string{"TEST_MCP_TOKEN"},
			PackageDigest: digest, Version: "sealed-real",
		},
		SecretEnv: map[string]string{"TEST_MCP_TOKEN": secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	sealed, err := os.ReadFile(filepath.Join(controlDir, secretFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(secret)) || bytes.Contains(sealed, []byte("TEST_MCP_TOKEN")) {
		t.Fatal("credential file contains plaintext credential material")
	}
	info, err := os.Stat(filepath.Join(controlDir, secretFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode=%o", info.Mode().Perm())
	}
	reopened, err := Open(context.Background(), controlDir, session, user)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.serverSecrets("sealed-chronos")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Env["TEST_MCP_TOKEN"] != secret {
		t.Fatal("sealed credential did not survive restart")
	}
	if redacted := reopened.Redact("sealed-chronos", "remote echoed "+secret); strings.Contains(redacted, secret) {
		t.Fatal("configured credential escaped integration result redaction")
	}
	accessToken := "oauth-access-token-that-must-remain-sealed"
	refreshToken := "oauth-refresh-token-that-must-remain-sealed"
	if err := reopened.storeTokens("sealed-chronos", tokenResponse{AccessToken: accessToken, RefreshToken: refreshToken, ExpiresIn: 3600}); err != nil {
		t.Fatal(err)
	}
	sealed, err = os.ReadFile(filepath.Join(controlDir, secretFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(accessToken)) || bytes.Contains(sealed, []byte(refreshToken)) {
		t.Fatal("OAuth token material was persisted in plaintext")
	}
	loaded, err = reopened.serverSecrets("sealed-chronos")
	if err != nil || loaded.OAuth.AccessToken != accessToken || loaded.OAuth.RefreshToken != refreshToken {
		t.Fatalf("sealed OAuth token reload failed: %v", err)
	}
}

func findExecutable(name string) (string, error) {
	paths := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))
	for _, directory := range paths {
		candidate := filepath.Join(directory, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return filepath.Abs(candidate)
		}
	}
	return "", os.ErrNotExist
}
