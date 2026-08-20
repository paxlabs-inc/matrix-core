// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_vault_test.go — ORACLE task 2.1 (executor daemon half): the daemon's
// transcript writer and conversation-thread store seal at rest through the real
// vault over real files. No fakes: a real vault.Session over a real KEK, real
// files on disk, the real read paths.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"centra/packages/vault"
)

// vaultSessionFor boots a real encrypting vault.Session for a user, keyed by a
// per-call temp DataDir so each user gets a distinct wrapped user key.
func vaultSessionFor(t *testing.T, user string) *vault.Session {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")) // 32 bytes
	sess, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: user, KEKHex: kek,
	})
	if err != nil {
		t.Fatalf("vault boot: %v", err)
	}
	if !sess.Encrypting() {
		t.Fatal("expected an encrypting session")
	}
	return sess
}

func rawLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// TestVaultTranscriptRoundTrip proves the transcript writer seals each line and
// readTranscriptLines decrypts them back under the reconstructed AD.
func TestVaultTranscriptRoundTrip(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	intent := "intent-1"
	sess := vaultSessionFor(t, user)

	tp := filepath.Join(dir, intent+".jsonl")
	tr, err := openTranscript(tp)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	tr.SetVault(sess, user)
	tr.SetIntentID(intent)
	tr.Event("walk.cortex.pre", "walk", map[string]interface{}{"overall_root": "root-abc"})
	tr.Event("step.text", "walk", map[string]interface{}{"text": "a private answer"})
	if err := tr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// On disk every line must be sealed (no plaintext content).
	lines := rawLines(t, tp)
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	for i, l := range lines {
		if !vault.IsSealedLine([]byte(l)) {
			t.Fatalf("transcript line %d not sealed: %q", i, l)
		}
		if strings.Contains(l, "private answer") {
			t.Fatalf("plaintext leaked on line %d", i)
		}
	}

	// The daemon read path decrypts under the reconstructed AD.
	d := &daemonState{transcriptsDir: dir, vault: sess, vaultUser: user}
	recs, err := d.readTranscriptLines(intent)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 decrypted records, got %d", len(recs))
	}
	var ev struct {
		Type   string                 `json:"type"`
		Fields map[string]interface{} `json:"fields"`
	}
	if err := json.Unmarshal(recs[0], &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.Type != "walk.cortex.pre" || ev.Fields["overall_root"] != "root-abc" {
		t.Fatalf("record 0 mismatch: %+v", ev)
	}
}

// TestVaultTranscriptWrongUserSkips proves a different user's key cannot read
// the sealed transcript — every record fails authentication and is skipped.
func TestVaultTranscriptWrongUserSkips(t *testing.T) {
	dir := t.TempDir()
	intent := "intent-1"
	alice := vaultSessionFor(t, "did:matrix:alice")

	tp := filepath.Join(dir, intent+".jsonl")
	tr, err := openTranscript(tp)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	tr.SetVault(alice, "did:matrix:alice")
	tr.SetIntentID(intent)
	tr.Event("step.text", "walk", map[string]interface{}{"text": "secret"})
	_ = tr.Close()

	bob := vaultSessionFor(t, "did:matrix:bob")
	d := &daemonState{transcriptsDir: dir, vault: bob, vaultUser: "did:matrix:bob"}
	recs, err := d.readTranscriptLines(intent)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("bob must not read alice's transcript, got %d records", len(recs))
	}
}

// TestVaultTranscriptLegacyPlaintext proves pre-migration plaintext transcripts
// still read through the sealing daemon (header sniffing).
func TestVaultTranscriptLegacyPlaintext(t *testing.T) {
	dir := t.TempDir()
	intent := "legacy-1"
	// Write a legacy plaintext JSONL transcript directly.
	tp := filepath.Join(dir, intent+".jsonl")
	legacy := `{"seq":1,"ts":"t","phase":"walk","type":"step.text","fields":{"text":"hi"}}` + "\n"
	if err := os.WriteFile(tp, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	d := &daemonState{transcriptsDir: dir, vault: vaultSessionFor(t, "did:matrix:alice"), vaultUser: "did:matrix:alice"}
	recs, err := d.readTranscriptLines(intent)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 legacy record, got %d", len(recs))
	}
}

// TestVaultConversationRoundTrip proves the daemon conversation store seals the
// whole file and reads it back under the reconstructed AD.
func TestVaultConversationRoundTrip(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	sess := vaultSessionFor(t, user)

	cs := newConversationStore(dir)
	cs.SetVault(sess, user)
	cs.AppendUser("c1", "what is the block height")
	cs.AppendAssistant("c1", "i1", "the height is private")

	path := filepath.Join(dir, "c1.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !vault.IsVault(data) {
		t.Fatal("conversation file not sealed")
	}
	if strings.Contains(string(data), "private") {
		t.Fatal("plaintext leaked in sealed conversation file")
	}

	turns := cs.Recent("c1", 10)
	if len(turns) != 2 || turns[0].Text != "what is the block height" || turns[1].IntentID != "i1" {
		t.Fatalf("round-trip mismatch: %+v", turns)
	}
}

// TestVaultConversationWrongUser proves a different user's key yields no turns
// (loadLocked returns an empty record rather than leaking bytes).
func TestVaultConversationWrongUser(t *testing.T) {
	dir := t.TempDir()
	alice := vaultSessionFor(t, "did:matrix:alice")
	cs := newConversationStore(dir)
	cs.SetVault(alice, "did:matrix:alice")
	cs.AppendUser("c1", "hello")

	bob := vaultSessionFor(t, "did:matrix:bob")
	cs2 := newConversationStore(dir)
	cs2.SetVault(bob, "did:matrix:bob")
	if turns := cs2.Recent("c1", 10); turns != nil {
		t.Fatalf("bob must not read alice's conversation, got %+v", turns)
	}
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
