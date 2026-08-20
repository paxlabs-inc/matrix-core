// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package dojo owns the disposable-desktop session lifecycle (DOJO req 1): an
// ephemeral Railway sandbox provisioned on demand, booting the digest-pinned
// bytebot-desktop appliance inside it, reattachable by id while alive, and
// destroyed on idle timeout, hard lifetime, task completion, or explicit
// discard. No standing per-user desktop service ever exists.
//
// This file is the Railway wire client, written against the LIVE sandbox
// GraphQL schema (introspected + round-tripped against the real API
// 2026-07-24 — sandboxCreate/sandbox/sandboxExec/sandboxDestroy). It follows
// the transport discipline of router/internal/railway and
// agents/neo/internal/sandbox: stdlib net/http + encoding/json against
// backboard.railway.com, bearer RAILWAY_API_TOKEN, 200-with-errors-array
// handling, sentinel-error mapping. It is deliberately separate from
// agents/neo/internal/sandbox (the preview lane's client): that schema predates the
// live API and carries operations (privateHost, sandboxFilesWrite) the live
// API does not have.
//
// Live-schema notes the Manager depends on:
//   - SandboxCreateInput has NO image field; custom base-image template builds
//     currently FAIL on the experimental API, so the appliance image is booted
//     INSIDE the sandbox via docker (sandboxes run dockerd as root — verified
//     live). The pin therefore never leaves our control.
//   - sandboxExec takes one shell-command string plus a timeoutSec bound.
//   - sandboxDestroy is naturally idempotent (repeat destroy answers
//     DESTROYING, no error).
//   - networkIsolation ISOLATED = public egress only (needed for the ghcr
//     pull and for the desktop to browse as the user); PRIVATE = environment
//     services, no public egress.
package dojo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultEndpoint is the Railway public GraphQL API base.
const DefaultEndpoint = "https://backboard.railway.com/graphql/v2"

// defaultWireTimeout bounds a single GraphQL round-trip when no HTTPClient is
// supplied. Exec calls carry their own server-side timeoutSec on top.
const defaultWireTimeout = 120 * time.Second

// Sandbox statuses the live API reports (SandboxStatus enum, introspected).
const (
	SandboxCreating   = "CREATING"
	SandboxRunning    = "RUNNING"
	SandboxDestroying = "DESTROYING"
	SandboxDestroyed  = "DESTROYED"
	SandboxFailed     = "FAILED"
)

// Network isolation modes (SandboxNetworkIsolation enum, introspected).
const (
	NetworkIsolated = "ISOLATED"
	NetworkPrivate  = "PRIVATE"
)

// Wire sentinel errors. Callers dispatch with errors.Is.
var (
	ErrWireNotFound     = errors.New("dojo: sandbox not found")
	ErrWireUnauthorized = errors.New("dojo: unauthorized (check RAILWAY_API_TOKEN)")
	ErrWireUpstream     = errors.New("dojo: railway upstream error")
)

// SandboxInfo is the subset of the live Sandbox object the Manager uses.
type SandboxInfo struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Region    string `json:"region"`
	CreatedAt string `json:"createdAt"`
}

// CreateOpts parameterises CreateSandbox.
type CreateOpts struct {
	// IdleTimeoutMinutes is Railway's own idle auto-destroy — the platform
	// backstop under the Manager's reaper, so a dead daemon can never leak a
	// paid sandbox forever.
	IdleTimeoutMinutes int
	// Network is NetworkIsolated (default: public egress, what a user desktop
	// needs) or NetworkPrivate.
	Network string
	// Variables are baked into the sandbox environment at create time.
	Variables map[string]string
}

// ExecResult is the outcome of one sandboxExec.
type ExecResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	TimedOut  bool
	Truncated bool
}

// RailwayClient is the Manager-facing wire seam.
type RailwayClient interface {
	// CreateSandbox provisions a sandbox in the configured environment.
	CreateSandbox(ctx context.Context, opts CreateOpts) (SandboxInfo, error)
	// GetSandbox reads a sandbox's live status (the reattach read).
	GetSandbox(ctx context.Context, id string) (SandboxInfo, error)
	// ExecSandbox runs one shell command inside the sandbox, bounded by
	// timeoutSec server-side.
	ExecSandbox(ctx context.Context, id, command string, timeoutSec int) (ExecResult, error)
	// DestroySandbox tears the sandbox down. Destroying an already-gone
	// sandbox is not an error.
	DestroySandbox(ctx context.Context, id string) error
}

// RailwayConfig configures the wire client. Token and EnvironmentID come from
// the environment references RAILWAY_API_TOKEN / RAILWAY_ENVIRONMENT_ID at the
// serve seam — never from manifests, spec files, or durable memory.
type RailwayConfig struct {
	Token         string
	EnvironmentID string
	Endpoint      string        // "" -> DefaultEndpoint
	Timeout       time.Duration // <=0 -> defaultWireTimeout (ignored if HTTPClient set)
	HTTPClient    *http.Client  // nil -> internal client with Timeout
}

type railway struct {
	endpoint      string
	token         string
	environmentID string
	hc            *http.Client
}

var _ RailwayClient = (*railway)(nil)

