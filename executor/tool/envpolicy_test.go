package tool

import (
	"slices"
	"strings"
	"testing"
)

func TestAgentEnvironmentOwnerClassification(t *testing.T) {
	visible := []string{
		"CODY_USER_ID", "MATRIX_USER_ID", "CODY_PREVIEW_IMAGE",
		"KINDLE_FRONTEND_URL", "KINDLE_MEDIA_GATEWAY", "KINDLE_METADATA_URL", "KINDLE_RPC_URL",
		"MATRIX_BROWSER_URL", "MATRIX_CHRONOS_TOKEN", "MATRIX_CHRONOS_URL",
		"MATRIX_COMPILER_ESCALATE_MODEL", "MATRIX_COMPILER_MODEL", "MATRIX_DATA_DIR",
		"MATRIX_DEFAULT_SKILL", "MATRIX_DEUS_TIMEOUT_MS", "MATRIX_DEUS_URL",
		"MATRIX_EXECUTOR_MODEL", "MATRIX_GATEWAY_TOKEN", "MATRIX_GATEWAY_URL",
		"MATRIX_LAYERX_TOKEN", "MATRIX_LAYERX_URL", "MATRIX_LIAISON_MODEL",
		"MATRIX_PLANNER_MODEL", "MATRIX_SEARXNG_TOKEN", "MATRIX_SEARXNG_URL",
		"MATRIX_SNAPSHOT_INTERVAL", "MATRIX_TACHYON_TOKEN", "MATRIX_TACHYON_URL",
		"MATRIX_UWAC_TOKEN", "MATRIX_UWAC_URL", "PAXC_API", "PAXC_TOKEN",
		"WEBSEARCH_PROVIDER", "NEO_AUTOMATRIX_ENABLED", "NEO_AUTOMATRIX_INTERVAL",
		"NEO_AUTOMATRIX_JITTER", "NEO_AUTOMATRIX_MAX_PER_DAY",
		"NEO_AUTOMATRIX_MIN_CONFIDENCE", "MATRIX_MEDIA_XAI_VIDEO_MODEL",
		"NEO_CONTINUOUS_MEMORY", "VAULT_REQUIRED", "VOICE_IDLE_DISCONNECT_S", "NEO_RUNTIME",
	}
	hidden := []string{
		"BRAVE_API_KEY", "MATRIX_BROWSER_TOKEN",
		"MATRIX_S3_BUCKET", "MATRIX_S3_ENDPOINT", "MATRIX_S3_KEY", "MATRIX_S3_SECRET",
		"NOVITA_API_KEY", "RAILWAY_API_TOKEN", "RAILWAY_ENVIRONMENT_ID",
		"RAILWAY_PROJECT_ID", "ROUTER_INTERNAL_URL", "ROUTER_PREVIEW_TOKEN",
		"TAVILY_API_KEY", "XAI_API_KEY", "MIMO_API_KEY",
		"MATRIX_LIVEKIT_KEY", "MATRIX_LIVEKIT_SECRET", "MATRIX_LIVEKIT_URL",
		"MATRIX_SANDBOX_TOKEN", "MATRIX_SANDBOX_URL",
		"NEO_VOICE_ENABLED", "NEO_VOICE_MODE", "NEO_VOICE_TTS_STYLE",
		"NEO_VOICE_TTS_VOICE", "NEO_VOICE_ASR_DEADLINE_SECONDS",
		"NEO_VOICE_TTS_DEADLINE_SECONDS", "ALPHAVANTAGE_API_KEY", "FMP_API_KEY",
		"ROUTER_FINANCE_TOKEN", "MATRIX_FINANCE_TOKEN", "MATRIX_FINANCE_URL",
		"VAULT_KEK", "VAULT_KEK_FILE",
	}
	for _, name := range visible {
		if !AgentEnvironmentVisible(name) {
			t.Errorf("%s should be visible", name)
		}
		if ProtectedEnvironment(name) {
			t.Errorf("%s classified both visible and protected", name)
		}
	}
	for _, name := range hidden {
		if AgentEnvironmentVisible(name) {
			t.Errorf("%s should not be visible", name)
		}
		if !ProtectedEnvironment(name) {
			t.Errorf("%s should be protected", name)
		}
		if owner, ok := ProtectedEnvironmentOwner(name); !ok || owner == "" {
			t.Errorf("%s has no bounded capability owner", name)
		}
	}
	for _, unknown := range []string{"UNREVIEWED_TOKEN", "AWS_SECRET_ACCESS_KEY", "DATABASE_URL"} {
		if AgentEnvironmentVisible(unknown) {
			t.Errorf("unknown %s was not denied", unknown)
		}
	}
}

