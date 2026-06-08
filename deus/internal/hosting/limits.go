package hosting

import (
	"os"
	"strconv"
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

// LimitsFromEnv loads hosting limits from environment variables.
func LimitsFromEnv() Limits {
	return Limits{
		BudgetPAXWei:            envOr("DEUS_HOSTING_BUDGET_PAX_WEI", "1000000000000000000"),
		MaxAlwaysWarm:           envInt("DEUS_HOSTING_MAX_ALWAYS_WARM", 10),
		KillSwitch:              envBool("DEUS_HOSTING_KILL_SWITCH"),
		MaxArtifactBytes:        int64(envInt("DEUS_HOSTING_MAX_ARTIFACT_BYTES", 10*1024*1024)),
		DefaultTimeoutMS:        envInt("DEUS_HOSTING_DEFAULT_TIMEOUT_MS", 30000),
		DefaultMaxResponseBytes: envInt("DEUS_HOSTING_MAX_RESPONSE_BYTES", 262144),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envBool(key string) bool {
	switch os.Getenv(key) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