// NewRailway constructs the live-schema Railway sandbox client.
func NewRailway(cfg RailwayConfig) RailwayClient {
	ep := cfg.Endpoint
	if ep == "" {
		ep = DefaultEndpoint
	}
	hc := cfg.HTTPClient
	if hc == nil {
		to := cfg.Timeout
		if to <= 0 {
			to = defaultWireTimeout
		}
		hc = &http.Client{Timeout: to}
	}
	return &railway{endpoint: ep, token: cfg.Token, environmentID: cfg.EnvironmentID, hc: hc}
}

func (r *railway) CreateSandbox(ctx context.Context, opts CreateOpts) (SandboxInfo, error) {
	network := opts.Network
	if network == "" {
		network = NetworkIsolated
	}
	const q = `mutation SandboxCreate($input: SandboxCreateInput!) {
  sandboxCreate(input: $input) { id status region createdAt }
}`
	input := map[string]any{
		"environmentId":    r.environmentID,
		"networkIsolation": network,
	}
	if opts.IdleTimeoutMinutes > 0 {
		input["idleTimeoutMinutes"] = opts.IdleTimeoutMinutes
	}
	if len(opts.Variables) > 0 {
		input["variables"] = opts.Variables
	}
	var out struct {
		SandboxCreate SandboxInfo `json:"sandboxCreate"`
	}
	if err := r.do(ctx, q, map[string]any{"input": input}, &out); err != nil {
		return SandboxInfo{}, err
	}
	return out.SandboxCreate, nil
}

func (r *railway) GetSandbox(ctx context.Context, id string) (SandboxInfo, error) {
	const q = `query Sandbox($environmentId: String!, $id: String!) {
  sandbox(environmentId: $environmentId, id: $id) { id status region createdAt }
}`
	var out struct {
		Sandbox SandboxInfo `json:"sandbox"`
	}
	if err := r.do(ctx, q, map[string]any{"environmentId": r.environmentID, "id": id}, &out); err != nil {
		return SandboxInfo{}, err
	}
	return out.Sandbox, nil
}

func (r *railway) ExecSandbox(ctx context.Context, id, command string, timeoutSec int) (ExecResult, error) {
	const q = `mutation SandboxExec($environmentId: String!, $id: String!, $command: String!, $timeoutSec: Int) {
  sandboxExec(environmentId: $environmentId, id: $id, command: $command, timeoutSec: $timeoutSec) {
    stdout stderr exitCode timedOut truncated
  }
}`
	vars := map[string]any{
		"environmentId": r.environmentID,
		"id":            id,
		"command":       command,
	}
	if timeoutSec > 0 {
		vars["timeoutSec"] = timeoutSec
	}
	var out struct {
		SandboxExec struct {
			Stdout    string `json:"stdout"`
			Stderr    string `json:"stderr"`
			ExitCode  int    `json:"exitCode"`
			TimedOut  bool   `json:"timedOut"`
			Truncated bool   `json:"truncated"`
		} `json:"sandboxExec"`
	}
	if err := r.do(ctx, q, vars, &out); err != nil {
		return ExecResult{}, err
	}
	e := out.SandboxExec
	return ExecResult{Stdout: e.Stdout, Stderr: e.Stderr, ExitCode: e.ExitCode, TimedOut: e.TimedOut, Truncated: e.Truncated}, nil
}

func (r *railway) DestroySandbox(ctx context.Context, id string) error {
	const q = `mutation SandboxDestroy($environmentId: String!, $id: String!) {
  sandboxDestroy(environmentId: $environmentId, id: $id) { id status }
}`
	err := r.do(ctx, q, map[string]any{"environmentId": r.environmentID, "id": id}, nil)
	if err != nil && errors.Is(err, ErrWireNotFound) {
		return nil
	}
	return err
}

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlError struct {
	Message string `json:"message"`
}

// do executes one GraphQL request, decoding response.data into out (nil to
// discard). HTTP 401/403 and "not authorized"/"not found" GraphQL errors map
// to sentinels; Railway returns most application errors as 200s with an
// errors array. The response body is bounded at 1 MiB.
func (r *railway) do(ctx context.Context, query string, variables map[string]any, out any) error {
	buf, err := json.Marshal(gqlRequest{Query: query, Variables: variables})
	if err != nil {
		return fmt.Errorf("dojo: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("dojo: build req: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := r.hc.Do(req)
	if err != nil {
		return fmt.Errorf("dojo: do: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrWireUnauthorized, http.StatusText(resp.StatusCode))
	case resp.StatusCode >= 400:
		return fmt.Errorf("%w: %d %s: %s", ErrWireUpstream, resp.StatusCode, http.StatusText(resp.StatusCode), truncate(respBody, 256))
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return fmt.Errorf("dojo: decode response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		msg := envelope.Errors[0].Message
		lower := strings.ToLower(msg)
		switch {
		case strings.Contains(lower, "not authorized"), strings.Contains(lower, "unauthorized"):
			return fmt.Errorf("%w: %s", ErrWireUnauthorized, msg)
		case strings.Contains(lower, "not found"), strings.Contains(lower, "does not exist"):
			return fmt.Errorf("%w: %s", ErrWireNotFound, msg)
		default:
			return fmt.Errorf("%w: %s", ErrWireUpstream, msg)
		}
	}
	if out != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("dojo: decode data: %w", err)
		}
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
