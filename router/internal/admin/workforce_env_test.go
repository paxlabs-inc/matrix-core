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
		MachineEnv: map[string]string{
			"MATRIX_GATEWAY_URL":            "https://gateway.example",
			"ROUTER_WORKFORCE_ENABLED":      "true",
			"ROUTER_WORKFORCE_PORT":         "8091",
			"ROUTER_WORKFORCE_POSTGRES_URI": "postgres://router-only",
			"ROUTER_WORKFORCE_ROOT_SECRET":  "must-not-reach-user-runtime",
			"ROUTER_WORKFORCE_WAKE_TOKEN":   "must-not-reach-user-runtime",
		},
		Workforce: deriver, WorkforcePostgresURI: "postgres://workforce",
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
	for _, name := range []string{
		"ROUTER_WORKFORCE_ENABLED", "ROUTER_WORKFORCE_PORT",
		"ROUTER_WORKFORCE_POSTGRES_URI", "ROUTER_WORKFORCE_ROOT_SECRET",
		"ROUTER_WORKFORCE_WAKE_TOKEN",
	} {
		if _, exists := envA[name]; exists {
			t.Fatalf("router-only variable %s leaked into machine environment", name)
		}
	}
	privateKey, err := base64.RawURLEncoding.DecodeString(envA["WORKFORCE_RUNTIME_PRIVATE_KEY"])
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		t.Fatalf("runtime key invalid: bytes=%d err=%v", len(privateKey), err)
	}
	if envA["MATRIX_GATEWAY_URL"] != "https://gateway.example" {
		t.Fatal("baseline machine environment was not preserved")
	}
	reconciled, err := handler.workforceReconcileEnvironment("user-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range routerOnlyWorkforceVariables {
		value, exists := reconciled[name]
		if !exists || value != "" {
			t.Fatalf("legacy Router variable %s was not neutralized", name)
		}
	}
	if reconciled["WORKFORCE_ENABLED"] != "true" || reconciled["WORKFORCE_OWNER_TOKEN"] == "" {
		t.Fatal("derived per-user Workforce authority was not preserved")
	}
}

func TestWorkforceReconcileConcurrency(t *testing.T) {
	tests := []struct {
		requested int
		want      int
		wantError bool
	}{
		{requested: 0, want: defaultWorkforceReconcileConcurrency},
		{requested: 1, want: 1},
		{requested: maxWorkforceReconcileConcurrency, want: maxWorkforceReconcileConcurrency},
		{requested: -1, wantError: true},
		{requested: maxWorkforceReconcileConcurrency + 1, wantError: true},
	}
	for _, test := range tests {
		got, err := workforceReconcileConcurrency(test.requested)
		if test.wantError {
			if err == nil {
				t.Fatalf("requested=%d: expected error", test.requested)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Fatalf("requested=%d: got=%d err=%v want=%d", test.requested, got, err, test.want)
		}
	}
}
