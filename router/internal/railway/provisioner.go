// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// provisioner.go — provision.Provisioner implemented on the Railway
// GraphQL client.
//
// Ensure = serviceCreate (image + env vars) + volumeCreate (/data) +
// poll the latest deployment until it is live, so the users row flips
// to 'active' (and GET /provision/status reports it) only once the
// environment genuinely serves — the onboarding readiness gate works
// unchanged.
//
// Wake = NO-OP: Railway Serverless wakes a slept service on the first
// inbound private-network packet, so the proxy's forwarded request is
// the wake signal and waitDaemonReady absorbs the cold boot.
//
// Endpoint = <service-name>.railway.internal (private DNS, IPv6-only
// network), derived deterministically from the user id — no API
// round-trip on the request hot path.
package railway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"matrix/router/internal/provision"
)

// Provisioner adapts *Client to provision.Provisioner.
type Provisioner struct {
	Client *Client
	// Image is the environment OCI image every new service runs
	// (ROUTER_DAEMON_IMAGE). Empty disables Ensure with a config error.
	Image string
	// ProbeInterval is the deployment-readiness poll cadence inside
	// Ensure; <=0 defaults to 2s (deployments take tens of seconds —
	// no point polling faster).
	ProbeInterval time.Duration
}

var _ provision.Provisioner = (*Provisioner)(nil)
var _ provision.Recoverable = (*Provisioner)(nil)

func (p *Provisioner) ServiceName(userID string) string { return ServiceName(userID) }

// Ensure provisions the per-user service + volume and blocks until the
// first deployment is live (or ctx expires). The caller's provision
// budget (admin.ProvisionTimeout) bounds the wait.
func (p *Provisioner) Ensure(ctx context.Context, req provision.CreateRequest) (*provision.Env, error) {
	if p.Image == "" {
		return nil, errors.New("railway: DaemonImage not configured (set ROUTER_DAEMON_IMAGE env)")
	}
	name := ServiceName(req.UserID)

	svc, err := p.Client.CreateService(ctx, name, p.Image, req.Env)
	if err != nil {
		return nil, mapErr(fmt.Errorf("create service: %w", err))
	}

	vol, err := p.Client.CreateVolume(ctx, svc.ID, "/data")
	if err != nil {
		// Leave the service for the operator: it is inspectable in the
		// Railway dashboard and deletable in one click, mirroring the
		// fly volume-leak policy.
		return nil, mapErr(fmt.Errorf("create volume: %w", err))
	}

	env := p.env(svc.ID, svc.Name, req.Region)
	env.VolumeID = vol.ID

	// Readiness gate: poll the latest deployment until it is live so
	// provision_jobs / GET /provision/status only report done/active
	// once the environment can serve.
	if err := p.waitDeployed(ctx, svc.ID); err != nil {
		return nil, fmt.Errorf("await deployment: %w", err)
	}
	env.Ready = true
	env.State = "deployed"
	return env, nil
}

func (p *Provisioner) FindService(ctx context.Context, name string) (*provision.Env, error) {
	svc, err := p.Client.FindService(ctx, name)
	if err != nil {
		return nil, mapErr(err)
	}
	return p.env(svc.ID, svc.Name, ""), nil
}

func (p *Provisioner) FindVolume(ctx context.Context, serviceID, mountPath string) (string, error) {
	vol, err := p.Client.FindVolume(ctx, serviceID, mountPath)
	if err != nil {
		return "", mapErr(err)
	}
	return vol.ID, nil
}

func (p *Provisioner) CreateService(ctx context.Context, req provision.CreateRequest) (*provision.Env, error) {
	if p.Image == "" {
		return nil, errors.New("railway: DaemonImage not configured (set ROUTER_DAEMON_IMAGE env)")
	}
	svc, err := p.Client.CreateService(ctx, ServiceName(req.UserID), p.Image, req.Env)
	if err != nil {
		return nil, mapErr(err)
	}
	return p.env(svc.ID, svc.Name, req.Region), nil
}

func (p *Provisioner) CreateVolume(ctx context.Context, serviceID, mountPath string) (string, error) {
	vol, err := p.Client.CreateVolume(ctx, serviceID, mountPath)
	if err != nil {
		return "", mapErr(err)
	}
	return vol.ID, nil
}

func (p *Provisioner) WaitReady(ctx context.Context, serviceID string) error {
	return p.waitDeployed(ctx, serviceID)
}

func (p *Provisioner) ServiceAbsent(ctx context.Context, serviceID string) (bool, error) {
	_, err := p.Client.Service(ctx, serviceID)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	return false, mapErr(err)
}

