// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Client is a JSON-RPC 2.0 + MCP layer above a Transport. One Client
// drives one server connection. Servers and clients are paired N:1
// behind the manager (one Client per server, one manager per agent).
//
// Concurrency: Initialize must be called exactly once before any other
// method, but is itself goroutine-safe through the read loop. After
// Initialize, ToolsList / ToolsCall / Ping may be called from any
// goroutine; the Client serialises through its outbound mutex.
type Client struct {
	t Transport

	// idCounter generates monotonic outbound request IDs. Use atomic
	// access so concurrent callers don't collide.
	idCounter atomic.Uint64

	// pending maps in-flight request id → response channel. Filled by
	// Send-side, drained by the read loop. Protected by mu.
	mu      sync.Mutex
	pending map[uint64]chan *Response

	// sendMu serialises Transport.Send calls to keep frames whole. The
	// Transport itself locks too, but client-side framing (encode +
	// pending-map insert + send) must be atomic w.r.t. read loop.
	sendMu sync.Mutex

	// closed signals the read loop to exit and prevents new sends.
	closed atomic.Bool

	// readDone is closed when the read loop exits, so Close can wait
	// for clean shutdown.
	readDone chan struct{}

	// readErr captures the read loop's exit error (nil for clean close,
	// or the underlying transport error for unexpected exits).
	readErrMu sync.Mutex
	readErr   error

	// notifyHandler is an optional callback invoked from the read loop
	// for inbound notifications. v1 wires this only for diagnostic
	// logging (notifications/cancelled, notifications/progress).
	notifyHandler func(*Notification)

	// info captures the InitializeResult once Initialize succeeds.
	infoMu sync.RWMutex
	info   *InitializeResult
}

// ClientParams configures a Client.
type ClientParams struct {
	// Transport is required.
	Transport Transport

	// NotifyHandler is invoked on inbound notifications. Default: drop.
	NotifyHandler func(*Notification)
}

// NewClient wraps a Transport and starts the read loop. The caller
// must call Initialize before issuing other methods. Close on the
// returned Client also closes the underlying Transport.
func NewClient(p ClientParams) (*Client, error) {
	if p.Transport == nil {
		return nil, errors.New("mcp/client: nil transport")
	}
	c := &Client{
		t:             p.Transport,
		pending:       make(map[uint64]chan *Response),
		readDone:      make(chan struct{}),
		notifyHandler: p.NotifyHandler,
	}
	go c.readLoop()
	return c, nil
}

// Initialize completes the MCP handshake: sends initialize request,
// awaits the result, sends notifications/initialized, and stashes the
// server's capabilities for later inspection via Info.
//
// Calling Initialize twice is a no-op the second time (returns the
// cached InitializeResult). This is forgiving by design; the manager
// treats Initialize as idempotent during reconnect attempts.
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	c.infoMu.RLock()
	cached := c.info
	c.infoMu.RUnlock()
	if cached != nil {
		return cached, nil
	}

	params := InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    ClientCapabilities{},
		ClientInfo: Implementation{
			Name:    ClientName,
			Version: ClientVersion,
		},
	}

	var result InitializeResult
	if err := c.Call(ctx, MethodInitialize, params, &result); err != nil {
		return nil, fmt.Errorf("mcp/client: initialize: %w", err)
	}

	if err := c.Notify(ctx, MethodNotificationsInit, struct{}{}); err != nil {
		return nil, fmt.Errorf("mcp/client: post-init notification: %w", err)
	}

	c.infoMu.Lock()
	c.info = &result
	c.infoMu.Unlock()

	return &result, nil
}

// Info returns the InitializeResult cached after a successful
// Initialize call, or nil if Initialize hasn't completed yet.
func (c *Client) Info() *InitializeResult {
	c.infoMu.RLock()
	defer c.infoMu.RUnlock()
	if c.info == nil {
		return nil
	}
	cp := *c.info
	return &cp
}

