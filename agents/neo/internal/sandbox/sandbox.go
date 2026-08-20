// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package sandbox is a Go-native client for Railway's ephemeral sandbox
// API, used by the Neo daemon to turn a finished project into an on-demand preview.
//
// It extends the transport pattern of router/internal/railway: the same
// GraphQL-over-HTTP endpoint (https://backboard.railway.com/graphql/v2), the
// same bearer-token auth (RAILWAY_API_TOKEN), the same 200-with-errors-array
// handling and sentinel-error mapping — stdlib net/http + encoding/json, no
// third-party SDK, so the trust surface stays auditable.
//
// Sandboxes are created PRIVATE (they reach the project/environment's private
// services and have no public NAT egress), on the SAME project + environment
// as the user's machine (RAILWAY_PROJECT_ID / RAILWAY_ENVIRONMENT_ID). Railway
// sandboxes expose no public URL/port, so the preview is reached by the router
// /preview/{user} proxy over railway.internal — never a public domain.
//
// the Neo daemon-facing seam: the Client interface (Create/Exec/WriteFiles/Destroy).
// Task 3.3 consumes this interface. A Node-sidecar fallback (per spec req 7.4)
// would satisfy the identical interface, so callers never learn which backs it.
//
// GraphQL schema caveat: the live Railway sandbox API is NOT reachable from
// the build box and its GraphQL schema is not publicly documented (only the
// railway-ts-sdk beta v3 surface is). The operation/field names below
// (sandboxCreate, sandboxExec, sandboxFilesWrite, sandboxDestroy and their
// input/output shapes) are the client's best-effort mapping of that SDK
// surface onto GraphQL and are marked in code as FIXTURE-VERIFIED-ONLY. They
// must be reconciled against the live API before production use; because every
// operation funnels through do(), only the const query strings + variable maps
// change if the live schema differs.
package sandbox

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

// DefaultEndpoint is the Railway public GraphQL API base — identical to
// router/internal/railway.DefaultEndpoint. Override via Config.Endpoint for
// tests / local mock servers.
const DefaultEndpoint = "https://backboard.railway.com/graphql/v2"

// defaultTimeout bounds a single GraphQL round-trip when Config.Timeout is 0.
// Larger than the router's provisioner (30s) because sandbox create + exec can
// legitimately run for a while; long operations should still pass their own
// context deadline.
const defaultTimeout = 120 * time.Second

// NetworkMode selects a sandbox's networking posture.
//
//   - NetworkPrivate reaches the project/environment's private services over
//     railway.internal and has no public NAT egress. This is what previews use.
//   - NetworkIsolated has public NAT egress but cannot reach environment
//     services — used for hermetic browser-driven e2e (playwright) where the
//     sandbox needs the open internet but not the user's services.
type NetworkMode string

const (
	NetworkPrivate  NetworkMode = "PRIVATE"
	NetworkIsolated NetworkMode = "ISOLATED"
)

// Sandbox statuses Railway reports (subset the Neo daemon dispatches on). The exact
// string set is FIXTURE-VERIFIED-ONLY — reconcile against the live API.
const (
	StatusCreating = "CREATING"
	StatusRunning  = "RUNNING"
	StatusStopped  = "STOPPED"
	StatusFailed   = "FAILED"
)

// Sentinel errors. Callers use errors.Is to dispatch; the Neo daemon maps them onto its
// own preview error taxonomy. Mirrors the router railway client's set.
var (
	ErrNotFound     = errors.New("sandbox: not found")
	ErrUnauthorized = errors.New("sandbox: unauthorized (check RAILWAY_API_TOKEN)")
	ErrUpstream     = errors.New("sandbox: upstream error")
)

// Sandbox is the subset of Railway's sandbox object the Neo daemon cares about.
// PrivateHost is the railway.internal hostname the router /preview proxy
// forwards to over the private network — sandboxes expose no public domain.
type Sandbox struct {
	ID          string `json:"id"`
	PrivateHost string `json:"privateHost"`
	Status      string `json:"status"`
}