func TestProtectedEnvironmentInventoryIsComplete(t *testing.T) {
	for name := range protectedEnvironment {
		if owner, ok := ProtectedEnvironmentOwner(name); !ok || strings.TrimSpace(owner) == "" {
			t.Errorf("protected variable %s has no bounded capability owner", name)
		}
	}
	for name := range protectedEnvironmentOwner {
		if !ProtectedEnvironment(name) {
			t.Errorf("capability inventory contains unprotected variable %s", name)
		}
	}
}

func TestMCPEnvironmentUsesFixedCredentialProfiles(t *testing.T) {
	source := []string{
		"PATH=/bin",
		"MATRIX_USER_ID=user-1",
		"TAVILY_API_KEY=search-sentinel",
		"RAILWAY_API_TOKEN=railway-sentinel",
		"UNREVIEWED_TOKEN=unknown-sentinel",
	}
	env, privileged, err := MCPEnvironment("web-search", source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !privileged || !slices.Contains(env, "TAVILY_API_KEY=search-sentinel") {
		t.Fatalf("web-search profile missing its credential: privileged=%v env=%v", privileged, env)
	}
	joined := strings.Join(env, "\n")
	for _, sentinel := range []string{"railway-sentinel", "unknown-sentinel"} {
		if strings.Contains(joined, sentinel) {
			t.Fatalf("unrelated secret reached web-search profile: %v", env)
		}
	}

	env, privileged, err = MCPEnvironment("exec", source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if privileged || strings.Contains(strings.Join(env, "\n"), "sentinel") {
		t.Fatalf("generic exec server received privileged environment: %v", env)
	}
}

func TestFinanceMCPEnvironmentAcceptsRouterOwnedConfiguration(t *testing.T) {
	source := []string{
		"PATH=/bin",
		"MATRIX_USER_ID=user-1",
		"ROUTER_INTERNAL_URL=http://router.internal:8088",
		"ROUTER_FINANCE_TOKEN=finance-sentinel",
		"FMP_API_KEY=vendor-sentinel",
		"UNREVIEWED_TOKEN=unknown-sentinel",
	}
	env, privileged, err := MCPEnvironment("finance", source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !privileged {
		t.Fatal("finance broker was not kept in the privileged service boundary")
	}
	for _, want := range []string{
		"ROUTER_INTERNAL_URL=http://router.internal:8088",
		"ROUTER_FINANCE_TOKEN=finance-sentinel",
	} {
		if !slices.Contains(env, want) {
			t.Fatalf("finance broker missing %q: %v", want, env)
		}
	}
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"vendor-sentinel", "unknown-sentinel"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("unrelated secret reached finance broker: %v", env)
		}
	}
}

func TestAgentEnvironmentFiltersValuesAndDuplicates(t *testing.T) {
	source := []string{
		"PATH=/bin", "MATRIX_USER_ID=first", "TAVILY_API_KEY=sentinel-search",
		"UNREVIEWED_TOKEN=sentinel-unknown", "MATRIX_USER_ID=final", "BROKEN",
		"VAULT_KEK=sentinel-vault",
	}
	got := AgentEnvironment(source)
	if !slices.Contains(got, "PATH=/bin") || !slices.Contains(got, "MATRIX_USER_ID=final") {
		t.Fatalf("approved environment missing: %v", got)
	}
	joined := strings.Join(got, "\n")
	for _, sentinel := range []string{"sentinel-search", "sentinel-unknown", "sentinel-vault", "first"} {
		if strings.Contains(joined, sentinel) {
			t.Errorf("filtered environment leaked %q: %v", sentinel, got)
		}
	}
}

func TestAgentEnvironmentWithOverridesRejectsProtectedAndUnknownNames(t *testing.T) {
	for _, override := range []string{
		"TAVILY_API_KEY=sentinel-secret",
		"VAULT_KEK=sentinel-vault",
		"UNREVIEWED_TOKEN=sentinel-unknown",
		"BROKEN",
	} {
		got, err := AgentEnvironmentWithOverrides([]string{"PATH=/bin"}, []string{override})
		if err == nil {
			t.Fatalf("override %q unexpectedly accepted: %v", override, got)
		}
		if strings.Contains(err.Error(), "sentinel") {
			t.Fatalf("override value leaked through error: %v", err)
		}
	}

	got, err := AgentEnvironmentWithOverrides(
		[]string{"PATH=/bin", "MATRIX_USER_ID=parent"},
		[]string{"MATRIX_USER_ID=override"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "MATRIX_USER_ID=override") ||
		slices.Contains(got, "MATRIX_USER_ID=parent") {
		t.Fatalf("approved override not applied: %v", got)
	}
}
