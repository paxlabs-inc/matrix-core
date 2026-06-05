// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// HTTPTransport implements the Streamable HTTP transport per the
// 2024-11-05 MCP spec revision (Q15 lock).
//
// Wire shape:
//   - Each client→server message is a POST to a single endpoint URL.
//   - The request body is one JSON-RPC frame (no batching at v1).
//   - Successful responses arrive synchronously as the POST response body.
//     Server-initiated notifications (and SSE-shaped streamed responses)
//     are deferred to v1.1 — Matrix v1 only consumes simple JSON responses.
//
// Q14 + executor_deferrals: streaming progress / SSE-shaped responses
// deferred to v1.1; we surface a clean error if a server tries to use
// them so manifest authors notice the gap rather than silently lose data.
type HTTPTransport struct {
	endpoint string
	headers  http.Header
	client   *http.Client

	// inbox carries response frames into Recv. Sized to one because
	// v1 issues exactly one Send before each Recv (Client.Call is
	// strictly request/response).
	inbox chan []byte

	// errInbox carries transport-level errors to Recv so the Client
	// surfaces them as call failures rather than hangs.
	errInbox chan error

	// done is closed by Close so a Recv blocked on inbox/errInbox can
	// unblock cleanly.
	done chan struct{}

	mu     sync.Mutex
	closed bool
}

// HTTPParams configures a streamable HTTP transport.
type HTTPParams struct {
	// Endpoint is the absolute URL of the MCP server (e.g.
	// "https://mcp.example.com/v1/jsonrpc").
	Endpoint string

	// Headers are appended to every outbound request. Q18 lock: bearer
	// tokens go here; never logged, never journaled.
	Headers http.Header

	// Timeout is the per-request HTTP timeout. Zero = 30s default.
	Timeout time.Duration

	// Client allows tests to inject a custom *http.Client (mock
	// transport). Default = http.DefaultClient with Timeout applied.
	Client *http.Client
}

// NewHTTPTransport constructs a streamable HTTP transport. No network
// activity here — the first Send actually contacts the server.
func NewHTTPTransport(p HTTPParams) (*HTTPTransport, error) {
	if p.Endpoint == "" {
		return nil, errors.New("mcp/http: empty endpoint")
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	cl := p.Client
	if cl == nil {
		cl = &http.Client{Timeout: timeout}
	}

	hdr := http.Header{}
	for k, vs := range p.Headers {
		for _, v := range vs {
			hdr.Add(k, v)
		}
	}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Accept", "application/json")

	return &HTTPTransport{
		endpoint: p.Endpoint,
		headers:  hdr,
		client:   cl,
		inbox:    make(chan []byte, 16),
		errInbox: make(chan error, 16),
		done:     make(chan struct{}),
	}, nil
}

// Send POSTs one JSON-RPC frame to the endpoint. The response body
// (if it carries a JSON-RPC frame, which it always does for
// requests; never for notifications) is enqueued on inbox for the
// next Recv.
//
// Notifications get a 202 with empty body per spec; we don't enqueue
// anything in that case.
func (t *HTTPTransport) Send(ctx context.Context, frame []byte) error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return ErrClosed
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(frame))
	if err != nil {
		return fmt.Errorf("mcp/http: build request: %w", err)
	}
	for k, vs := range t.headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("mcp/http: post: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("mcp/http: read body: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		// Should be a JSON-RPC response; enqueue for Recv.
		// Reject SSE-shaped responses explicitly so manifest authors
		// see the v1 limitation rather than silently lose frames.
		if ct := resp.Header.Get("Content-Type"); ct != "" && isSSE(ct) {
			return fmt.Errorf("mcp/http: server returned SSE (%s); streamable HTTP SSE responses deferred to v1.1", ct)
		}
		if len(body) == 0 {
			// Empty success body — server treated us as a notification.
			// Nothing to enqueue; caller didn't expect a response.
			return nil
		}
		select {
		case t.inbox <- body:
		default:
			return errors.New("mcp/http: response inbox full")
		}
		return nil

	case http.StatusAccepted:
		// 202 — notification accepted, no response. Nothing to enqueue.
		return nil

	default:
		// Any other status is a transport error. Surface to Recv too
		// so a pending request unblocks instead of timing out.
		err := fmt.Errorf("mcp/http: server returned %d: %s", resp.StatusCode, truncate(string(body), 256))
		select {
		case t.errInbox <- err:
		default:
		}
		return err
	}
}

// Recv blocks for the next frame, an out-of-band error, ctx cancel,
// or transport close.
func (t *HTTPTransport) Recv(ctx context.Context) ([]byte, error) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, ErrClosed
	case frame := <-t.inbox:
		return frame, nil
	case err := <-t.errInbox:
		return nil, err
	}
}

// Close marks the transport closed and drains pending channels.
// Subsequent Send/Recv return ErrClosed.
func (t *HTTPTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.done)
	t.mu.Unlock()
	return nil
}

// isSSE reports whether a Content-Type header is text/event-stream.
func isSSE(ct string) bool {
	for i := 0; i < len(ct); i++ {
		if ct[i] == ';' {
			ct = ct[:i]
			break
		}
	}
	// Trim trailing whitespace.
	for len(ct) > 0 && (ct[len(ct)-1] == ' ' || ct[len(ct)-1] == '\t') {
		ct = ct[:len(ct)-1]
	}
	return ct == "text/event-stream"
}

// truncate caps a string for error messages.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
