package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"centra/gateway/internal/auth"
	"centra/gateway/internal/types"
)

func TestLoadAgentCoreVerificationKeysIncludesActiveAndRotatedKeys(t *testing.T) {
	active := bytes.Repeat([]byte("a"), 32)
	previous := bytes.Repeat([]byte("p"), 32)
	environment := map[string]string{
		agentCoreActiveKeyIDEnv:      "active-2",
		agentCoreSigningKeyEnv:       base64.RawURLEncoding.EncodeToString(active),
		agentCoreVerificationKeysEnv: "previous-1=" + base64.RawURLEncoding.EncodeToString(previous),
	}
	keys, err := loadAgentCoreVerificationKeys(func(name string) string { return environment[name] })
	if err != nil {
		t.Fatalf("loadAgentCoreVerificationKeys: %v", err)
	}
	if !bytes.Equal(keys["active-2"], active) || !bytes.Equal(keys["previous-1"], previous) || len(keys) != 2 {
		t.Fatalf("keys=%+v", keys)
	}
}

func TestMintAgentCoreTokenCLIOutputsOnlyUsableToken(t *testing.T) {
	now := time.Date(2026, 8, 2, 15, 0, 0, 0, time.UTC)
	key := bytes.Repeat([]byte("m"), 32)
	encodedKey := base64.RawURLEncoding.EncodeToString(key)
	environment := map[string]string{
		agentCoreActiveKeyIDEnv: "active-cli",
		agentCoreSigningKeyEnv:  encodedKey,
	}
	var stdout bytes.Buffer
	err := runMintAgentCoreToken(
		[]string{"-actor", "did:matrix:user-cli:cody"},
		&stdout,
		func(name string) string { return environment[name] },
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("runMintAgentCoreToken: %v", err)
	}
	output := stdout.String()
	token := strings.TrimSpace(output)
	if output != token+"\n" || !strings.HasPrefix(token, "mxg1.") || strings.Contains(output, encodedKey) {
		t.Fatalf("stdout was not token-only: %q", output)
	}
	authenticator, err := auth.New(auth.Options{
		Token:                     "legacy",
		AgentCoreVerificationKeys: map[string][]byte{"active-cli": key},
		Now:                       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
	request.Header.Set(types.HeaderAuthorization, "Bearer "+token)
	request.Header.Set(types.HeaderActorDID, "did:matrix:user-cli:cody")
	request.Header.Set(types.HeaderSlot, types.SlotCody)
	if actor, err := authenticator.Verify(request); err != nil || actor != "did:matrix:user-cli:cody" {
		t.Fatalf("minted token verification actor=%q err=%v", actor, err)
	}
}

func TestMintAgentCoreTokenCLIRejectsOverlongTTLWithoutLeakingKey(t *testing.T) {
	key := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("z"), 32))
	environment := map[string]string{
		agentCoreActiveKeyIDEnv: "active",
		agentCoreSigningKeyEnv:  key,
	}
	err := runMintAgentCoreToken(
		[]string{"-actor", "did:matrix:user:cody", "-ttl", "1h1s"},
		io.Discard,
		func(name string) string { return environment[name] },
		time.Now,
	)
	if err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("overlong TTL error=%v", err)
	}
}

func TestLoadAgentCoreVerificationKeysRejectsPartialAndConflictingConfig(t *testing.T) {
	keyA := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("a"), 32))
	keyB := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("b"), 32))
	tests := []map[string]string{
		{agentCoreActiveKeyIDEnv: "active"},
		{
			agentCoreActiveKeyIDEnv:      "same",
			agentCoreSigningKeyEnv:       keyA,
			agentCoreVerificationKeysEnv: "same=" + keyB,
		},
	}
	for _, environment := range tests {
		_, err := loadAgentCoreVerificationKeys(func(name string) string { return environment[name] })
		if err == nil {
			t.Fatalf("configuration accepted: %+v", environment)
		}
	}
}
