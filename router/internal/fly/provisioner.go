// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// provisioner.go — provision.Provisioner implemented on the Fly
// Machines client. Behavior is the pre-extraction router flow verbatim:
// Ensure = CreateVolume + CreateMachine (the old admin.provisionMachine),
// Wake = EnsureStarted (explicit start API + state polling), Destroy =
// force machine delete. The provider-neutral Endpoint prefers the 6PN
// private IPv6 and falls back to Fly internal DNS, exactly as the proxy
// used to compute upstream hosts.
package fly

import (
	"context"
	"errors"
	"fmt"
	"time"

	"centra/router/internal/provision"
)

// Provisioner adapts *Client to provision.Provisioner. The fields that
// used to live on admin.Handler (image, volume sizing) are provider
// config, so they move here.
type Provisioner struct {
	Client *Client
	// Image is the daemon OCI image every new Machine runs
	// (ROUTER_DAEMON_IMAGE). Empty disables Ensure with a config error.
	Image string
	// VolumeSizeGB sizes each user's persistent volume; <=0 defaults to 5.
	VolumeSizeGB int
	// ProbeInterval is the EnsureStarted poll cadence; <=0 defaults to
	// the client's 250ms.
	ProbeInterval time.Duration
}

var _ provision.Provisioner = (*Provisioner)(nil)

// Ensure provisions a fresh Volume + Machine for the user. The Machine
// config (guest sizing, restart policy, /data mount) is unchanged from
// the pre-extraction admin.provisionMachine.
func (p *Provisioner) Ensure(ctx context.Context, req provision.CreateRequest) (*provision.Env, error) {
	if p.Image == "" {
		return nil, errors.New("fly: DaemonImage not configured (set ROUTER_DAEMON_IMAGE env)")
	}
	volSize := p.VolumeSizeGB
	if volSize <= 0 {
		volSize = 5
	}

	// Volume name must be [a-z0-9_] only and ≤30 chars per Fly API.
	// "matrix_" prefix (7) leaves 23 chars for the user id; we map
	// hyphens to underscores so UUIDs round-trip cleanly.
	volName := "matrix_" + volumeSafeName(req.UserID, 23)
	vol, err := p.Client.CreateVolume(ctx, &CreateVolumeRequest{
		Name:   volName,
		Region: req.Region,
		SizeGB: volSize,
	})
	if err != nil {
		return nil, mapErr(fmt.Errorf("create volume: %w", err))
	}

	mreq := &CreateMachineRequest{
		Name:   "matrix-" + safeName(req.UserID, 26),
		Region: req.Region,
		Config: CreateMachineConfig{
			Image: p.Image,
			Env:   req.Env,
			Mounts: []CreateMachineMount{
				{Volume: vol.ID, Path: "/data"},
			},
			Guest: &CreateMachineGuest{
				CPUs:     1,
				MemoryMB: 1024,
				CPUKind:  "shared",
			},
			Restart: &CreateMachineRestart{Policy: "on-failure"},
		},
	}
	mach, err := p.Client.CreateMachine(ctx, mreq)
	if err != nil {
		// Best-effort cleanup: leave the volume — it's billed but
		// reattachable, and an operator can manually reap it via
		// `flyctl volumes destroy`.
		return nil, mapErr(fmt.Errorf("create machine: %w", err))
	}
	env := p.env(mach)
	env.VolumeID = vol.ID
	return env, nil
}

// Status fetches the machine and reports Started as readiness.
func (p *Provisioner) Status(ctx context.Context, ref provision.Ref) (*provision.Env, error) {
	m, err := p.Client.GetMachine(ctx, ref.EnvID)
	if err != nil {
		return nil, mapErr(err)
	}
	e := p.env(m)
	e.VolumeID = ref.VolumeID
	return e, nil
}

// Wake is the pre-extraction proxy hot path: EnsureStarted wakes a
// suspended machine and polls until state=started or ctx times out.
func (p *Provisioner) Wake(ctx context.Context, ref provision.Ref) (*provision.Env, error) {
	m, err := p.Client.EnsureStarted(ctx, ref.EnvID, p.ProbeInterval)
	if err != nil {
		return nil, mapErr(err)
	}
	e := p.env(m)
	e.VolumeID = ref.VolumeID
	return e, nil
}

// Destroy force-deletes the machine even if started. The volume is
// left for the operator (same policy as the admin delete path).
func (p *Provisioner) Destroy(ctx context.Context, ref provision.Ref) error {
	if err := p.Client.DestroyMachine(ctx, ref.EnvID, true); err != nil {
		return mapErr(err)
	}
	return nil
}

// WakeOnRequest is false: Fly needs the explicit start API.
func (p *Provisioner) WakeOnRequest() bool { return false }

// env converts a Machine to the neutral view. Endpoint prefers the
// explicit private IPv6 from Fly so requests don't depend on DNS
// resolution; when absent it falls back to Fly internal DNS
// (<machine_id>.vm.<app>.internal — requires wg0 + the 6PN resolver,
// verified live in matrix.kvx sess#26 step 3).
func (p *Provisioner) env(m *Machine) *provision.Env {
	host := m.PrivateIP
	if host == "" {
		host = fmt.Sprintf("%s.vm.%s.internal", m.ID, p.Client.app)
	}
	return &provision.Env{
		ID:       m.ID,
		State:    m.State,
		Region:   m.Region,
		Endpoint: provision.Endpoint{Host: host},
		Ready:    m.Started(),
	}
}

// mapErr translates the client's sentinel errors onto the neutral ones
// so interface consumers dispatch without importing fly. Everything
// else (including context.DeadlineExceeded) passes through unchanged.
func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrMachineNotFound), errors.Is(err, ErrAppNotFound):
		return fmt.Errorf("%w: %v", provision.ErrNotFound, err)
	case errors.Is(err, ErrUnauthorized):
		return fmt.Errorf("%w: %v", provision.ErrUnauthorized, err)
	default:
		return err
	}
}

// safeName returns a DNS-safe lowercase prefix of s, max length n.
// Used so machine names don't blow up Fly's validation. Hyphens are
// preserved; Fly machine names accept them.
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

// volumeSafeName returns a [a-z0-9_]-only prefix of s, max length n.
// Stricter than safeName because Fly volume names reject hyphens
// ("name only allows lowercase alphanumeric characters and underscores
// with at most 30 characters" — observed 400 from POST /v1/apps/<app>/volumes).
// Hyphens are mapped to underscores so UUIDs round-trip cleanly.
func volumeSafeName(s string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < len(s) && len(out) < n; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '_':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		case c == '-':
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "user"
	}
	return string(out)
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
