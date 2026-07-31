package admin

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	"matrix/router/internal/workforceauth"
)

func TestInstanceEnvAddsPerUserWorkforceCredentialsWithoutRootSecret(t *testing.T) {
	deriver, err := workforceauth.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{
		MachineEnv: map[string]string{"MATRIX_GATEWAY_URL": "https://gateway.example"},
		Workforce:  deriver, WorkforcePostgresURI: "postgres://workforce",
	}
	envA, err := handler.instanceEnv("user-a")
	if err != nil {
		t.Fatal(err)
	}
	envB, err := handler.instanceEnv("user-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"WORKFORCE_POSTGRES_URI", "WORKFORCE_TENANT_ID",
		"WORKFORCE_ORGANIZATION_ID", "WORKFORCE_OWNER_ID",
		"WORKFORCE_OWNER_TOKEN", "WORKFORCE_WAKE_TOKEN",
		"WORKFORCE_OWNER_KEY_ID", "WORKFORCE_OWNER_PUBLIC_KEY",
		"WORKFORCE_RUNTIME_KEY_ID", "WORKFORCE_RUNTIME_PRIVATE_KEY",
		"WORKFORCE_AUDITOR_SEAT_ID",
	} {
		if envA[name] == "" {
			t.Fatalf("%s missing", name)
		}
	}
	if envA["WORKFORCE_OWNER_TOKEN"] == envB["WORKFORCE_OWNER_TOKEN"] ||
		envA["WORKFORCE_RUNTIME_PRIVATE_KEY"] == envB["WORKFORCE_RUNTIME_PRIVATE_KEY"] {
		t.Fatal("per-user credentials were reused")
	}
	if _, exists := envA["ROUTER_WORKFORCE_ROOT_SECRET"]; exists {
		t.Fatal("router root secret leaked into machine environment")
	}
	privateKey, err := base64.RawURLEncoding.DecodeString(envA["WORKFORCE_RUNTIME_PRIVATE_KEY"])
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		t.Fatalf("runtime key invalid: bytes=%d err=%v", len(privateKey), err)
	}
	if envA["MATRIX_GATEWAY_URL"] != "https://gateway.example" {
		t.Fatal("baseline machine environment was not preserved")
	}
}
