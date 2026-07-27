package tool

import (
	"fmt"
	"sort"
	"strings"
)

var agentVisibleEnvironment = stringSet(
	"HOME", "USER", "LOGNAME", "SHELL", "PATH", "PWD", "OLDPWD",
	"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "TERM", "COLORTERM", "TZ", "TMPDIR",
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
	"MATRIX_EXEC_STATE_DIR", "MATRIX_EXEC_WORKDIR", "MATRIX_EXEC_TIMEOUT_MS",
	"MATRIX_EXEC_MAX_OUTPUT_BYTES", "MATRIX_EXEC_MAX_SERVICES", "MATRIX_EXEC_MAX_LOG_LINES",
)

var protectedEnvironment = stringSet(
	"BRAVE_API_KEY", "MATRIX_BROWSER_TOKEN",
	"MATRIX_S3_BUCKET", "MATRIX_S3_ENDPOINT", "MATRIX_S3_KEY", "MATRIX_S3_SECRET",
	"NOVITA_API_KEY", "RAILWAY_API_TOKEN", "RAILWAY_ENVIRONMENT_ID",
	"RAILWAY_PROJECT_ID", "ROUTER_INTERNAL_URL", "ROUTER_PREVIEW_TOKEN",
	"TAVILY_API_KEY", "XAI_API_KEY", "MIMO_API_KEY", "XIAOMI_API_KEY",
	"MATRIX_LIVEKIT_KEY", "MATRIX_LIVEKIT_SECRET", "MATRIX_LIVEKIT_URL",
	"MATRIX_SANDBOX_TOKEN", "MATRIX_SANDBOX_URL",
	"NEO_VOICE_ENABLED", "NEO_VOICE_MODE", "NEO_VOICE_TTS_STYLE",
	"NEO_VOICE_TTS_VOICE", "NEO_VOICE_ASR_DEADLINE_SECONDS",
	"NEO_VOICE_TTS_DEADLINE_SECONDS", "ALPHAVANTAGE_API_KEY", "FMP_API_KEY",
	"ROUTER_FINANCE_TOKEN",
	// The root data-encryption key cannot be agent-visible without defeating
	// the vault, even though it was initially listed as ordinary config.
	"VAULT_KEK", "VAULT_KEK_FILE", "MATRIX_VAULT_KEK", "MATRIX_VAULT_KEK_ID",
)

var protectedEnvironmentOwner = map[string]string{
	"BRAVE_API_KEY":                  "web-search",
	"TAVILY_API_KEY":                 "web-search",
	"MATRIX_BROWSER_TOKEN":           "browser",
	"MATRIX_S3_BUCKET":               "snapshot",
	"MATRIX_S3_ENDPOINT":             "snapshot",
	"MATRIX_S3_KEY":                  "snapshot",
	"MATRIX_S3_SECRET":               "snapshot",
	"NOVITA_API_KEY":                 "media-model",
	"XAI_API_KEY":                    "media-model",
	"MIMO_API_KEY":                   "media-model",
	"XIAOMI_API_KEY":                 "media-model",
	"RAILWAY_API_TOKEN":              "railway-control",
	"RAILWAY_ENVIRONMENT_ID":         "railway-control",
	"RAILWAY_PROJECT_ID":             "railway-control",
	"ROUTER_INTERNAL_URL":            "preview",
	"ROUTER_PREVIEW_TOKEN":           "preview",
	"MATRIX_LIVEKIT_KEY":             "voice",
	"MATRIX_LIVEKIT_SECRET":          "voice",
	"MATRIX_LIVEKIT_URL":             "voice",
	"NEO_VOICE_ENABLED":              "voice",
	"NEO_VOICE_MODE":                 "voice",
	"NEO_VOICE_TTS_STYLE":            "voice",
	"NEO_VOICE_TTS_VOICE":            "voice",
	"NEO_VOICE_ASR_DEADLINE_SECONDS": "voice",
	"NEO_VOICE_TTS_DEADLINE_SECONDS": "voice",
	"MATRIX_SANDBOX_TOKEN":           "sandbox",
	"MATRIX_SANDBOX_URL":             "sandbox",
	"ALPHAVANTAGE_API_KEY":           "finance",
	"FMP_API_KEY":                    "finance",
	"ROUTER_FINANCE_TOKEN":           "finance",
	"VAULT_KEK":                      "vault-bootstrap",
	"VAULT_KEK_FILE":                 "vault-bootstrap",
	"MATRIX_VAULT_KEK":               "vault-bootstrap",
	"MATRIX_VAULT_KEK_ID":            "vault-bootstrap",
}

