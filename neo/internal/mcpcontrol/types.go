// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcpcontrol

import (
	"errors"
	"time"
)

type State string

const (
	StateCandidate State = "candidate"
	StatePending   State = "pending"
	StateActive    State = "active"
	StateDisabled  State = "disabled"
	StateRollback  State = "rolled_back"
)

type Health string

const (
	HealthUnknown   Health = "unknown"
	HealthHealthy   Health = "healthy"
	HealthUnhealthy Health = "unhealthy"
)

type Circuit string

const (
	CircuitClosed   Circuit = "closed"
	CircuitOpen     Circuit = "open"
	CircuitHalfOpen Circuit = "half_open"
)

var (
	ErrNotFound           = errors.New("MCP server not found")
	ErrConflict           = errors.New("MCP server configuration conflict")
	ErrUnclassified       = errors.New("every MCP tool requires an explicit effect class")
	ErrUnhealthy          = errors.New("MCP server has not passed a live health probe")
	ErrEncryptionRequired = errors.New("encrypted credential storage is required")
	ErrCircuitOpen        = errors.New("MCP server circuit breaker is open")
	ErrOAuthState         = errors.New("OAuth authorization state is invalid or expired")
)

type Config struct {
	Alias         string       `json:"alias"`
	DisplayName   string       `json:"display_name"`
	Transport     string       `json:"transport"`
	Command       string       `json:"command,omitempty"`
	Args          []string     `json:"args,omitempty"`
	EnvKeys       []string     `json:"env_keys,omitempty"`
	Endpoint      string       `json:"endpoint,omitempty"`
	HeaderKeys    []string     `json:"header_keys,omitempty"`
	PackageDigest string       `json:"package_digest"`
	Version       string       `json:"version"`
	OAuth         *OAuthConfig `json:"oauth,omitempty"`
}

type OAuthConfig struct {
	ResourceURL         string   `json:"resource_url"`
	AuthorizationURL    string   `json:"authorization_endpoint,omitempty"`
	TokenURL            string   `json:"token_endpoint,omitempty"`
	RegistrationURL     string   `json:"registration_endpoint,omitempty"`
	ClientID            string   `json:"client_id,omitempty"`
	Scopes              []string `json:"scopes,omitempty"`
	RedirectURL         string   `json:"redirect_url"`
	AuthorizationServer string   `json:"authorization_server,omitempty"`
}

type Tool struct {
	Name        string         `json:"name"`
	Function    string         `json:"function"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
	EffectClass string         `json:"effect_class,omitempty"`
	Enabled     bool           `json:"enabled"`
}

type Server struct {
	Config                Config    `json:"config"`
	Tools                 []Tool    `json:"tools"`
	State                 State     `json:"state"`
	Enabled               bool      `json:"enabled"`
	DesiredGeneration     int64     `json:"desired_generation"`
	AppliedGeneration     int64     `json:"applied_generation,omitempty"`
	LastHealthyGeneration int64     `json:"last_healthy_generation,omitempty"`
	Health                Health    `json:"health"`
	LatencyMS             int64     `json:"latency_ms,omitempty"`
	Circuit               Circuit   `json:"circuit"`
	CircuitUntil          time.Time `json:"circuit_until,omitempty"`
	FailureCount          int       `json:"failure_count"`
	LastError             string    `json:"last_error,omitempty"`
	OAuthConnected        bool      `json:"oauth_connected"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type CreateRequest struct {
	Config        Config            `json:"config"`
	SecretEnv     map[string]string `json:"secret_env,omitempty"`
	SecretHeaders map[string]string `json:"secret_headers,omitempty"`
	ClientSecret  string            `json:"client_secret,omitempty"`
}

type Classification struct {
	Name        string `json:"name"`
	EffectClass string `json:"effect_class"`
	Enabled     bool   `json:"enabled"`
}

type AuditEvent struct {
	ID         int64     `json:"id"`
	Alias      string    `json:"alias,omitempty"`
	Generation int64     `json:"generation,omitempty"`
	Action     string    `json:"action"`
	Detail     string    `json:"detail,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type OAuthStart struct {
	AuthorizationURL string    `json:"authorization_url"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type secretState struct {
	Servers map[string]serverSecrets `json:"servers"`
}

type serverSecrets struct {
	Env          map[string]string       `json:"env,omitempty"`
	Headers      map[string]string       `json:"headers,omitempty"`
	ClientSecret string                  `json:"client_secret,omitempty"`
	OAuth        oauthTokens             `json:"oauth,omitempty"`
	Pending      map[string]oauthPending `json:"pending,omitempty"`
}

type oauthTokens struct {
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	Scope        string    `json:"scope,omitempty"`
}

type oauthPending struct {
	Verifier  string    `json:"verifier"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
