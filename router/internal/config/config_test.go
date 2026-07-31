package config

import "testing"

func TestLoadRequiresCompleteWorkforceProductionBoundary(t *testing.T) {
	setRequiredRouterEnvironment(t)
	t.Setenv("ROUTER_WORKFORCE_ENABLED", "true")
	if _, err := Load(); err == nil {
		t.Fatal("incomplete Workforce configuration was accepted")
	}

	t.Setenv("ROUTER_WORKFORCE_ROOT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("ROUTER_WORKFORCE_POSTGRES_URI", "postgres://workforce")
	t.Setenv("ROUTER_WORKFORCE_WAKE_TOKEN", "chronos-workforce-token")
	t.Setenv("MATRIX_GATEWAY_URL", "https://gateway.example")
	t.Setenv("MATRIX_GATEWAY_TOKEN", "gateway-token")
	t.Setenv("MATRIX_VAULT_KEK", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !config.WorkforceEnabled || config.WorkforcePort != DefaultWorkforcePort {
		t.Fatalf("workforce config = %#v", config)
	}
}

func setRequiredRouterEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("ROUTER_ADDR", ":8080")
	t.Setenv("ROUTER_INTERNAL_ADDR", ":8088")
	t.Setenv("SUPABASE_URL", "https://supabase.example")
	t.Setenv("DATABASE_URL", "postgres://router")
	t.Setenv("ROUTER_PROVIDER", ProviderRailway)
	t.Setenv("RAILWAY_API_TOKEN", "railway-token")
	t.Setenv("RAILWAY_PROJECT_ID", "project")
	t.Setenv("RAILWAY_ENVIRONMENT_ID", "environment")
}