// ToolsList enumerates the server's exposed tools. Used by the
// manager at startup to verify the agent manifest matches what the
// server actually offers (Q21).
//
// Pagination via NextCursor is fully consumed here so callers see one
// flat list; paginated servers are uncommon at v1 scale but the spec
// allows them.
func (c *Client) ToolsList(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for {
		params := struct {
			Cursor string `json:"cursor,omitempty"`
		}{Cursor: cursor}

		var result ToolsListResult
		if err := c.Call(ctx, MethodToolsList, params, &result); err != nil {
			return nil, fmt.Errorf("mcp/client: tools/list: %w", err)
		}
		all = append(all, result.Tools...)
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return all, nil
}

// ToolsCall invokes a tool by name. Args are server-validated against
// the tool's inputSchema (and Matrix re-validates client-side via the
// tool registry).
//
// Returns the typed CallToolResult. Note: result.IsError=true is NOT
// surfaced as a Go error — that's an in-band tool-level failure (e.g.
// "shell exit 1") which the executor maps to a Failure step. Only
// transport-level / RPC-level errors come back through err.
func (c *Client) ToolsCall(ctx context.Context, name string, args map[string]interface{}) (*CallToolResult, error) {
	if name == "" {
		return nil, errors.New("mcp/client: tools/call requires name")
	}
	params := CallToolParams{Name: name, Arguments: args}
	var result CallToolResult
	if err := c.Call(ctx, MethodToolsCall, params, &result); err != nil {
		return nil, fmt.Errorf("mcp/client: tools/call %q: %w", name, err)
	}
	return &result, nil
}

// Ping issues a JSON-RPC ping (Q16 health-pinged lifecycle). Returns
// nil on success, transport error otherwise.
func (c *Client) Ping(ctx context.Context) error {
	var result PingResult
	if err := c.Call(ctx, MethodPing, struct{}{}, &result); err != nil {
		return fmt.Errorf("mcp/client: ping: %w", err)
	}
	return nil
}

// Call performs a synchronous request/response. params is JSON-encoded
// inline; result is JSON-decoded into resultPtr (must be a pointer to
// a struct or map). resultPtr may be nil if the caller doesn't care
// about the response body shape.
//
// Surfaces RPCError verbatim through err so callers can switch on
// codes (ErrCodeMethodNotFound, etc.).
func (c *Client) Call(ctx context.Context, method string, params interface{}, resultPtr interface{}) error {
	if c.closed.Load() {
		return ErrClosed
	}

	id := c.idCounter.Add(1)
	idJSON := NewIDInt(id)

	rawParams, err := encodeParams(params)
	if err != nil {
		return fmt.Errorf("mcp/client: encode params: %w", err)
	}

	frame, err := EncodeRequest(&Request{
		ID:     idJSON,
		Method: method,
		Params: rawParams,
	})
	if err != nil {
		return fmt.Errorf("mcp/client: encode request: %w", err)
	}

	respCh := make(chan *Response, 1)
	c.mu.Lock()
	c.pending[id] = respCh
	c.mu.Unlock()

	// Cleanup: always remove the pending entry so a slow response after
	// ctx cancel doesn't leak.
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	c.sendMu.Lock()
	err = c.t.Send(ctx, frame)
	c.sendMu.Unlock()
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp, ok := <-respCh:
		if !ok {
			// readLoop closed the channel due to transport close.
			return ErrClosed
		}
		if resp.Error != nil {
			return resp.Error
		}
		if resultPtr != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, resultPtr); err != nil {
				return fmt.Errorf("mcp/client: decode result: %w", err)
			}
		}
		return nil
	}
}

// Notify sends a one-way notification (no response expected).
func (c *Client) Notify(ctx context.Context, method string, params interface{}) error {
	if c.closed.Load() {
		return ErrClosed
	}
	rawParams, err := encodeParams(params)
	if err != nil {
		return fmt.Errorf("mcp/client: encode params: %w", err)
	}
	frame, err := EncodeNotification(&Notification{
		Method: method,
		Params: rawParams,
	})
	if err != nil {
		return fmt.Errorf("mcp/client: encode notification: %w", err)
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.t.Send(ctx, frame)
}

// Close shuts down the read loop and the underlying transport. Safe
// to call multiple times. Pending Calls unblock with ErrClosed.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	err := c.t.Close()
	<-c.readDone
	// Fail any still-pending requests so callers unblock.
	c.mu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	return err
}

// readLoop consumes inbound frames, classifies them, and routes to
// pending response channels (responses) or the notify handler
// (notifications). Inbound requests are not expected at v1 (no
// sampling/createMessage support); they're logged and ignored.
func (c *Client) readLoop() {
	defer close(c.readDone)
	ctx := context.Background()
	for {
		if c.closed.Load() {
			return
		}
		frame, err := c.t.Recv(ctx)
		if err != nil {
			c.readErrMu.Lock()
			c.readErr = err
			c.readErrMu.Unlock()
			return
		}
		kind, _, resp, note, classifyErr := Classify(frame)
		if classifyErr != nil {
			// Couldn't parse — drop and continue. A noisy server can't
			// stall us. Manager-level health checks catch persistent
			// breakage via ping timeouts.
			continue
		}
		switch kind {
		case KindResponse:
			c.routeResponse(resp)
		case KindNotification:
			if c.notifyHandler != nil {
				c.notifyHandler(note)
			}
		case KindRequest:
			// v1: ignore inbound requests. Future sampling/createMessage
			// support would dispatch here.
		}
	}
}

// routeResponse hands resp to the matching pending channel. Drops
// silently on no match (caller cancelled, double-response, etc.).
func (c *Client) routeResponse(resp *Response) {
	if resp == nil {
		return
	}
	var id uint64
	if err := json.Unmarshal(resp.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	c.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- resp:
	default:
		// Caller already received or cancelled; drop.
	}
}

// encodeParams marshals an arbitrary value to json.RawMessage, with
// special-case handling for nil → empty params.
func encodeParams(v interface{}) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	// Detect typed nil pointers via reflection-free fast path: if v is
	// the empty struct{}, encode as {}.
	if _, ok := v.(struct{}); ok {
		return json.RawMessage(`{}`), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	// Encode null as omitted.
	if string(b) == "null" {
		return nil, nil
	}
	return json.RawMessage(b), nil
}

// ReadError returns the read loop's terminating error after Close, or
// nil if the loop is still running or closed cleanly via Close.
func (c *Client) ReadError() error {
	c.readErrMu.Lock()
	defer c.readErrMu.Unlock()
	return c.readErr
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
