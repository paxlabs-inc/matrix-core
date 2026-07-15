// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package telegramsettings

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"matrix/vault"
)

func telegramVault(t *testing.T, user string) *vault.Session {
	t.Helper()
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true,
		DataDir:  t.TempDir(),
		UserDID:  user,
		KEKHex:   hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatalf("boot vault: %v", err)
	}
	return session
}

func TestPersistentStoreRequiresEncryption(t *testing.T) {
	store := Open(t.TempDir())
	err := store.Replace(State{Token: "123456:secret-value", BotID: 1, BotUsername: "neo_bot"})
	if !errors.Is(err, ErrEncryptionRequired) {
		t.Fatalf("expected ErrEncryptionRequired, got %v", err)
	}
}

func TestEncryptedRoundTripPairAndClear(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	session := telegramVault(t, user)
	store := Open(dir)
	store.SetVault(session, user)

	token := "123456789:abcdefghijklmnopqrstuvwxyzABCDE"
	if err := store.Replace(State{
		Token:       token,
		BotID:       42,
		BotUsername: "matrix_neo_bot",
		PairSecret:  "pair-secret",
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	raw, err := os.ReadFile(store.path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !vault.IsVault(raw) || strings.Contains(string(raw), token) {
		t.Fatal("telegram token was not sealed at rest")
	}

	reopened := Open(dir)
	reopened.SetVault(session, user)
	if reopened.View().Token != token {
		t.Fatal("encrypted token did not round trip")
	}
	paired, err := reopened.Pair("pair-secret", 100, 100, "alice", "tg-42-100")
	if err != nil || !paired {
		t.Fatalf("pair: paired=%v err=%v", paired, err)
	}
	state := reopened.View()
	if !state.Paired() || state.PairSecret != "" || state.ConversationID != "tg-42-100" {
		t.Fatalf("bad paired state: %+v", state)
	}
	if err := reopened.Clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(reopened.path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings file still exists: %v", err)
	}
}
