// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"matrix/neo/internal/runtime/profile"
	"matrix/vault"
)

func profileRequest(t *testing.T, client *http.Client, method, url, body string) (*http.Response, profile.Profile) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var decoded profile.Profile
	if resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode profile response: %v", err)
		}
	}
	return resp, decoded
}

func TestProfileDaemonIsEncryptedPersistentAndMemoryIndependent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "neo-profile.vault")
	session := mediaVaultSession(t, "did:matrix:profile-test")
	engine := NewEngine(EngineOptions{
		ConversationDir: filepath.Join(root, "conversations"),
		ProfilePath:     path,
		Vault:           session,
		// Pager and backend are intentionally absent: both semantic-memory
		// substrates are unavailable during this onboarding flow.
	})
	server := httptest.NewServer(http.HandlerFunc((&Server{engine: engine}).handleProfile))
	defer server.Close()

	resp, saved := profileRequest(t, server.Client(), http.MethodPut, server.URL,
		`{"preferred_name":"Ada","agent_name":"Nova","expertise_domains":["Go","distributed systems"],"consent_state":"granted"}`)
	if resp.StatusCode != http.StatusOK || saved.PreferredPersonName != "Ada" ||
		saved.AgentName != "Nova" || saved.ProfileVersion != 1 ||
		saved.Consent != profile.ConsentGranted || saved.Deletion != profile.DeletionActive {
		t.Fatalf("PUT /profile = %d %#v", resp.StatusCode, saved)
	}
	agentName, preferredName, domains := engine.profileSnapshot()
	if agentName != "Nova" || preferredName != "Ada" || len(domains) != 2 {
		t.Fatalf("session identity cache was not updated immediately: %q %q %#v", agentName, preferredName, domains)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !vault.IsVault(raw) || bytes.Contains(raw, []byte("Ada")) || bytes.Contains(raw, []byte("Nova")) {
		t.Fatal("profile was not encrypted at rest")
	}

	restarted := NewEngine(EngineOptions{
		ConversationDir: filepath.Join(root, "conversations-restarted"),
		ProfilePath:     path,
		Vault:           session,
	})
	restartServer := httptest.NewServer(http.HandlerFunc((&Server{engine: restarted}).handleProfile))
	defer restartServer.Close()
	resp, loaded := profileRequest(t, restartServer.Client(), http.MethodGet, restartServer.URL, "")
	if resp.StatusCode != http.StatusOK || loaded.PreferredPersonName != "Ada" || loaded.AgentName != "Nova" || loaded.ProfileVersion != 1 {
		t.Fatalf("restart GET /profile = %d %#v", resp.StatusCode, loaded)
	}

	resp, _ = profileRequest(t, restartServer.Client(), http.MethodDelete, restartServer.URL, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /profile = %d", resp.StatusCode)
	}
	resp, loaded = profileRequest(t, restartServer.Client(), http.MethodGet, restartServer.URL, "")
	if resp.StatusCode != http.StatusOK || loaded.PreferredPersonName != "" || loaded.AgentName != "Neo" {
		t.Fatalf("GET after independent delete = %d %#v", resp.StatusCode, loaded)
	}
}

func TestProfileLegacyAssertionMigratesOnce(t *testing.T) {
	var reads atomic.Int32
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"preferred_name": "Lin", "agent_name": "Neo",
			"expertise_domains": []string{"systems"},
			"uri":               "cortex://identity/onboarding/7",
		})
	}))
	root := t.TempDir()
	path := filepath.Join(root, "neo-profile.vault")
	session := mediaVaultSession(t, "did:matrix:profile-migration")
	first := NewEngine(EngineOptions{ConversationDir: filepath.Join(root, "one"), ProfilePath: path, Vault: session, BackendURL: legacy.URL})
	if _, preferred, _ := first.profileSnapshot(); preferred != "Lin" {
		t.Fatalf("legacy profile was not imported: %q", preferred)
	}
	legacy.Close()
	second := NewEngine(EngineOptions{ConversationDir: filepath.Join(root, "two"), ProfilePath: path, Vault: session, BackendURL: "http://127.0.0.1:1"})
	if _, preferred, _ := second.profileSnapshot(); preferred != "Lin" {
		t.Fatalf("persisted migration was not authoritative: %q", preferred)
	}
	if reads.Load() != 1 {
		t.Fatalf("legacy assertion read %d times, want once", reads.Load())
	}
}
