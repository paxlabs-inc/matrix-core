// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package railway is a thin client for Railway's public GraphQL API.
//
// Endpoint: https://backboard.railway.com/graphql/v2 (public;
// bearer-token via RAILWAY_API_TOKEN). Stdlib net/http +
// encoding/json — no third-party SDK so the trust surface stays
// auditable, mirroring router/internal/fly.
//
// Operations matrix-router needs (per-user environment lifecycle,
// scoped to ONE project + environment — Railway private networking
// only spans a single project/environment):
//
//	mutation serviceCreate     Create the per-user service (image source + env vars)
//	mutation volumeCreate      Attach the persistent volume (mount /data)
//	query    deployments       Latest deployment status (readiness gate)
//	mutation serviceDelete     Destroy the service
//	mutation volumeDelete      Destroy the volume
//
// The Provisioner built on this client implements
// provision.Provisioner. Wake is a NO-OP: Railway Serverless wakes a
// slept service on the first inbound packet — public or
// private-network — so the router's forwarded request IS the wake
// signal and the daemon-readiness probe absorbs the cold boot.
package railway

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

// DefaultEndpoint is the public GraphQL API base. Override via
// WithEndpoint for local mock servers.
const DefaultEndpoint = "https://backboard.railway.com/graphql/v2"

// Sentinel errors. Callers errors.Is to dispatch on these; the
// Provisioner maps them onto the provision package's neutral ones.
var (
	ErrNotFound     = errors.New("railway: not found")
	ErrUnauthorized = errors.New("railway: unauthorized (check RAILWAY_API_TOKEN)")
	ErrUpstream     = errors.New("railway: upstream error")
	ErrUncertain    = errors.New("railway: mutation outcome uncertain")
)

// Client makes authenticated calls to the Railway GraphQL API, scoped
// to one project + environment.
type Client struct {
	endpoint      string
	token         string
	projectID     string
	environmentID string
	hc            *http.Client
}

// New constructs a Client. token is the RAILWAY_API_TOKEN; projectID +
// environmentID scope every operation (RAILWAY_PROJECT_ID /
// RAILWAY_ENVIRONMENT_ID). Uses an internal http.Client with 30s
// default timeout; long waits (deployment readiness) poll with their
// own context.
func New(token, projectID, environmentID string) *Client {
	return &Client{
		endpoint:      DefaultEndpoint,
		token:         token,
		projectID:     projectID,
		environmentID: environmentID,
		hc:            &http.Client{Timeout: 30 * time.Second},
	}
}

// WithEndpoint overrides the API base URL — used by tests with mock
// servers. Returns *Client for chaining.
func (c *Client) WithEndpoint(ep string) *Client {
	c.endpoint = ep
	return c
}

// Service is the subset of Railway's service object the router cares
// about. Name matters: the private-network hostname is
// <name>.railway.internal.
type Service struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Volume is the subset of Railway's volume object the router cares about.
type Volume struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FindService returns the uniquely named service in this client's project.
func (c *Client) FindService(ctx context.Context, name string) (*Service, error) {
	const q = `query ProjectServices($id: String!) {
  project(id: $id) { services { edges { node { id name } } } }
}`
	var out struct {
		Project struct {
			Services struct {
				Edges []struct {
					Node Service `json:"node"`
				} `json:"edges"`
			} `json:"services"`
		} `json:"project"`
	}
	if err := c.do(ctx, q, map[string]any{"id": c.projectID}, &out); err != nil {
		return nil, err
	}
	var found *Service
	for _, edge := range out.Project.Services.Edges {
		if edge.Node.Name != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: duplicate service name %q", ErrUpstream, name)
		}
		svc := edge.Node
		found = &svc
	}
	if found == nil {
		return nil, fmt.Errorf("%w: service name %q", ErrNotFound, name)
	}
	return found, nil
}

