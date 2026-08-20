// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_vault2_test.go — ORACLE task 2.2 (executor daemon half): the async
// job registry and the persisted intent-envelope journal seal at rest through
// the real vault over real files. No fakes: a real vault.Session over a real
// KEK, real files on disk, the real read paths (loadFromDir, openEnvelopeFile,
// readEnvelopeBody).

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"centra/core/mcl/envelope"
	"centra/packages/vault"
)

// --- async registry ---

// TestVaultAsyncJobRoundTrip proves persisted jobs are sealed on disk (no
// plaintext prose) and rehydrate through a fresh registry under the same key.
func TestVaultAsyncJobRoundTrip(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	sess := vaultSessionFor(t, user)

	r := newAsyncRegistry(8, dir)
	r.SetVault(sess, user)
	if _, err := r.CreateQueued("intent-a", "u1", messageRequest{Prose: "the secret prose"}); err != nil {
		t.Fatalf("CreateQueued: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "intent-a.json"))
	if err != nil {
		t.Fatalf("read job file: %v", err)
	}
	if !vault.IsVault(raw) {
		t.Fatal("job file is not sealed")
	}
	if strings.Contains(string(raw), "secret prose") {
		t.Fatal("plaintext leaked into the sealed job file")
	}

	r2 := newAsyncRegistry(8, dir)
	r2.SetVault(sess, user)
	job := r2.Get("intent-a")
	if job == nil || job.Request.Prose != "the secret prose" || job.Status != asyncQueued {
		t.Fatalf("rehydrate mismatch: %+v", job)
	}
}

// TestVaultAsyncJobWrongUser proves another user's key cannot rehydrate jobs.
func TestVaultAsyncJobWrongUser(t *testing.T) {
	dir := t.TempDir()
	r := newAsyncRegistry(8, dir)
	r.SetVault(vaultSessionFor(t, "did:matrix:alice"), "did:matrix:alice")
	if _, err := r.CreateQueued("intent-a", "u1", messageRequest{Prose: "private"}); err != nil {
		t.Fatalf("CreateQueued: %v", err)
	}

	r2 := newAsyncRegistry(8, dir)
	r2.SetVault(vaultSessionFor(t, "did:matrix:mallory"), "did:matrix:mallory")
	if job := r2.Get("intent-a"); job != nil {
		t.Fatal("wrong user rehydrated a sealed job")
	}
}

// TestVaultAsyncJobLegacyPlaintext proves a pre-vault plaintext job stays
// readable once the vault is wired (sniffing loader).
func TestVaultAsyncJobLegacyPlaintext(t *testing.T) {
	dir := t.TempDir()
	plain := newAsyncRegistry(8, dir)
	if _, err := plain.CreateQueued("intent-old", "u1", messageRequest{Prose: "old plain prose"}); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "intent-old.json")); !json.Valid(b) {
		t.Fatal("seed job should be plaintext JSON")
	}

	r := newAsyncRegistry(8, dir)
	r.SetVault(vaultSessionFor(t, "did:matrix:alice"), "did:matrix:alice")
	job := r.Get("intent-old")
	if job == nil || job.Request.Prose != "old plain prose" {
		t.Fatalf("legacy plaintext unreadable: %+v", job)
	}
}

// --- intent envelope journal ---

func testEnvelopeStream(t *testing.T, journalDir, intentID string) (*envelopeStream, *actorIdentity) {
	t.Helper()
	actor, err := loadOrCreateIdentity(filepath.Join(t.TempDir(), "actor.key"), "vaulttest")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	tr, err := openTranscript(filepath.Join(t.TempDir(), "t.jsonl"))
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	es, err := newEnvelopeStream(journalDir, intentID, actor, tr)
	if err != nil {
		t.Fatalf("envelope stream: %v", err)
	}
	return es, actor
}

