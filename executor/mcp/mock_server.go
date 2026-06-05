// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// MockServer is an in-process MCP server stub used by tests. It pairs
// with a matching mockTransport so a Client can drive it without
// spawning a subprocess or opening a socket.
//
// Behaviour: implements initialize, tools/list, tools/call, ping with
// configurable handlers + a captured request log so tests can assert
// on the wire shape after the fact.
type MockServer struct {
	mu sync.Mutex

	tools []Tool

	// callHandler is invoked for tools/call requests. Default returns
	// an "unhandled" CallToolResult so tests notice missing wiring.
	callHandler func(name string, args map[string]interface{}) (*CallToolResult, error)

	// pingHandler optionally overrides the default ping behaviour for
	// tests of failure paths.
	pingHandler func() error

	// log captures every inbound JSON-RPC frame for test assertions.
	log [][]byte

	// initialized is set true after notifications/initialized arrives.
	initialized bool

	serverInfo Implementation
}

// MockServerParams configures a MockServer.
type MockServerParams struct {
	Tools       []Tool
	CallHandler func(name string, args map[string]interface{}) (*CallToolResult, error)
	PingHandler func() error
	ServerName  string
	ServerVer   string
}

// NewMockServer constructs an in-process server stub.
func NewMockServer(p MockServerParams) *MockServer {
	name := p.ServerName
	if name == "" {
		name = "mock-mcp"
	}
	ver := p.ServerVer
	if ver == "" {
		ver = "0.0.0-test"
	}
	return &MockServer{
		tools:       append([]Tool(nil), p.Tools...),
		callHandler: p.CallHandler,
		pingHandler: p.PingHandler,
		serverInfo:  Implementation{Name: name, Version: ver},
	}
}

// SetTools replaces the advertised tools list. Used by tests of
// manifest-drift detection (Q21).
func (m *MockServer) SetTools(tools []Tool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = append([]Tool(nil), tools...)
}

// Log returns a copy of the captured request log.
func (m *MockServer) Log() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]byte, len(m.log))
	for i, b := range m.log {
		c := make([]byte, len(b))
		copy(c, b)
		out[i] = c
	}
	return out
}

// Initialized reports whether the post-initialize notification arrived.
func (m *MockServer) Initialized() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.initialized
}

// handle dispatches one inbound frame and returns the response frame
// (or empty bytes for notifications, which never get a response).
func (m *MockServer) handle(frame []byte) ([]byte, error) {
	m.mu.Lock()
	m.log = append(m.log, append([]byte(nil), frame...))
	m.mu.Unlock()

	kind, req, _, note, err := Classify(frame)
	if err != nil {
		return nil, fmt.Errorf("mock server: classify: %w", err)
	}

	switch kind {
	case KindNotification:
		if note.Method == MethodNotificationsInit {
			m.mu.Lock()
			m.initialized = true
			m.mu.Unlock()
		}
		// Notifications never get a response.
		return nil, nil

	case KindRequest:
		return m.handleRequest(req)

	default:
		return nil, fmt.Errorf("mock server: unexpected inbound kind %v", kind)
	}
}

func (m *MockServer) handleRequest(req *Request) ([]byte, error) {
	switch req.Method {
	case MethodInitialize:
		result := InitializeResult{
			ProtocolVersion: ProtocolVersion,
			Capabilities: ServerCapabilities{
				Tools: &ToolsCapability{},
			},
			ServerInfo: m.serverInfo,
		}
		return m.respond(req.ID, result, nil)

	case MethodToolsList:
		m.mu.Lock()
		tools := append([]Tool(nil), m.tools...)
		m.mu.Unlock()
		return m.respond(req.ID, ToolsListResult{Tools: tools}, nil)

	case MethodToolsCall:
		var p CallToolParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return m.respond(req.ID, nil, &RPCError{
				Code:    ErrCodeInvalidParams,
				Message: "invalid tools/call params",
			})
		}
		m.mu.Lock()
		h := m.callHandler
		m.mu.Unlock()
		if h == nil {
			return m.respond(req.ID, &CallToolResult{
				IsError: true,
				Content: []Content{{Type: ContentTypeText, Text: "no handler"}},
			}, nil)
		}
		result, err := h(p.Name, p.Arguments)
		if err != nil {
			return m.respond(req.ID, nil, &RPCError{
				Code:    ErrCodeServerError,
				Message: err.Error(),
			})
		}
		return m.respond(req.ID, result, nil)

	case MethodPing:
		m.mu.Lock()
		h := m.pingHandler
		m.mu.Unlock()
		if h != nil {
			if err := h(); err != nil {
				return m.respond(req.ID, nil, &RPCError{
					Code:    ErrCodeServerUnavailable,
					Message: err.Error(),
				})
			}
		}
		return m.respond(req.ID, PingResult{}, nil)

	default:
		return m.respond(req.ID, nil, &RPCError{
			Code:    ErrCodeMethodNotFound,
			Message: "method not found: " + req.Method,
		})
	}
}

