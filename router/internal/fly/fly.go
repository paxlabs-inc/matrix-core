// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package fly is a thin client for the Fly Machines REST API.
//
// Endpoint: https://api.machines.dev (public; bearer-token via
// FLY_API_TOKEN). Stdlib net/http + encoding/json — no third-party
// SDK so the trust surface stays auditable.
//
// Operations matrix-router needs:
//
//	GET    /v1/apps/{app}/machines/{id}        Machine status
//	POST   /v1/apps/{app}/machines/{id}/start  Wake from suspend
//	POST   /v1/apps/{app}/machines             Create
//	DELETE /v1/apps/{app}/machines/{id}        Destroy
//	POST   /v1/apps/{app}/volumes              Volume create
//
// Higher-order helpers (WaitStarted, EnsureStarted) compose these.
package fly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultEndpoint is the public Machines API base. Override via
// WithEndpoint for local mock servers.
const DefaultEndpoint = "https://api.machines.dev"

// Sentinel errors. Callers errors.Is to dispatch on these.
var (
	ErrMachineNotFound = errors.New("fly: machine not found")
	ErrAppNotFound     = errors.New("fly: app not found")
	ErrUnauthorized    = errors.New("fly: unauthorized (check FLY_API_TOKEN)")
	ErrUpstream        = errors.New("fly: upstream error")
)

// Client makes authenticated calls to the Machines API.
type Client struct {
	endpoint string
	token    string
	app      string
	hc       *http.Client
}

// New constructs a Client. Token is the FLY_API_TOKEN. app is the Fly
// app slug (e.g. "matrix-daemon"). Uses an internal http.Client with
// 30s default timeout for read endpoints; long polls (start) take
// their own context.
func New(token, app string) *Client {
	return &Client{
		endpoint: DefaultEndpoint,
		token:    token,
		app:      app,
		hc:       &http.Client{Timeout: 30 * time.Second},
	}
}

// WithEndpoint overrides the API base URL — used by tests with mock
// servers. Returns *Client for chaining.
func (c *Client) WithEndpoint(ep string) *Client {
	c.endpoint = ep
	return c
}

// Machine is the subset of the Fly Machines API "machine" object that
// matrix-router cares about. Other fields are intentionally dropped.
type Machine struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	State      string         `json:"state"` // started, stopped, suspended, ...
	Region     string         `json:"region"`
	PrivateIP  string         `json:"private_ip"`
	InstanceID string         `json:"instance_id"`
	CreatedAt  string         `json:"created_at"`
	Config     map[string]any `json:"config,omitempty"`
}

// Started reports whether the Machine state is the running state ("started").
func (m *Machine) Started() bool { return m.State == "started" }

// Volume is the subset of the Fly volume object.
type Volume struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state"`
	SizeGB    int    `json:"size_gb"`
	Region    string `json:"region"`
	CreatedAt string `json:"created_at"`
}

// CreateMachineRequest is the POST /machines body. We omit everything
// optional so callers can supply only what the matrix-daemon image
// needs (image+env+region+volume mount).
type CreateMachineRequest struct {
	Name   string              `json:"name,omitempty"`
	Region string              `json:"region,omitempty"`
	Config CreateMachineConfig `json:"config"`
}

// CreateMachineConfig wraps the immutable Machine config block.
type CreateMachineConfig struct {
	Image       string                `json:"image"`
	Env         map[string]string     `json:"env,omitempty"`
	Mounts      []CreateMachineMount  `json:"mounts,omitempty"`
	Services    []map[string]any      `json:"services,omitempty"`
	Guest       *CreateMachineGuest   `json:"guest,omitempty"`
	Restart     *CreateMachineRestart `json:"restart,omitempty"`
	AutoDestroy bool                  `json:"auto_destroy,omitempty"`
	Init        map[string]any        `json:"init,omitempty"`
}

// CreateMachineMount attaches a volume.
type CreateMachineMount struct {
	Volume string `json:"volume"`
	Path   string `json:"path"`
}

