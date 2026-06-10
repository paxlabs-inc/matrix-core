package hosting

import (
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Limits configures free-hosting budget and resource caps (docs/06-execution-hosting.md §6.7).
type Limits struct {
	BudgetPAXWei            string
	MaxAlwaysWarm           int
	KillSwitch              bool
	MaxArtifactBytes        int64
	DefaultTimeoutMS        int
	DefaultMaxResponseBytes int
}

// defaultLimits returns the built-in fallback caps used when neither a YAML
// file nor environment variables provide a value.
func defaultLimits() Limits {
	return Limits{
		BudgetPAXWei:            "1000000000000000000",
		MaxAlwaysWarm:           10,
		KillSwitch:              false,
		MaxArtifactBytes:        10 * 1024 * 1024,
		DefaultTimeoutMS:        30000,
		DefaultMaxResponseBytes: 262144,
	}
}

// LimitsFromEnv loads hosting limits from defaults overlaid with environment
// variables. Kept for callers/tests that only want the env view.
func LimitsFromEnv() Limits {
	return defaultLimits().overlayEnv()
}

// LoadLimits resolves limits with precedence: defaults < YAML file < env vars.
// The YAML file is loaded only when DEUS_HOSTING_LIMITS_FILE points at a
// readable configs/limits.<env>.yaml; env vars always win when set so an
// operator can override a deployed config without editing files.
func LoadLimits() Limits {
	base := defaultLimits()
	if path := strings.TrimSpace(os.Getenv("DEUS_HOSTING_LIMITS_FILE")); path != "" {
		if y, err := readLimitsYAML(path); err == nil {
			base = base.overlayYAML(y)
		}
	}
	return base.overlayEnv()
}

// LimitsFromYAML parses a configs/limits.<env>.yaml file over the defaults.
func LimitsFromYAML(path string) (Limits, error) {
	y, err := readLimitsYAML(path)
	if err != nil {
		return Limits{}, err
	}
	return defaultLimits().overlayYAML(y), nil
}

// limitsYAML mirrors configs/limits.<env>.yaml. Pointer fields distinguish an
// absent key from an explicit zero so partial files only override what they set.
type limitsYAML struct {
	BudgetPAXWei     *string `yaml:"budget_pax_wei"`
	MaxAlwaysWarm    *int    `yaml:"max_always_warm"`
	KillSwitch       *bool   `yaml:"kill_switch"`
	MaxArtifactBytes *int64  `yaml:"max_artifact_bytes"`
	DefaultTimeoutMS *int    `yaml:"default_timeout_ms"`
	MaxResponseBytes *int    `yaml:"max_response_bytes"`
}

func readLimitsYAML(path string) (limitsYAML, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return limitsYAML{}, err
	}
	var y limitsYAML
	if err := yaml.Unmarshal(raw, &y); err != nil {
		return limitsYAML{}, err
	}
	return y, nil
}

func (l Limits) overlayYAML(y limitsYAML) Limits {
	if y.BudgetPAXWei != nil && strings.TrimSpace(*y.BudgetPAXWei) != "" {
		l.BudgetPAXWei = *y.BudgetPAXWei
	}
	if y.MaxAlwaysWarm != nil {
		l.MaxAlwaysWarm = *y.MaxAlwaysWarm
	}
	if y.KillSwitch != nil {
		l.KillSwitch = *y.KillSwitch
	}
	if y.MaxArtifactBytes != nil {
		l.MaxArtifactBytes = *y.MaxArtifactBytes
	}
	if y.DefaultTimeoutMS != nil {
		l.DefaultTimeoutMS = *y.DefaultTimeoutMS
	}
	if y.MaxResponseBytes != nil {
		l.DefaultMaxResponseBytes = *y.MaxResponseBytes
	}
	return l
}

func (l Limits) overlayEnv() Limits {
	if v, ok := lookupNonEmpty("DEUS_HOSTING_BUDGET_PAX_WEI"); ok {
		l.BudgetPAXWei = v
	}
	if v, ok := lookupInt("DEUS_HOSTING_MAX_ALWAYS_WARM"); ok {
		l.MaxAlwaysWarm = v
	}
	if v, ok := lookupBool("DEUS_HOSTING_KILL_SWITCH"); ok {
		l.KillSwitch = v
	}
	if v, ok := lookupInt("DEUS_HOSTING_MAX_ARTIFACT_BYTES"); ok {
		l.MaxArtifactBytes = int64(v)
	}
	if v, ok := lookupInt("DEUS_HOSTING_DEFAULT_TIMEOUT_MS"); ok {
		l.DefaultTimeoutMS = v
	}
	if v, ok := lookupInt("DEUS_HOSTING_MAX_RESPONSE_BYTES"); ok {
		l.DefaultMaxResponseBytes = v
	}
	return l
}

func lookupNonEmpty(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, true
}

func lookupInt(key string) (int, bool) {
	v, ok := lookupNonEmpty(key)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

func lookupBool(key string) (bool, bool) {
	v, ok := lookupNonEmpty(key)
	if !ok {
		return false, false
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}
