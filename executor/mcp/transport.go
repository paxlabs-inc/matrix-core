// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcp

import (
	"context"
	"errors"
)

// Transport abstracts the wire-format channel between Client and an MCP
// server. Implementations exist for stdio (subprocess) and streamable
// HTTP (Q15 lock).
//
// Concurrency contract:
//   - Send may be called from any goroutine; implementations serialise
//     internally.
//   - Recv is called by exactly one reader goroutine inside Client; it
//     blocks until a frame arrives or the transport is closed.
//   - Close is idempotent and can race with Send/Recv; both must return
//     promptly with ErrClosed after Close.
type Transport interface {
	// Send writes a single JSON-RPC frame (already encoded, no trailing
	// newline) to the peer. Stdio transports append the line terminator;
	// HTTP transports treat the frame as the request body.
	Send(ctx context.Context, frame []byte) error

	// Recv reads the next JSON-RPC frame from the peer. Returns ErrClosed
	// once the peer disconnects or Close is called locally.
	Recv(ctx context.Context) ([]byte, error)

	// Close shuts the transport down. After Close, Send and Recv return
	// ErrClosed. Close is safe to call multiple times.
	Close() error
}

// ErrClosed signals that a Transport has been closed (either locally
// via Close, by the peer disconnecting, or by EOF on the underlying
// stream). Returned by Send and Recv post-close.
var ErrClosed = errors.New("mcp: transport closed")

// Copyright © 2026 Paxlabs Inc. All rights reserved.