func (p *Provisioner) VolumeAbsent(ctx context.Context, volumeID string) (bool, error) {
	_, err := p.Client.Volume(ctx, volumeID)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	return false, mapErr(err)
}

func (p *Provisioner) DeleteService(ctx context.Context, serviceID string) error {
	return mapErr(p.Client.DeleteService(ctx, serviceID))
}

func (p *Provisioner) DeleteVolume(ctx context.Context, volumeID string) error {
	return mapErr(p.Client.DeleteVolume(ctx, volumeID))
}

// Status reports the latest deployment state; Live (SUCCESS or
// SLEEPING) counts as ready — a slept service revives on the first
// inbound packet.
func (p *Provisioner) Status(ctx context.Context, ref provision.Ref) (*provision.Env, error) {
	d, err := p.Client.LatestDeployment(ctx, ref.EnvID)
	if err != nil {
		return nil, mapErr(err)
	}
	env := p.env(ref.EnvID, ServiceName(ref.UserID), "")
	env.VolumeID = ref.VolumeID
	env.State = d.Status
	env.Ready = d.Live()
	return env, nil
}

// Wake is a NO-OP by design: Railway Serverless wakes the service on
// the first inbound private-network packet, so the forwarded request
// itself is the wake signal. No API call — the request hot path stays
// free of provider round-trips; waitDaemonReady absorbs the cold boot.
func (p *Provisioner) Wake(ctx context.Context, ref provision.Ref) (*provision.Env, error) {
	env := p.env(ref.EnvID, ServiceName(ref.UserID), "")
	env.VolumeID = ref.VolumeID
	env.Ready = true
	return env, nil
}

// Destroy deletes the service, then best-effort deletes the volume
// (Railway does not cascade volume deletion with the service).
func (p *Provisioner) Destroy(ctx context.Context, ref provision.Ref) error {
	if err := p.Client.DeleteService(ctx, ref.EnvID); err != nil {
		return mapErr(err)
	}
	if ref.VolumeID != "" {
		if err := p.Client.DeleteVolume(ctx, ref.VolumeID); err != nil && !errors.Is(err, ErrNotFound) {
			return mapErr(fmt.Errorf("delete volume: %w", err))
		}
	}
	return nil
}

// WakeOnRequest is true: waking is platform-managed by inbound
// traffic, and the readiness probe must absorb the whole cold boot
// (including the documented possible first-request 502 from the
// private-network edge while the service revives).
func (p *Provisioner) WakeOnRequest() bool { return true }

// waitDeployed polls the service's latest deployment until Live, a
// terminal failure, or ctx expiry. A service created moments ago may
// briefly report no deployments — treated as not-ready, not an error.
func (p *Provisioner) waitDeployed(ctx context.Context, serviceID string) error {
	interval := p.ProbeInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		d, err := p.Client.LatestDeployment(ctx, serviceID)
		switch {
		case err != nil && errors.Is(err, ErrNotFound):
			// no deployment registered yet — keep waiting
		case err != nil:
			return mapErr(err)
		case d.Live():
			return nil
		case d.Status == DeployStatusFailed || d.Status == DeployStatusCrashed:
			return fmt.Errorf("railway: deployment %s is %s", d.ID, d.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// env builds the neutral view. The endpoint is the service's private
// hostname; the port is left empty so the router's configured daemon
// port applies (the daemon binds the same port on every provider).
func (p *Provisioner) env(id, name, region string) *provision.Env {
	return &provision.Env{
		ID:     id,
		Region: region,
		Endpoint: provision.Endpoint{
			Host: name + ".railway.internal",
		},
	}
}

// ServiceName derives the deterministic per-user service name — and
// therefore the <name>.railway.internal private hostname — from the
// user id, mirroring the fly machine-name convention
// ("matrix-" + DNS-safe user id prefix).
func ServiceName(userID string) string {
	return "matrix-" + safeName(userID, 26)
}

// mapErr translates the client's sentinel errors onto the neutral ones
// so interface consumers dispatch without importing railway.
// Everything else (including context.DeadlineExceeded) passes through
// unchanged.
func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: %v", provision.ErrNotFound, err)
	case errors.Is(err, ErrUnauthorized):
		return fmt.Errorf("%w: %v", provision.ErrUnauthorized, err)
	case errors.Is(err, ErrUncertain):
		return fmt.Errorf("%w: %v", provision.ErrUncertain, err)
	default:
		return err
	}
}

// safeName returns a DNS-safe lowercase prefix of s, max length n —
// the same sanitization the fly provisioner applies to machine names,
// so a user keeps the same environment name across providers.
func safeName(s string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < len(s) && len(out) < n; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-', c == '_':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		}
	}
	if len(out) == 0 {
		return "user"
	}
	return string(out)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
