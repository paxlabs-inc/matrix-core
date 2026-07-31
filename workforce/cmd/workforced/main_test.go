package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func TestRun_PrintsVersion_WithVersionFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"-version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("version returned %d: %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != version {
		t.Fatalf("version output = %q, want %q", stdout.String(), version)
	}
}

func TestLoadConfigUsesGatewayAndSeparateWakeCredential(t *testing.T) {
	ownerPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, runtimePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"WORKFORCE_POSTGRES_URI":         "postgres://workforce",
		"WORKFORCE_TENANT_ID":            "user-one",
		"WORKFORCE_ORGANIZATION_ID":      "organization-user-one",
		"WORKFORCE_OWNER_ID":             "owner-user-one",
		"WORKFORCE_OWNER_TOKEN":          "owner-token",
		"WORKFORCE_WAKE_TOKEN":           "wake-token",
		"WORKFORCE_OWNER_KEY_ID":         "bootstrap-owner-v1",
		"WORKFORCE_OWNER_PUBLIC_KEY":     base64.RawURLEncoding.EncodeToString(ownerPublic),
		"WORKFORCE_RUNTIME_KEY_ID":       "runtime-v1",
		"WORKFORCE_RUNTIME_PRIVATE_KEY":  base64.RawURLEncoding.EncodeToString(runtimePrivate),
		"WORKFORCE_BUBBLEWRAP":           "/usr/bin/bwrap",
		"WORKFORCE_SEAT_BINARY":          "/opt/matrix/bin/workforce-seat",
		"WORKFORCE_AUDITOR_BINARY":       "/opt/matrix/bin/workforce-auditor",
		"WORKFORCE_DEVELOPER_REPOSITORY": "/workspace",
		"WORKFORCE_CODEGRAPH_EXECUTABLE": "/usr/local/bin/cg",
		"WORKFORCE_AUDITOR_SEAT_ID":      "seat-developer-auditor",
		"WORKFORCE_DATA_DIR":             "/data/workforce",
		"MATRIX_GATEWAY_URL":             "https://gateway.example/gw",
		"MATRIX_GATEWAY_TOKEN":           "gateway-token",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
	t.Setenv("MIMO_API_KEY", "")
	t.Setenv("XIAOMI_API_KEY", "")
	config, err := loadConfig(":8091")
	if err != nil {
		t.Fatal(err)
	}
	if config.wakeToken != "wake-token" ||
		config.model.Endpoint != "https://gateway.example/gw/v1/chat/completions" ||
		config.model.APIKey != "gateway-token" ||
		config.model.ActorDID != "did:matrix:user-one:workforce" {
		t.Fatalf("gateway Workforce config was not preserved")
	}
}

func TestRun_RejectsRuntimeStart_WithoutConfiguredService(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("empty invocation returned %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("usage error missing: %q", stderr.String())
	}
}

func TestRun_RejectsUnknownFlag_WithoutStarting(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"-unknown"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown flag returned %d, want 2", code)
	}
}