// respond builds a Response frame for the given id, result, error.
func (m *MockServer) respond(id json.RawMessage, result interface{}, rpcErr *RPCError) ([]byte, error) {
	resp := &Response{ID: id}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		b, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		resp.Result = b
	}
	return EncodeResponse(resp)
}

// PipeMock pairs a MockServer with a Transport stub so a Client can
// drive it directly. Returns the client-facing Transport. Close on
// the transport tears down the pair.
func PipeMock(server *MockServer) Transport {
	t := newPipeTransport(server)
	go t.run()
	return t
}

// pipeTransport is the in-process Transport that drives a MockServer.
// Implements Transport with two channels: outbound frames go to the
// server's handle() and the response goes onto the inbox channel.
type pipeTransport struct {
	server *MockServer

	inbound  chan []byte // frames going to server
	outbound chan []byte // frames coming back to client

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

func newPipeTransport(s *MockServer) *pipeTransport {
	return &pipeTransport{
		server:   s,
		inbound:  make(chan []byte, 16),
		outbound: make(chan []byte, 16),
		done:     make(chan struct{}),
	}
}

func (t *pipeTransport) Send(ctx context.Context, frame []byte) error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return ErrClosed
	}
	cp := make([]byte, len(frame))
	copy(cp, frame)
	select {
	case t.inbound <- cp:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.done:
		return ErrClosed
	}
}

func (t *pipeTransport) Recv(ctx context.Context) ([]byte, error) {
	select {
	case b, ok := <-t.outbound:
		if !ok {
			return nil, ErrClosed
		}
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, ErrClosed
	}
}

func (t *pipeTransport) Close() error {
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

// run is the server-side dispatcher loop.
func (t *pipeTransport) run() {
	for {
		select {
		case <-t.done:
			return
		case frame := <-t.inbound:
			resp, err := t.server.handle(frame)
			if err != nil {
				// Build an internal-error response targeted at the
				// request id we can extract from the inbound frame.
				if id := extractID(frame); id != nil {
					b, _ := EncodeResponse(&Response{
						ID: id,
						Error: &RPCError{
							Code:    ErrCodeInternalError,
							Message: err.Error(),
						},
					})
					t.deliver(b)
				}
				continue
			}
			if len(resp) > 0 {
				t.deliver(resp)
			}
		}
	}
}

func (t *pipeTransport) deliver(b []byte) {
	select {
	case t.outbound <- b:
	case <-t.done:
	}
}

// extractID pulls the id out of a JSON-RPC frame for error-response
// targeting; returns nil if the frame has no id or is malformed.
func extractID(frame []byte) json.RawMessage {
	var f struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(frame, &f); err != nil {
		return nil
	}
	if len(f.ID) == 0 || string(f.ID) == "null" {
		return nil
	}
	return f.ID
}

// Compile-time interface check.
var _ Transport = (*pipeTransport)(nil)

// ensure errors is used; mock_server imports it for fmt.Errorf which
// already pulls it in. Belt-and-suspenders for refactor safety.
var _ = errors.New

// Copyright © 2026 Paxlabs Inc. All rights reserved.