// TestVaultEnvelopeRoundTrip proves signed envelopes are sealed on disk (no
// plaintext body) and decrypt back through the daemon read paths, with the
// signature verifying over the decrypted logical bytes.
func TestVaultEnvelopeRoundTrip(t *testing.T) {
	journal := t.TempDir()
	user := "did:matrix:alice"
	intent := "INTENTVAULT0000000000000A"
	sess := vaultSessionFor(t, user)

	es, actor := testEnvelopeStream(t, journal, intent)
	es.SetVault(sess, user)
	env, err := es.SignAndPersist("intent.compiled", envelope.IntentCompiledBody{IntentJSON: []byte(`{"prose":"the secret intent"}`)}, "", "")
	if err != nil {
		t.Fatalf("SignAndPersist: %v", err)
	}

	path := filepath.Join(journal, intent, "0001-intent-compiled.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read envelope file: %v", err)
	}
	if !vault.IsVault(raw) {
		t.Fatal("envelope file is not sealed")
	}
	// IntentJSON is []byte so it rides as base64 in the persisted JSON — the
	// leak check must look for the encoded form, not the raw string.
	b64 := base64.StdEncoding.EncodeToString([]byte(`{"prose":"the secret intent"}`))
	if strings.Contains(string(raw), b64) || strings.Contains(string(raw), `"kind"`) {
		t.Fatal("plaintext leaked into the sealed envelope file")
	}

	d := &daemonState{vault: sess, vaultUser: user, actor: actor}
	plain, err := d.openEnvelopeFile(path)
	if err != nil {
		t.Fatalf("openEnvelopeFile: %v", err)
	}
	if !strings.Contains(string(plain), b64) {
		t.Fatal("decrypted envelope missing body content")
	}
	body, err := d.readEnvelopeBody(path)
	if err != nil {
		t.Fatalf("readEnvelopeBody: %v", err)
	}
	if body.ID != env.ID || body.Kind != "intent.compiled" {
		t.Fatalf("envelope body mismatch: %+v vs env id %s", body, env.ID)
	}
}

// TestVaultEnvelopeWrongUser proves another user's key cannot open a sealed
// envelope through the daemon read path.
func TestVaultEnvelopeWrongUser(t *testing.T) {
	journal := t.TempDir()
	intent := "INTENTVAULT0000000000000B"
	es, actor := testEnvelopeStream(t, journal, intent)
	es.SetVault(vaultSessionFor(t, "did:matrix:alice"), "did:matrix:alice")
	if _, err := es.SignAndPersist("intent.compiled", envelope.IntentCompiledBody{IntentJSON: []byte(`{"prose":"private"}`)}, "", ""); err != nil {
		t.Fatalf("SignAndPersist: %v", err)
	}

	d := &daemonState{
		vault:     vaultSessionFor(t, "did:matrix:mallory"),
		vaultUser: "did:matrix:mallory",
		actor:     actor,
	}
	path := filepath.Join(journal, intent, "0001-intent-compiled.json")
	if _, err := d.openEnvelopeFile(path); err == nil {
		t.Fatal("wrong user opened a sealed envelope")
	}
}

// TestVaultEnvelopePositionBound proves the AD binds the chain position: a
// sealed envelope renamed to another seq fails authentication.
func TestVaultEnvelopePositionBound(t *testing.T) {
	journal := t.TempDir()
	user := "did:matrix:alice"
	intent := "INTENTVAULT0000000000000C"
	sess := vaultSessionFor(t, user)
	es, actor := testEnvelopeStream(t, journal, intent)
	es.SetVault(sess, user)
	if _, err := es.SignAndPersist("intent.compiled", envelope.IntentCompiledBody{IntentJSON: []byte(`{"k":"v"}`)}, "", ""); err != nil {
		t.Fatalf("SignAndPersist: %v", err)
	}

	dir := filepath.Join(journal, intent)
	src := filepath.Join(dir, "0001-intent-compiled.json")
	dst := filepath.Join(dir, "0002-intent-compiled.json")
	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("rename: %v", err)
	}
	d := &daemonState{vault: sess, vaultUser: user, actor: actor}
	if _, err := d.openEnvelopeFile(dst); err == nil {
		t.Fatal("re-positioned sealed envelope still opened")
	}
}

// TestVaultEnvelopeLegacyPlaintext proves a pre-vault plaintext envelope stays
// readable through the daemon read path (sniffing reader).
func TestVaultEnvelopeLegacyPlaintext(t *testing.T) {
	journal := t.TempDir()
	intent := "INTENTVAULT0000000000000D"
	es, actor := testEnvelopeStream(t, journal, intent)
	// No SetVault: legacy plaintext write path.
	if _, err := es.SignAndPersist("intent.compiled", envelope.IntentCompiledBody{IntentJSON: []byte(`{"prose":"old plain"}`)}, "", ""); err != nil {
		t.Fatalf("SignAndPersist: %v", err)
	}

	path := filepath.Join(journal, intent, "0001-intent-compiled.json")
	if b, _ := os.ReadFile(path); !json.Valid(b) {
		t.Fatal("seed envelope should be plaintext JSON")
	}
	d := &daemonState{vault: vaultSessionFor(t, "did:matrix:alice"), vaultUser: "did:matrix:alice", actor: actor}
	body, err := d.readEnvelopeBody(path)
	if err != nil {
		t.Fatalf("readEnvelopeBody: %v", err)
	}
	if body.Kind != "intent.compiled" {
		t.Fatalf("legacy envelope kind = %q", body.Kind)
	}
}