var credentialedMCPEnvironment = map[string][]string{
	"web-search": {
		"TAVILY_API_KEY", "BRAVE_API_KEY",
	},
	"browser": {
		"MATRIX_BROWSER_TOKEN", "MATRIX_BROWSER_TIMEOUT_MS",
	},
	"media": {
		"NOVITA_API_KEY", "XAI_API_KEY", "MIMO_API_KEY", "XIAOMI_API_KEY",
		"MATRIX_MEDIA_API_BASE", "MATRIX_MEDIA_XAI_BASE", "MATRIX_MEDIA_XAI_IMAGE_MODEL",
		"MATRIX_MEDIA_XAI_RESOLUTION", "MATRIX_MEDIA_XAI_VIDEO_RES",
		"MATRIX_MEDIA_MIMO_BASE", "MATRIX_MEDIA_MIMO_ASR_MODEL", "MATRIX_MEDIA_MIMO_TTS_MODEL",
		"MATRIX_MEDIA_MIMO_TTS_VOICE", "MATRIX_MEDIA_IMAGE_MODEL", "MATRIX_MEDIA_INPAINT_MODEL",
		"MATRIX_MEDIA_VIDEO_MODEL", "MATRIX_MEDIA_IMG2VIDEO_MODEL", "MATRIX_MEDIA_TTS_VOICE",
		"MATRIX_MEDIA_TTS_LANGUAGE", "MATRIX_MEDIA_DIR", "MATRIX_MEDIA_BASE",
		"MATRIX_MEDIA_TIMEOUT_MS", "MATRIX_MEDIA_TASK_POLL_MS", "MATRIX_MEDIA_TASK_MAX_WAIT_MS",
	},
	"finance": {
		"MATRIX_FINANCE_URL", "MATRIX_FINANCE_TOKEN", "MATRIX_FINANCE_TIMEOUT_MS",
		"MATRIX_FINANCE_MAX_RESPONSE_BYTES",
	},
	"sandbox": {
		"MATRIX_SANDBOX_URL", "MATRIX_SANDBOX_TOKEN", "MATRIX_SANDBOX_TIMEOUT_MS",
		"NEO_WORKSPACE_DIR", "MATRIX_WORKSPACE_ROOT",
	},
	"desktop": {
		"MATRIX_DOJO_URL", "MATRIX_DOJO_TOKEN", "MATRIX_DOJO_TIMEOUT_MS",
	},
	"machine-mail": {
		"MACHINEMAIL_API_URL", "MACHINEMAIL_API_KEY", "MACHINEMAIL_TIMEOUT_MS",
	},
}

// AgentEnvironment returns the exact subset of source approved for an
// agent/user child process. Unknown names are denied. Duplicate names use the
// final value, matching process-environment lookup behavior.
func AgentEnvironment(source []string) []string {
	values := make(map[string]string)
	for _, entry := range source {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !AgentEnvironmentVisible(name) {
			continue
		}
		values[name] = entry
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, values[name])
	}
	return out
}

// AgentEnvironmentWithOverrides builds the safe base and applies explicitly
// requested overrides. Protected and unknown names are rejected, and error
// messages never include values.
func AgentEnvironmentWithOverrides(source, overrides []string) ([]string, error) {
	values := make(map[string]string)
	for _, entry := range AgentEnvironment(source) {
		name, _, _ := strings.Cut(entry, "=")
		values[name] = entry
	}
	for _, entry := range overrides {
		name, _, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid environment override")
		}
		if !AgentEnvironmentVisible(name) {
			return nil, fmt.Errorf("environment override %q is not permitted", name)
		}
		values[name] = entry
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, values[name])
	}
	return out, nil
}

// MCPEnvironment returns the environment for one fixed MCP server. Generic
// servers get only the agent-visible base. Credentialed aliases additionally
// receive their exact capability profile and must run under the privileged
// service identity; no caller can request an arbitrary secret name.
func MCPEnvironment(alias string, source, overrides []string) ([]string, bool, error) {
	env, err := AgentEnvironmentWithOverrides(source, overrides)
	if err != nil {
		return nil, false, err
	}
	names, privileged := credentialedMCPEnvironment[strings.TrimSpace(alias)]
	if !privileged {
		return env, false, nil
	}
	sourceValues := make(map[string]string)
	for _, entry := range source {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			sourceValues[name] = entry
		}
	}
	for _, name := range names {
		if entry, ok := sourceValues[name]; ok {
			env = append(env, entry)
		}
	}
	sort.Strings(env)
	return env, true, nil
}

func AgentEnvironmentVisible(name string) bool {
	_, ok := agentVisibleEnvironment[strings.TrimSpace(name)]
	return ok
}

func ProtectedEnvironment(name string) bool {
	_, ok := protectedEnvironment[strings.TrimSpace(name)]
	return ok
}

// ProtectedEnvironmentOwner returns the bounded capability that must replace
// ambient access to a protected value. It is deliberately not a generic secret
// lookup: callers receive only an ownership label, never the value.
func ProtectedEnvironmentOwner(name string) (string, bool) {
	owner, ok := protectedEnvironmentOwner[strings.TrimSpace(name)]
	return owner, ok
}

func stringSet(values ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}
