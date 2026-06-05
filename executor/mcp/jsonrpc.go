// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package mcp implements an Anthropic Model Context Protocol client.
//
// Spec: https://spec.modelcontextprotocol.io (2024-11+ revision)
// Wire format: JSON-RPC 2.0 over either stdio (line-delimited) or
// streamable HTTP (POST + JSON or SSE response). Q15 lock pins these
// two transports for v1; SSE-only legacy transport is rejected.
//
// Design (matrix.kvx executor_locked_design Q4-Q5, Q15-Q22):
//   - Q4  off-chain tools dispatch through MCP, not custom shell/fs/http
//   - Q15 stdio + streamable HTTP transports only
//   - Q16 per-agent persistent server processes; spawn-on-boot, drain on stop
//   - Q21 static manifest pinning — verify tools/list matches at startup
//   - Q22 server version pinned via package digest (sha256)
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
)

// JSONRPCVersion is the protocol version identifier required on every
// JSON-RPC 2.0 message.
const JSONRPCVersion = "2.0"

// Request is a JSON-RPC 2.0 request envelope. ID is required for
// requests and absent for notifications (see Notification).
//
// Params is left as raw bytes so the codec doesn't depend on knowledge
// of the method's parameter shape — the MCP-specific protocol layer
// in protocol.go decodes against the typed struct.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Notification is a JSON-RPC 2.0 notification — like Request but with
// no ID and no expected response. Used by MCP for
// notifications/initialized, notifications/cancelled, and
// notifications/progress (the latter two deferred to v1.1 per Q14).
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response envelope. Exactly one of Result
// or Error must be populated; the codec rejects malformed responses
// where both or neither are present.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements the error interface so RPCError can be returned
// from client methods without unwrapping.
func (e *RPCError) Error() string {
	if e == nil {
		return "<nil rpc error>"
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// Standard JSON-RPC 2.0 error codes (https://www.jsonrpc.org/specification).
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternalError  = -32603
)

// MCP-specific error codes (server-defined range -32000..-32099 per
// JSON-RPC spec). These are subject to MCP spec evolution; recorded
// here so the client can map common server errors to typed sentinels.
const (
	ErrCodeServerError       = -32000
	ErrCodeToolNotFound      = -32001
	ErrCodeInvalidToolInput  = -32602 // reuses InvalidParams
	ErrCodeServerUnavailable = -32002
)

// Sentinel errors raised by the codec layer (vs RPCError which wraps
// errors returned by the remote peer).
var (
	// ErrInvalidVersion fires when an inbound message lacks the required
	// "jsonrpc": "2.0" header. MCP servers that emit JSON-RPC 1.0 or
	// arbitrary JSON are rejected at the codec boundary.
	ErrInvalidVersion = errors.New("mcp: jsonrpc version must be 2.0")

	// ErrMissingMethod fires when a Request or Notification has no
	// method field.
	ErrMissingMethod = errors.New("mcp: jsonrpc message missing method")

	// ErrMissingID fires when a Request has no id field. Note that
	// Notifications legitimately have no ID; the codec routes the
	// frame correctly via the absent-id check.
	ErrMissingID = errors.New("mcp: jsonrpc request missing id")

	// ErrAmbiguousResponse fires when a Response carries both Result
	// AND Error, or neither. Per JSON-RPC spec exactly one is required.
	ErrAmbiguousResponse = errors.New("mcp: jsonrpc response must have exactly one of result or error")

	// ErrUnknownMessage fires when an inbound JSON object cannot be
	// classified as Request, Response, or Notification.
	ErrUnknownMessage = errors.New("mcp: unrecognised jsonrpc message shape")
)

// EncodeRequest serialises a request to JSON-RPC 2.0 wire bytes.
// Always emits jsonrpc: "2.0" regardless of caller-supplied value
// so callers can construct Request{} without filling in the version
// field every time.
func EncodeRequest(r *Request) ([]byte, error) {
	if r == nil {
		return nil, errors.New("mcp: nil request")
	}
	if r.Method == "" {
		return nil, ErrMissingMethod
	}
	if len(r.ID) == 0 {
		return nil, ErrMissingID
	}
	out := *r
	out.JSONRPC = JSONRPCVersion
	return json.Marshal(out)
}

// EncodeNotification serialises a notification to wire bytes.
func EncodeNotification(n *Notification) ([]byte, error) {
	if n == nil {
		return nil, errors.New("mcp: nil notification")
	}
	if n.Method == "" {
		return nil, ErrMissingMethod
	}
	out := *n
	out.JSONRPC = JSONRPCVersion
	return json.Marshal(out)
}

// EncodeResponse serialises a response to wire bytes. Used by the
// in-process mock server in tests; production clients only decode.
func EncodeResponse(r *Response) ([]byte, error) {
	if r == nil {
		return nil, errors.New("mcp: nil response")
	}
	if len(r.ID) == 0 {
		return nil, ErrMissingID
	}
	hasResult := len(r.Result) > 0
	hasError := r.Error != nil
	if hasResult == hasError {
		return nil, ErrAmbiguousResponse
	}
	out := *r
	out.JSONRPC = JSONRPCVersion
	return json.Marshal(out)
}

// MessageKind discriminates inbound JSON-RPC frames after a single
// pass of generic parsing. Used by the transport layer to route
// frames into the right typed channel.
type MessageKind int

const (
	// KindRequest is an inbound request from peer. MCP servers do not
	// send requests to clients in v1, but the spec allows it (e.g.
	// for sampling/createMessage callbacks); we surface it for forward
	// compatibility.
	KindRequest MessageKind = iota + 1

	// KindResponse is an inbound response to one of our outgoing requests.
	KindResponse

	// KindNotification is an inbound server-initiated notification.
	KindNotification
)

// rawFrame is an internal scratch struct for two-pass classification.
// Decodes once into RawMessage fields, then we inspect which fields
// were present to discriminate the kind.
type rawFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Classify parses an inbound JSON-RPC frame and returns the discriminated
// kind plus the typed message. The caller can pattern-match on kind
// and use the corresponding pointer (the others will be nil).
//
// JSON-RPC 2.0 routing rules:
//   - id present + method present  → Request
//   - id absent  + method present  → Notification
//   - id present + (result | error) → Response
//   - anything else                → ErrUnknownMessage
//
// jsonrpc version mismatch returns ErrInvalidVersion regardless of shape.
func Classify(data []byte) (MessageKind, *Request, *Response, *Notification, error) {
	var f rawFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return 0, nil, nil, nil, fmt.Errorf("mcp: invalid json: %w", err)
	}
	if f.JSONRPC != JSONRPCVersion {
		return 0, nil, nil, nil, ErrInvalidVersion
	}
	idPresent := len(f.ID) > 0 && string(f.ID) != "null"
	hasResult := len(f.Result) > 0
	hasError := f.Error != nil

	switch {
	case f.Method != "" && !idPresent:
		// Notification.
		return KindNotification, nil, nil, &Notification{
			JSONRPC: f.JSONRPC,
			Method:  f.Method,
			Params:  f.Params,
		}, nil

	case f.Method != "" && idPresent:
		// Inbound request from peer.
		return KindRequest, &Request{
			JSONRPC: f.JSONRPC,
			ID:      f.ID,
			Method:  f.Method,
			Params:  f.Params,
		}, nil, nil, nil

	case idPresent && (hasResult || hasError):
		if hasResult && hasError {
			return 0, nil, nil, nil, ErrAmbiguousResponse
		}
		return KindResponse, nil, &Response{
			JSONRPC: f.JSONRPC,
			ID:      f.ID,
			Result:  f.Result,
			Error:   f.Error,
		}, nil, nil

	default:
		return 0, nil, nil, nil, ErrUnknownMessage
	}
}

// NewIDInt constructs a json.RawMessage holding a numeric id. MCP
// servers tolerate either string or numeric ids; the client uses
// monotonic ints for compactness.
func NewIDInt(n uint64) json.RawMessage {
	b, _ := json.Marshal(n)
	return b
}

// NewIDString constructs a json.RawMessage holding a string id. Useful
// for tests where readable ids ease debugging.
func NewIDString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
