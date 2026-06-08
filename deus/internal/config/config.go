// Package config loads Deus configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config aggregates environment-driven settings for the Deus control plane.
type Config struct {
	Port         int
	PostgresURI  string
	RPCURL       string
	ChainID      int64
	MigrationsDir string

	ObjStoreEndpoint  string
	ObjStoreAccessKey string
	ObjStoreSecretKey string
	ObjStoreBucket    string
	ObjStoreUseSSL    bool

	WalletAPIURL          string
	ServiceRegistryAddr   string
	SettlementAnchorAddr  string
	GatewaySigningKeyRef  string
	SettlerKeyRef         string

	EmbedEndpoint string
	EmbedModel    string

	PublishPrivateKey string
	GatewaySigningKey string

	AppwriteEndpoint  string
	AppwriteProjectID string
	AppwriteAPIKey    string
	HostingDevExecURL string
	HostingKillSwitch bool

	Dev bool
}

// Load reads configuration from the environment.
// When DEUS_DEV=1, optional integrations may be unset for local skeleton boot.
func Load() (*Config, error) {
	dev := envBool("DEUS_DEV", false)
	cfg := &Config{
		Port:          envInt("DEUS_PORT", 9095),
		PostgresURI:   strings.TrimSpace(os.Getenv("DEUS_POSTGRES_URI")),
		RPCURL:        strings.TrimSpace(os.Getenv("PAXEER_RPC_URL")),
		ChainID:       envInt64("DEUS_CHAIN_ID", 125),
		MigrationsDir: envOr("DEUS_MIGRATIONS_DIR", "migrations"),

		ObjStoreEndpoint:  strings.TrimSpace(os.Getenv("DEUS_OBJSTORE_ENDPOINT")),
		ObjStoreAccessKey: strings.TrimSpace(os.Getenv("DEUS_OBJSTORE_ACCESS_KEY")),
		ObjStoreSecretKey: strings.TrimSpace(os.Getenv("DEUS_OBJSTORE_SECRET_KEY")),
		ObjStoreBucket:    strings.TrimSpace(os.Getenv("DEUS_OBJSTORE_BUCKET")),
		ObjStoreUseSSL:    envBool("DEUS_OBJSTORE_USE_SSL", true),

		WalletAPIURL:         strings.TrimSpace(os.Getenv("MATRIX_WALLET_API_URL")),
		ServiceRegistryAddr:  strings.TrimSpace(os.Getenv("DEUS_SERVICE_REGISTRY_ADDR")),
		SettlementAnchorAddr: strings.TrimSpace(os.Getenv("DEUS_SETTLEMENT_ANCHOR_ADDR")),
		GatewaySigningKeyRef: strings.TrimSpace(os.Getenv("DEUS_GATEWAY_SIGNING_KEY_REF")),
		SettlerKeyRef:        strings.TrimSpace(os.Getenv("DEUS_SETTLER_KEY_REF")),

		EmbedEndpoint: strings.TrimSpace(os.Getenv("DEUS_EMBED_ENDPOINT")),
		EmbedModel:    envOr("DEUS_EMBED_MODEL", "nomic-embed-text-v1.5"),

		PublishPrivateKey: strings.TrimSpace(os.Getenv("DEUS_PUBLISH_PRIVATE_KEY")),
		GatewaySigningKey: strings.TrimSpace(os.Getenv("DEUS_GATEWAY_SIGNING_KEY")),

		AppwriteEndpoint:  strings.TrimSpace(os.Getenv("DEUS_APPWRITE_ENDPOINT")),
		AppwriteProjectID: strings.TrimSpace(os.Getenv("DEUS_APPWRITE_PROJECT_ID")),
		AppwriteAPIKey:    strings.TrimSpace(os.Getenv("DEUS_APPWRITE_API_KEY")),
		HostingDevExecURL: strings.TrimSpace(os.Getenv("DEUS_HOSTING_DEV_EXEC_URL")),
		HostingKillSwitch: envBool("DEUS_HOSTING_KILL_SWITCH", false),

		Dev: dev,
	}

	if cfg.PostgresURI == "" {
		return nil, errors.New("config: DEUS_POSTGRES_URI is required")
	}
	if !dev {
		if cfg.RPCURL == "" {
			return nil, errors.New("config: PAXEER_RPC_URL is required")
		}
		if cfg.ObjStoreEndpoint == "" || cfg.ObjStoreAccessKey == "" || cfg.ObjStoreSecretKey == "" || cfg.ObjStoreBucket == "" {
			return nil, errors.New("config: DEUS_OBJSTORE_ENDPOINT, DEUS_OBJSTORE_ACCESS_KEY, DEUS_OBJSTORE_SECRET_KEY, DEUS_OBJSTORE_BUCKET are required")
		}
		if cfg.GatewaySigningKeyRef == "" && cfg.GatewaySigningKey == "" {
			return nil, errors.New("config: DEUS_GATEWAY_SIGNING_KEY_REF or DEUS_GATEWAY_SIGNING_KEY is required")
		}
		if cfg.WalletAPIURL == "" {
			return nil, errors.New("config: MATRIX_WALLET_API_URL is required")
		}
		if cfg.ServiceRegistryAddr == "" {
			return nil, errors.New("config: DEUS_SERVICE_REGISTRY_ADDR is required")
		}
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("config: DEUS_PORT out of range: %d", cfg.Port)
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envInt64(key string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}
