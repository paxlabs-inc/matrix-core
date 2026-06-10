package hosting_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/paxlabs-inc/deus/internal/hosting"
)

func TestLimitsFromYAMLDevFile(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "limits.dev.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("dev limits file missing: %v", err)
	}
	lim, err := hosting.LimitsFromYAML(path)
	if err != nil {
		t.Fatalf("LimitsFromYAML: %v", err)
	}
	if lim.MaxAlwaysWarm != 10 {
		t.Errorf("MaxAlwaysWarm = %d, want 10", lim.MaxAlwaysWarm)
	}
	if lim.DefaultTimeoutMS != 30000 {
		t.Errorf("DefaultTimeoutMS = %d, want 30000", lim.DefaultTimeoutMS)
	}
	if lim.DefaultMaxResponseBytes != 262144 {
		t.Errorf("DefaultMaxResponseBytes = %d, want 262144", lim.DefaultMaxResponseBytes)
	}
	if lim.MaxArtifactBytes != 10485760 {
		t.Errorf("MaxArtifactBytes = %d, want 10485760", lim.MaxArtifactBytes)
	}
	if lim.KillSwitch {
		t.Error("KillSwitch = true, want false")
	}
}

func TestLoadLimitsEnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "limits.test.yaml")
	if err := os.WriteFile(yamlPath, []byte("max_always_warm: 7\ndefault_timeout_ms: 12000\nkill_switch: true\n"), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("DEUS_HOSTING_LIMITS_FILE", yamlPath)
	// Env wins over the YAML value.
	t.Setenv("DEUS_HOSTING_MAX_ALWAYS_WARM", "3")

	lim := hosting.LoadLimits()
	if lim.MaxAlwaysWarm != 3 {
		t.Errorf("MaxAlwaysWarm = %d, want 3 (env precedence)", lim.MaxAlwaysWarm)
	}
	if lim.DefaultTimeoutMS != 12000 {
		t.Errorf("DefaultTimeoutMS = %d, want 12000 (from yaml)", lim.DefaultTimeoutMS)
	}
	if !lim.KillSwitch {
		t.Error("KillSwitch = false, want true (from yaml)")
	}
}

func TestLoadLimitsDefaultsWhenUnset(t *testing.T) {
	t.Setenv("DEUS_HOSTING_LIMITS_FILE", "")
	for _, k := range []string{
		"DEUS_HOSTING_MAX_ALWAYS_WARM", "DEUS_HOSTING_DEFAULT_TIMEOUT_MS",
		"DEUS_HOSTING_KILL_SWITCH", "DEUS_HOSTING_MAX_RESPONSE_BYTES",
		"DEUS_HOSTING_MAX_ARTIFACT_BYTES", "DEUS_HOSTING_BUDGET_PAX_WEI",
	} {
		t.Setenv(k, "")
	}
	lim := hosting.LoadLimits()
	if lim.MaxAlwaysWarm != 10 || lim.DefaultTimeoutMS != 30000 || lim.DefaultMaxResponseBytes != 262144 {
		t.Errorf("defaults not applied: %+v", lim)
	}
}
