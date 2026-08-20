// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// media_vault_test.go — ORACLE task 2.4 (media half): uploads and browser
// screenshots seal at rest (opt-in via NEO_MEDIA_SEAL) and GET /media
// decrypt-streams them back; legacy plaintext keeps serving; the flag stays
// OFF by default. No fakes: real vault sessions, real handlers, real files.

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"centra/packages/vault"
)

func mediaVaultSession(t *testing.T, user string) *vault.Session {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
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

func getMedia(t *testing.T, s *Server, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleMedia(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

// TestVaultMediaUploadSealedRoundTrip proves POST /upload seals the stored
// file (ciphertext on disk, plaintext byte count reported) and GET /media
// decrypt-streams it back with the right type.
func TestVaultMediaUploadSealedRoundTrip(t *testing.T) {
	t.Setenv(envMediaSeal, "true")
	dir := t.TempDir()
	user := "did:matrix:alice"
	s := &Server{engine: &Engine{mediaDir: dir, vault: mediaVaultSession(t, user), vaultUser: user}}

	body := "%PDF-1.7 the private document body"
	code, out := uploadOne(t, s, "secret.pdf", "application/pdf", body)
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%v), want 201", code, out)
	}
	if int(out["bytes"].(float64)) != len(body) {
		t.Fatalf("bytes = %v, want plaintext len %d", out["bytes"], len(body))
	}
	url, _ := out["url"].(string)
	name := strings.TrimPrefix(url, "/media/")

	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read stored: %v", err)
	}
	if !vault.IsVault(raw) {
		t.Fatal("stored upload is not sealed")
	}
	if strings.Contains(string(raw), "private document") {
		t.Fatal("plaintext leaked into the sealed upload")
	}

	rec := getMedia(t, s, url)
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("GET sealed = %d %q, want 200 with original body", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("document served without download disposition: %q", cd)
	}
}

// TestVaultMediaScreenshotSealed proves persistToolImage seals the screenshot
// and the filmstrip GET path decrypts it byte-identically.
func TestVaultMediaScreenshotSealed(t *testing.T) {
	t.Setenv(envMediaSeal, "true")
	dir := t.TempDir()
	user := "did:matrix:alice"
	e := &Engine{mediaDir: dir, vault: mediaVaultSession(t, user), vaultUser: user}
	s := &Server{engine: e}

	shot := []byte("\xff\xd8\xff\xe0 fake jpeg viewport bytes of a private page")
	url, err := e.persistToolImage("image/jpeg", base64.StdEncoding.EncodeToString(shot))
	if err != nil {
		t.Fatalf("persistToolImage: %v", err)
	}
	name := strings.TrimPrefix(url, "/media/")
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read stored: %v", err)
	}
	if !vault.IsVault(raw) {
		t.Fatal("screenshot not sealed on disk")
	}

	rec := getMedia(t, s, url)
	if rec.Code != http.StatusOK || rec.Body.String() != string(shot) {
		t.Fatalf("GET screenshot = %d (%d bytes), want 200 with original bytes", rec.Code, rec.Body.Len())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// TestVaultMediaSealDefaultOff proves that with the vault up but no explicit
// NEO_MEDIA_SEAL, writes stay plaintext — the flip is an owner decision (the
// Node media bridge and agent file reads consume media raw off the volume).
func TestVaultMediaSealDefaultOff(t *testing.T) {
	t.Setenv(envMediaSeal, "")
	dir := t.TempDir()
	user := "did:matrix:alice"
	s := &Server{engine: &Engine{mediaDir: dir, vault: mediaVaultSession(t, user), vaultUser: user}}

	body := "plain payload"
	code, out := uploadOne(t, s, "note.txt", "text/plain", body)
	if code != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", code)
	}
	name := strings.TrimPrefix(out["url"].(string), "/media/")
	raw, _ := os.ReadFile(filepath.Join(dir, name))
	if vault.IsVault(raw) {
		t.Fatal("upload sealed without explicit NEO_MEDIA_SEAL opt-in")
	}
	if string(raw) != body {
		t.Fatalf("stored bytes = %q", raw)
	}
}

// TestVaultMediaWrongUserFailsClosed proves another user's session cannot
// serve a sealed media file as plaintext.
func TestVaultMediaWrongUserFailsClosed(t *testing.T) {
	t.Setenv(envMediaSeal, "true")
	dir := t.TempDir()
	s := &Server{engine: &Engine{
		mediaDir: dir, vault: mediaVaultSession(t, "did:matrix:alice"), vaultUser: "did:matrix:alice",
	}}
	code, out := uploadOne(t, s, "secret.pdf", "application/pdf", "the private document")
	if code != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", code)
	}
	url, _ := out["url"].(string)

	s2 := &Server{engine: &Engine{
		mediaDir: dir, vault: mediaVaultSession(t, "did:matrix:mallory"), vaultUser: "did:matrix:mallory",
	}}
	rec := getMedia(t, s2, url)
	if strings.Contains(rec.Body.String(), "private document") {
		t.Fatal("wrong user's session served sealed media plaintext")
	}
}

// TestVaultMediaLegacyPlaintextServes proves pre-vault plaintext media keeps
// serving through the sniffing handler when the vault is active.
func TestVaultMediaLegacyPlaintextServes(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	if err := os.WriteFile(filepath.Join(dir, "old.png"), []byte("PNG legacy bytes"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := &Server{engine: &Engine{mediaDir: dir, vault: mediaVaultSession(t, user), vaultUser: user}}
	rec := getMedia(t, s, "/media/old.png")
	if rec.Code != http.StatusOK || rec.Body.String() != "PNG legacy bytes" {
		t.Fatalf("legacy serve = %d %q", rec.Code, rec.Body.String())
	}
}