// FindVolume returns the one volume attached to serviceID at mountPath in
// this client's environment.
func (c *Client) FindVolume(ctx context.Context, serviceID, mountPath string) (*Volume, error) {
	const q = `query ProjectVolumes($id: String!) {
  project(id: $id) {
    volumes { edges { node {
      id name
      volumeInstances { edges { node { environmentId serviceId mountPath } } }
    } } }
  }
}`
	var out struct {
		Project struct {
			Volumes struct {
				Edges []struct {
					Node struct {
						Volume
						VolumeInstances struct {
							Edges []struct {
								Node struct {
									EnvironmentID string `json:"environmentId"`
									ServiceID     string `json:"serviceId"`
									MountPath     string `json:"mountPath"`
								} `json:"node"`
							} `json:"edges"`
						} `json:"volumeInstances"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"volumes"`
		} `json:"project"`
	}
	if err := c.do(ctx, q, map[string]any{"id": c.projectID}, &out); err != nil {
		return nil, err
	}
	var found *Volume
	for _, volumeEdge := range out.Project.Volumes.Edges {
		for _, instanceEdge := range volumeEdge.Node.VolumeInstances.Edges {
			instance := instanceEdge.Node
			if instance.EnvironmentID != c.environmentID || instance.ServiceID != serviceID || instance.MountPath != mountPath {
				continue
			}
			if found != nil {
				return nil, fmt.Errorf("%w: multiple volumes for service %s at %s", ErrUpstream, serviceID, mountPath)
			}
			volume := volumeEdge.Node.Volume
			found = &volume
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%w: volume for service %s at %s", ErrNotFound, serviceID, mountPath)
	}
	return found, nil
}

func (c *Client) Service(ctx context.Context, id string) (*Service, error) {
	const q = `query Service($id: String!) { service(id: $id) { id name } }`
	var out struct {
		Service Service `json:"service"`
	}
	if err := c.do(ctx, q, map[string]any{"id": id}, &out); err != nil {
		return nil, err
	}
	return &out.Service, nil
}

func (c *Client) Volume(ctx context.Context, id string) (*Volume, error) {
	const q = `query ProjectVolumes($id: String!) {
  project(id: $id) { volumes { edges { node { id name } } } }
}`
	var out struct {
		Project struct {
			Volumes struct {
				Edges []struct {
					Node Volume `json:"node"`
				} `json:"edges"`
			} `json:"volumes"`
		} `json:"project"`
	}
	if err := c.do(ctx, q, map[string]any{"id": c.projectID}, &out); err != nil {
		return nil, err
	}
	for _, edge := range out.Project.Volumes.Edges {
		if edge.Node.ID == id {
			v := edge.Node
			return &v, nil
		}
	}
	return nil, fmt.Errorf("%w: volume %s", ErrNotFound, id)
}

// Deployment statuses Railway reports (subset the router dispatches on).
const (
	DeployStatusSuccess  = "SUCCESS"
	DeployStatusSleeping = "SLEEPING"
	DeployStatusFailed   = "FAILED"
	DeployStatusCrashed  = "CRASHED"
)

// Deployment is the subset of Railway's deployment object the router
// cares about.
type Deployment struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Live reports whether the deployment serves (or will serve on wake)
// traffic: SUCCESS is running; SLEEPING is a Serverless-slept service
// that revives on the first inbound packet.
func (d *Deployment) Live() bool {
	return d.Status == DeployStatusSuccess || d.Status == DeployStatusSleeping
}

// CreateService provisions a new service in the client's
// project/environment from an image source, with the given
// environment variables baked in at create time (one deployment, not
// a create-then-redeploy pair).
func (c *Client) CreateService(ctx context.Context, name, image string, env map[string]string) (*Service, error) {
	const q = `mutation ServiceCreate($input: ServiceCreateInput!) {
  serviceCreate(input: $input) { id name }
}`
	vars := map[string]any{
		"input": map[string]any{
			"projectId":     c.projectID,
			"environmentId": c.environmentID,
			"name":          name,
			"source":        map[string]any{"image": image},
			"variables":     env,
		},
	}
	var out struct {
		ServiceCreate Service `json:"serviceCreate"`
	}
	if err := c.do(ctx, q, vars, &out); err != nil {
		return nil, err
	}
	return &out.ServiceCreate, nil
}

// UpsertVariables merges variables into an existing service instance. Railway
// deploys the resulting staged change by default; replace is deliberately
// omitted so unrelated user-service configuration is preserved.
func (c *Client) UpsertVariables(ctx context.Context, serviceID string, env map[string]string) error {
	if serviceID == "" || len(env) == 0 {
		return fmt.Errorf("railway: service id and variables are required")
	}
	const q = `mutation VariableCollectionUpsert($input: VariableCollectionUpsertInput!) {
  variableCollectionUpsert(input: $input)
}`
	vars := map[string]any{
		"input": map[string]any{
			"projectId": c.projectID, "environmentId": c.environmentID,
			"serviceId": serviceID, "variables": env,
		},
	}
	return c.do(ctx, q, vars, nil)
}

// CreateVolume attaches a fresh persistent volume to the service at
// mountPath. Railway allows exactly ONE volume mount per service.
func (c *Client) CreateVolume(ctx context.Context, serviceID, mountPath string) (*Volume, error) {
	const q = `mutation VolumeCreate($input: VolumeCreateInput!) {
  volumeCreate(input: $input) { id name }
}`
	vars := map[string]any{
		"input": map[string]any{
			"projectId":     c.projectID,
			"environmentId": c.environmentID,
			"serviceId":     serviceID,
			"mountPath":     mountPath,
		},
	}
	var out struct {
		VolumeCreate Volume `json:"volumeCreate"`
	}
	if err := c.do(ctx, q, vars, &out); err != nil {
		return nil, err
	}
	return &out.VolumeCreate, nil
}

// LatestDeployment returns the service's most recent deployment, or
// ErrNotFound when the service has none yet.
func (c *Client) LatestDeployment(ctx context.Context, serviceID string) (*Deployment, error) {
	const q = `query Deployments($input: DeploymentListInput!, $first: Int) {
  deployments(input: $input, first: $first) {
    edges { node { id status } }
  }
}`
	vars := map[string]any{
		"input": map[string]any{
			"projectId":     c.projectID,
			"environmentId": c.environmentID,
			"serviceId":     serviceID,
		},
		"first": 1,
	}
	var out struct {
		Deployments struct {
			Edges []struct {
				Node Deployment `json:"node"`
			} `json:"edges"`
		} `json:"deployments"`
	}
	if err := c.do(ctx, q, vars, &out); err != nil {
		return nil, err
	}
	if len(out.Deployments.Edges) == 0 {
		return nil, fmt.Errorf("%w: service %s has no deployments", ErrNotFound, serviceID)
	}
	return &out.Deployments.Edges[0].Node, nil
}

// DeleteService tears the service down.
func (c *Client) DeleteService(ctx context.Context, serviceID string) error {
	const q = `mutation ServiceDelete($id: String!) {
  serviceDelete(id: $id)
}`
	return c.do(ctx, q, map[string]any{"id": serviceID}, nil)
}

// DeleteVolume tears the volume down.
func (c *Client) DeleteVolume(ctx context.Context, volumeID string) error {
	const q = `mutation VolumeDelete($volumeId: String!) {
  volumeDelete(volumeId: $volumeId)
}`
	return c.do(ctx, q, map[string]any{"volumeId": volumeID}, nil)
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

// do executes a GraphQL request, decoding response.data into out (nil
// to discard). Maps HTTP 401/403 and "not authorized"/"not found"
// GraphQL errors to sentinel errors for clean callsite branching —
// Railway returns most application errors as 200s with an errors
// array.
func (c *Client) do(ctx context.Context, query string, variables map[string]any, out any) error {
	buf, err := json.Marshal(gqlRequest{Query: query, Variables: variables})
	if err != nil {
		return fmt.Errorf("railway: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("railway: build req: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: railway request: %v", ErrUncertain, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrUnauthorized, http.StatusText(resp.StatusCode))
	case resp.StatusCode >= 400:
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w: %d %s", ErrUncertain, resp.StatusCode, http.StatusText(resp.StatusCode))
		}
		return fmt.Errorf("%w: %d %s: %s", ErrUpstream, resp.StatusCode, http.StatusText(resp.StatusCode), truncate(respBody, 256))
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return fmt.Errorf("%w: decode response: %v", ErrUncertain, err)
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
			return fmt.Errorf("railway: decode data: %w", err)
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