// CreateMachineGuest sizes the VM.
type CreateMachineGuest struct {
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`
	CPUKind  string `json:"cpu_kind"`
}

// CreateMachineRestart matches Fly's restart policy block.
type CreateMachineRestart struct {
	Policy string `json:"policy"`
}

// CreateVolumeRequest creates a volume.
type CreateVolumeRequest struct {
	Name   string `json:"name"`
	Region string `json:"region"`
	SizeGB int    `json:"size_gb"`
}

// GetMachine fetches a machine by id.
func (c *Client) GetMachine(ctx context.Context, id string) (*Machine, error) {
	var m Machine
	if err := c.do(ctx, http.MethodGet, c.machinesPath()+"/"+id, nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// StartMachine wakes a stopped/suspended machine. Idempotent: starting
// an already-started machine returns nil.
func (c *Client) StartMachine(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, c.machinesPath()+"/"+id+"/start", nil, nil)
}

// CreateMachine provisions a new machine with the given config.
func (c *Client) CreateMachine(ctx context.Context, req *CreateMachineRequest) (*Machine, error) {
	var m Machine
	if err := c.do(ctx, http.MethodPost, c.machinesPath(), req, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// DestroyMachine deletes a machine. force=true unconditionally tears it
// down even if started.
func (c *Client) DestroyMachine(ctx context.Context, id string, force bool) error {
	q := ""
	if force {
		q = "?force=true"
	}
	return c.do(ctx, http.MethodDelete, c.machinesPath()+"/"+id+q, nil, nil)
}

// CreateVolume provisions a fresh volume in the given region.
func (c *Client) CreateVolume(ctx context.Context, req *CreateVolumeRequest) (*Volume, error) {
	var v Volume
	if err := c.do(ctx, http.MethodPost, c.volumesPath(), req, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// EnsureStarted wakes the machine if not already started, then polls
// at probeInterval (jittered up to 1.5x) until the machine reports
// state=started or ctx times out. Returns the final Machine state.
//
// This is the hot-path the proxy calls per request: when the user's
// machine is suspended it incurs a wake-then-poll latency (~250ms-2s
// in our experience). The proxy adds ~one extra DB round-trip + the
// wake; an already-started machine returns near-instantly because the
// first GetMachine sees Started=true.
func (c *Client) EnsureStarted(ctx context.Context, id string, probeInterval time.Duration) (*Machine, error) {
	m, err := c.GetMachine(ctx, id)
	if err != nil {
		return nil, err
	}
	if m.Started() {
		return m, nil
	}
	if err := c.StartMachine(ctx, id); err != nil {
		return nil, fmt.Errorf("fly: start: %w", err)
	}
	// Poll until Started or ctx deadline.
	if probeInterval <= 0 {
		probeInterval = 250 * time.Millisecond
	}
	t := time.NewTicker(probeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
			m, err := c.GetMachine(ctx, id)
			if err != nil {
				return nil, err
			}
			if m.Started() {
				return m, nil
			}
		}
	}
}

// machinesPath returns "/v1/apps/<app>/machines".
func (c *Client) machinesPath() string {
	return "/v1/apps/" + c.app + "/machines"
}

// volumesPath returns "/v1/apps/<app>/volumes".
func (c *Client) volumesPath() string {
	return "/v1/apps/" + c.app + "/volumes"
}

// do executes a JSON request, decoding the response into out (nil to
// discard). Maps 401/403/404 to sentinel errors for clean callsite
// branching.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("fly: marshal: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, rdr)
	if err != nil {
		return fmt.Errorf("fly: build req: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("fly: do: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrUnauthorized, http.StatusText(resp.StatusCode))
	case resp.StatusCode == http.StatusNotFound:
		// Differentiate machine-not-found vs app-not-found via path shape.
		if path == c.machinesPath() {
			return ErrAppNotFound
		}
		return ErrMachineNotFound
	case resp.StatusCode >= 400:
		return fmt.Errorf("%w: %d %s: %s", ErrUpstream, resp.StatusCode, http.StatusText(resp.StatusCode), truncate(respBody, 256))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("fly: decode response: %w", err)
		}
	}
	return nil
}

// truncate trims b for error-message inclusion so we don't dump the
// upstream payload into application logs.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