// CreateOpts parameterises Create. Image is the sandbox base template image;
// Env is baked in at create time; Network defaults to NetworkPrivate (the
// preview posture) when empty.
type CreateOpts struct {
	Image   string
	Env     map[string]string
	Network NetworkMode
}

// File is one file written into a sandbox's filesystem (deploying project
// state). Content is the raw file bytes as a string.
type File struct {
	Path    string
	Content string
}

// ExecResult is the outcome of one Exec: the aggregated streamed stdout/stderr,
// the process exit code, and the wall-clock duration.
//
// Streaming caveat: Railway streams stdout during exec, but this seam returns
// the aggregated result once the command completes — the value the Neo daemon needs for
// health-probing and evidence. A live-tail variant (GraphQL subscription /
// websocket) is a future addition behind the same interface, not v1.
type ExecResult struct {
	Stdout   string
	Stderr   string
	Exit     int
	Duration time.Duration
}

// Client is the the Neo daemon-facing seam. Task 3.3 (preview lifecycle) consumes ONLY
// this interface, so a Node-sidecar fallback (spec req 7.4) is drop-in.
type Client interface {
	// Create provisions a sandbox (PRIVATE network by default, from a template
	// base image) in the client's project/environment.
	Create(ctx context.Context, opts CreateOpts) (*Sandbox, error)
	// Exec runs cmd (argv form) in the sandbox and returns the aggregated
	// stdout/stderr, exit code, and duration.
	Exec(ctx context.Context, sandboxID string, cmd []string) (ExecResult, error)
	// WriteFiles deploys project state by writing files into the sandbox.
	WriteFiles(ctx context.Context, sandboxID string, files []File) error
	// Destroy tears the sandbox down (the TTL reaper and explicit re-preview
	// both call it). Destroying an already-gone sandbox is not an error.
	Destroy(ctx context.Context, sandboxID string) error
}

// Config configures the GraphQL client. Token/ProjectID/EnvironmentID are read
// from RAILWAY_API_TOKEN / RAILWAY_PROJECT_ID / RAILWAY_ENVIRONMENT_ID by the
// the Neo daemon caller (not by this package). Endpoint and HTTPClient are override
// seams for tests; both are optional.
type Config struct {
	Token         string
	ProjectID     string
	EnvironmentID string
	Endpoint      string        // "" -> DefaultEndpoint
	Timeout       time.Duration // <=0 -> defaultTimeout (ignored if HTTPClient set)
	HTTPClient    *http.Client  // nil -> internal client with Timeout
}

// client is the concrete GraphQL-backed Client.
type client struct {
	endpoint      string
	token         string
	projectID     string
	environmentID string
	hc            *http.Client
}

// compile-time proof the concrete type satisfies the seam.
var _ Client = (*client)(nil)

// New constructs a GraphQL-backed sandbox Client from cfg.
func New(cfg Config) Client {
	ep := cfg.Endpoint
	if ep == "" {
		ep = DefaultEndpoint
	}
	hc := cfg.HTTPClient
	if hc == nil {
		to := cfg.Timeout
		if to <= 0 {
			to = defaultTimeout
		}
		hc = &http.Client{Timeout: to}
	}
	return &client{
		endpoint:      ep,
		token:         cfg.Token,
		projectID:     cfg.ProjectID,
		environmentID: cfg.EnvironmentID,
		hc:            hc,
	}
}

// Create — FIXTURE-VERIFIED-ONLY schema (sandboxCreate).
func (c *client) Create(ctx context.Context, opts CreateOpts) (*Sandbox, error) {
	network := opts.Network
	if network == "" {
		network = NetworkPrivate
	}
	const q = `mutation SandboxCreate($input: SandboxCreateInput!) {
  sandboxCreate(input: $input) { id privateHost status }
}`
	input := map[string]any{
		"projectId":     c.projectID,
		"environmentId": c.environmentID,
		"image":         opts.Image,
		"networkMode":   string(network),
	}
	if len(opts.Env) > 0 {
		input["variables"] = opts.Env
	}
	var out struct {
		SandboxCreate Sandbox `json:"sandboxCreate"`
	}
	if err := c.do(ctx, q, map[string]any{"input": input}, &out); err != nil {
		return nil, err
	}
	return &out.SandboxCreate, nil
}

