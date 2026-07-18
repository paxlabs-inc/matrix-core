// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package provision defines the provider-neutral interface the router
// uses to manage per-user environments (a compute instance + its
// persistent volume). router/internal/fly implements it against the
// Fly Machines REST API; router/internal/railway against Railway's
// public GraphQL API. The active implementation is selected by
// configuration (ROUTER_PROVIDER=fly|railway) in cmd/matrix-router.
//
// admin.EnsureMachine, proxy.ServeHTTP, and the chronos wake path
// consume only this interface, so moving a deployment between
// providers is configuration, not code.
package provision

import (
	"context"
	"errors"
)

// Sentinel errors. Implementations map their provider-native failures
// onto these so callers can dispatch without provider knowledge.
var (
	// ErrNotFound: the environment (or its app/project scope) no
	// longer exists at the provider.
	ErrNotFound = errors.New("provision: environment not found")
	// ErrUnauthorized: the provider rejected the router's credential.
	ErrUnauthorized = errors.New("provision: unauthorized (check provider token)")
	// ErrUncertain means a mutation may have reached the provider but
	// its response was not observed. Callers must reconcile before retry.
	ErrUncertain = errors.New("provision: provider outcome uncertain")
)

// Endpoint is where the environment's daemon HTTP server is reachable
// on the private network. Host is unbracketed — a 6PN IPv6 address on
// Fly, a <service>.railway.internal hostname on Railway; callers
// bracket IPv6 literals when composing URLs. Port, when empty, falls
// back to the router's configured daemon port.
type Endpoint struct {
	Host string
	Port string
}

// Env is the provider-neutral view of a per-user environment.
type Env struct {
	ID       string // provider id: Fly machine id / Railway service id
	VolumeID string
	State    string // provider-native state string (informational)
	Region   string
	Endpoint Endpoint
	Ready    bool // instance is running / deployment succeeded
}

// Ref identifies an existing environment for status/wake/destroy. The
// ids come from the router's users row; UserID rides along so
// providers with deterministic per-user naming (Railway service
// hostnames) can derive the endpoint without an API round-trip.
type Ref struct {
	UserID   string
	EnvID    string
	VolumeID string
}

// CreateRequest carries everything Ensure needs to provision a fresh
// environment + volume. Env is the complete environment-variable set
// for the instance (identity + control-plane URLs), built by the
// admin layer; provider-specific config (image, sizing) lives on the
// implementation.
type CreateRequest struct {
	UserID string
	Region string
	Env    map[string]string
}

// Provisioner is the per-user environment lifecycle: ensure/create,
// status, wake, destroy — environment plus its persistent volume.
type Provisioner interface {
	// Ensure provisions a fresh environment + volume for the user and
	// returns it ready enough for the router to attach and route to.
	// Idempotence at the user level is the caller's job (the admin
	// layer checks the users row before calling).
	Ensure(ctx context.Context, req CreateRequest) (*Env, error)

	// Status reports the environment's current state. Ready mirrors
	// the provider's own readiness signal (machine started / latest
	// deployment succeeded) and feeds the provision-status gate.
	Status(ctx context.Context, ref Ref) (*Env, error)

	// Wake makes sure the environment can take traffic and returns
	// its endpoint. Providers with an explicit start API block until
	// the instance runs; wake-on-request providers return immediately
	// (the forwarded request itself is the wake signal).
	Wake(ctx context.Context, ref Ref) (*Env, error)

	// Destroy tears the environment down (volume follows the
	// provider's lifecycle rules). Destroying a vanished environment
	// returns ErrNotFound.
	Destroy(ctx context.Context, ref Ref) error

	// WakeOnRequest reports whether waking is platform-managed by
	// inbound traffic (Wake is a no-op and the readiness probe must
	// absorb the whole cold boot, including gateway 502/503s emitted
	// while the platform revives the instance).
	WakeOnRequest() bool
}

// Recoverable is implemented by providers that can discover and mutate
// compute and storage independently. It is the durable-operation seam used
// by the Railway reconciler; discovery must be safe and side-effect free.
type Recoverable interface {
	Provisioner
	ServiceName(userID string) string
	FindService(ctx context.Context, name string) (*Env, error)
	FindVolume(ctx context.Context, serviceID, mountPath string) (string, error)
	CreateService(ctx context.Context, req CreateRequest) (*Env, error)
	CreateVolume(ctx context.Context, serviceID, mountPath string) (string, error)
	WaitReady(ctx context.Context, serviceID string) error
	ServiceAbsent(ctx context.Context, serviceID string) (bool, error)
	VolumeAbsent(ctx context.Context, volumeID string) (bool, error)
	DeleteService(ctx context.Context, serviceID string) error
	DeleteVolume(ctx context.Context, volumeID string) error
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
