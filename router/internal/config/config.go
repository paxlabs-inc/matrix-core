// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package config loads matrix-router configuration from environment.
//
// Production layout: systemd unit at /etc/systemd/system/matrix-router.service
// loads /etc/matrix/router.env + /etc/matrix/postgres.env (see deploy/box/
// router/router.service); both files are mode 0640 owned by root:matrix and
// readable only by the service user.
//
// Required envs at minimum:
//
//	ROUTER_ADDR             public listen addr (e.g. :443)
//	ROUTER_INTERNAL_ADDR    private listen addr for admin (e.g. :8088)
//	SUPABASE_URL            project URL; JWKS at /auth/v1/.well-known/jwks.json
//	FLY_API_TOKEN           bearer token for api.machines.dev
//	FLY_APP_NAME            e.g. matrix-daemon
//	FLY_REGION              default region for new Machines (e.g. fra)
//	DATABASE_URL            postgres://matrix:...@127.0.0.1:5432/matrix
//
// Optional but recommended during the Supabase JWT migration:
//
//	SUPABASE_JWT_SECRET     legacy HS256 shared secret. Required only
//	                        as long as in-flight HS256 tokens issued
//	                        before the asymmetric rollout are still
//	                        verifiable. Drop once Supabase confirms
//	                        all live tokens are asymmetric.
//
// Optional:
//
//	ROUTER_ADMIN_TOKEN      bearer token for /admin/* (empty disables admin)
//	ROUTER_DAEMON_PORT      default 8080 (port on each Machine the daemon listens on)
//	ROUTER_WAKE_TIMEOUT     default 30s
//	ROUTER_PROXY_TIMEOUT    default 5m
//	ROUTER_PROBE_INTERVAL   default 250ms (poll cadence while waiting for Machine boot)
//	S3_ENDPOINT             for echoing back to provisioning admin response
//	S3_BUCKET               (informational; same as MinIO bucket bootstrapped)
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config aggregates every env-driven setting matrix-router needs.
//
// Empty optional fields fall back to package-level defaults; required
// fields are validated by Load and surface as errors with the offending
// env-var name so operators see the missing key directly.
type Config struct {
	PublicAddr   string
	InternalAddr string
	AdminToken   string

	// SupabaseURL is the project URL (e.g. https://xxx.supabase.co).
	// JWKS endpoint = SupabaseURL + "/auth/v1/.well-known/jwks.json".
	// Required: the asymmetric JWT path is now the canonical one.
	SupabaseURL string

	// SupabaseLegacyJWTSecret is the project's "JWT secret" used to
	// verify HS256-signed tokens issued before the asymmetric
	// rollout. Optional once Supabase finishes the migration; left
	// non-empty during the overlap window.
	SupabaseLegacyJWTSecret string

	FlyAPIToken string
	FlyApp      string
	FlyRegion   string

	DatabaseURL string

	S3Endpoint string
	S3Bucket   string

	DaemonPort         string
	WakeTimeout        time.Duration
	ProxyTimeout       time.Duration
	ProbeInterval      time.Duration
	DaemonReadyTimeout time.Duration
}

// Defaults — applied when an env value is unset or empty.
const (
	DefaultDaemonPort         = "8080"
	DefaultWakeTimeout        = 30 * time.Second
	DefaultProxyTimeout       = 5 * time.Minute
	DefaultProbeInterval      = 250 * time.Millisecond
	DefaultDaemonReadyTimeout = 30 * time.Second
)

// Load reads from os.Getenv. Returns aggregate error listing every
// missing required field. ParseDuration failures are also surfaced.
func Load() (*Config, error) {
	c := &Config{
		PublicAddr:              os.Getenv("ROUTER_ADDR"),
		InternalAddr:            os.Getenv("ROUTER_INTERNAL_ADDR"),
		AdminToken:              os.Getenv("ROUTER_ADMIN_TOKEN"),
		SupabaseURL:             os.Getenv("SUPABASE_URL"),
		SupabaseLegacyJWTSecret: os.Getenv("SUPABASE_JWT_SECRET"),
		FlyAPIToken:             os.Getenv("FLY_API_TOKEN"),
		FlyApp:                  os.Getenv("FLY_APP_NAME"),
		FlyRegion:               os.Getenv("FLY_REGION"),
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		S3Endpoint:              os.Getenv("S3_ENDPOINT"),
		S3Bucket:                os.Getenv("S3_BUCKET"),
		DaemonPort:              getOrDefault("ROUTER_DAEMON_PORT", DefaultDaemonPort),
	}

	var err error
	if c.WakeTimeout, err = parseDurationOr("ROUTER_WAKE_TIMEOUT", DefaultWakeTimeout); err != nil {
		return nil, err
	}
	if c.ProxyTimeout, err = parseDurationOr("ROUTER_PROXY_TIMEOUT", DefaultProxyTimeout); err != nil {
		return nil, err
	}
	if c.ProbeInterval, err = parseDurationOr("ROUTER_PROBE_INTERVAL", DefaultProbeInterval); err != nil {
		return nil, err
	}
	if c.DaemonReadyTimeout, err = parseDurationOr("ROUTER_DAEMON_READY_TIMEOUT", DefaultDaemonReadyTimeout); err != nil {
		return nil, err
	}

	var missing []string
	if c.PublicAddr == "" {
		missing = append(missing, "ROUTER_ADDR")
	}
	if c.InternalAddr == "" {
		missing = append(missing, "ROUTER_INTERNAL_ADDR")
	}
	if c.SupabaseURL == "" {
		missing = append(missing, "SUPABASE_URL")
	}
	if c.FlyAPIToken == "" {
		missing = append(missing, "FLY_API_TOKEN")
	}
	if c.FlyApp == "" {
		missing = append(missing, "FLY_APP_NAME")
	}
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("config: missing required env vars: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func getOrDefault(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

func parseDurationOr(env string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(env)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", env, err)
	}
	return d, nil
}

// AdminEnabled reports whether the admin endpoints should be mounted.
// We require a non-empty token to mount admin routes at all so a
// misconfigured deploy can't accidentally expose user-creation.
func (c *Config) AdminEnabled() bool {
	return c.AdminToken != ""
}

// ErrAdminDisabled is returned by handlers when admin endpoints are
// hit but no ROUTER_ADMIN_TOKEN is configured.
var ErrAdminDisabled = errors.New("admin: ROUTER_ADMIN_TOKEN not configured")

// Copyright © 2026 Paxlabs Inc. All rights reserved.