// Exec — FIXTURE-VERIFIED-ONLY schema (sandboxExec).
func (c *client) Exec(ctx context.Context, sandboxID string, cmd []string) (ExecResult, error) {
	const q = `mutation SandboxExec($input: SandboxExecInput!) {
  sandboxExec(input: $input) { stdout stderr exitCode durationMs }
}`
	input := map[string]any{
		"sandboxId": sandboxID,
		"command":   cmd,
	}
	var out struct {
		SandboxExec struct {
			Stdout     string `json:"stdout"`
			Stderr     string `json:"stderr"`
			ExitCode   int    `json:"exitCode"`
			DurationMs int64  `json:"durationMs"`
		} `json:"sandboxExec"`
	}
	if err := c.do(ctx, q, map[string]any{"input": input}, &out); err != nil {
		return ExecResult{}, err
	}
	r := out.SandboxExec
	return ExecResult{
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Exit:     r.ExitCode,
		Duration: time.Duration(r.DurationMs) * time.Millisecond,
	}, nil
}

// WriteFiles — FIXTURE-VERIFIED-ONLY schema (sandboxFilesWrite).
func (c *client) WriteFiles(ctx context.Context, sandboxID string, files []File) error {
	if len(files) == 0 {
		return nil
	}
	const q = `mutation SandboxFilesWrite($input: SandboxFilesWriteInput!) {
  sandboxFilesWrite(input: $input) { written }
}`
	payload := make([]map[string]any, 0, len(files))
	for _, f := range files {
		payload = append(payload, map[string]any{"path": f.Path, "content": f.Content})
	}
	input := map[string]any{
		"sandboxId": sandboxID,
		"files":     payload,
	}
	return c.do(ctx, q, map[string]any{"input": input}, nil)
}

// Destroy — FIXTURE-VERIFIED-ONLY schema (sandboxDestroy). An already-gone
// sandbox (ErrNotFound) is treated as success — Destroy is idempotent so the
// TTL reaper and explicit re-preview never fight over a teardown.
func (c *client) Destroy(ctx context.Context, sandboxID string) error {
	const q = `mutation SandboxDestroy($id: String!) {
  sandboxDestroy(id: $id)
}`
	err := c.do(ctx, q, map[string]any{"id": sandboxID}, nil)
	if err != nil && errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// gqlRequest is the GraphQL-over-HTTP request body.
type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// gqlError is one entry of the GraphQL "errors" array.
type gqlError struct {
	Message string `json:"message"`
}

// do executes a GraphQL request against the Railway endpoint, decoding
// response.data into out (nil to discard). It mirrors the router railway
// client: HTTP 401/403 and "not authorized"/"not found" GraphQL errors map to
// sentinels; Railway returns most application errors as 200s with an errors
// array. The response body is limited to 1 MiB.
func (c *client) do(ctx context.Context, query string, variables map[string]any, out any) error {
	buf, err := json.Marshal(gqlRequest{Query: query, Variables: variables})
	if err != nil {
		return fmt.Errorf("sandbox: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("sandbox: build req: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox: do: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrUnauthorized, http.StatusText(resp.StatusCode))
	case resp.StatusCode >= 400:
		return fmt.Errorf("%w: %d %s: %s", ErrUpstream, resp.StatusCode, http.StatusText(resp.StatusCode), truncate(respBody, 256))
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return fmt.Errorf("sandbox: decode response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		msg := envelope.Errors[0].Message
		lower := strings.ToLower(msg)
		switch {
		case strings.Contains(lower, "not authorized"), strings.Contains(lower, "unauthorized"):
			return fmt.Errorf("%w: %s", ErrUnauthorized, msg)
		case strings.Contains(lower, "not found"), strings.Contains(lower, "does not exist"):
			return fmt.Errorf("%w: %s", ErrNotFound, msg)
		default:
			return fmt.Errorf("%w: %s", ErrUpstream, msg)
		}
	}
	if out != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("sandbox: decode data: %w", err)
		}
	}
	return nil
}

// truncate trims b for error-message inclusion so the upstream payload never
// gets dumped wholesale into application logs.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
