// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeRequestAlwaysStampsVersion(t *testing.T) {
	r := &Request{
		ID:     NewIDInt(1),
		Method: "tools/list",
	}
	b, err := EncodeRequest(r)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(b), `"jsonrpc":"2.0"`) {
		t.Fatalf("missing jsonrpc version: %s", b)
	}
	if !strings.Contains(string(b), `"method":"tools/list"`) {
		t.Fatalf("missing method: %s", b)
	}
}

func TestEncodeRequestRequiresMethod(t *testing.T) {
	_, err := EncodeRequest(&Request{ID: NewIDInt(1)})
	if err != ErrMissingMethod {
		t.Fatalf("expected ErrMissingMethod, got %v", err)
	}
}

func TestEncodeRequestRequiresID(t *testing.T) {
	_, err := EncodeRequest(&Request{Method: "tools/list"})
	if err != ErrMissingID {
		t.Fatalf("expected ErrMissingID, got %v", err)
	}
}

func TestEncodeNotificationOmitsID(t *testing.T) {
	n := &Notification{Method: "notifications/initialized"}
	b, err := EncodeNotification(n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(b), `"id"`) {
		t.Fatalf("notification must not carry id: %s", b)
	}
}

func TestEncodeResponseRejectsAmbiguous(t *testing.T) {
	// Both result and error set.
	_, err := EncodeResponse(&Response{
		ID:     NewIDInt(1),
		Result: json.RawMessage(`{}`),
		Error:  &RPCError{Code: 1, Message: "bad"},
	})
	if err != ErrAmbiguousResponse {
		t.Fatalf("expected ErrAmbiguousResponse with both, got %v", err)
	}
	// Neither set.
	_, err = EncodeResponse(&Response{ID: NewIDInt(1)})
	if err != ErrAmbiguousResponse {
		t.Fatalf("expected ErrAmbiguousResponse with neither, got %v", err)
	}
}

func TestClassifyRequest(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":42,"method":"ping"}`)
	kind, req, resp, note, err := Classify(frame)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if kind != KindRequest {
		t.Fatalf("kind=%v want Request", kind)
	}
	if req == nil || resp != nil || note != nil {
		t.Fatalf("only req should be populated: req=%v resp=%v note=%v", req, resp, note)
	}
	if req.Method != "ping" {
		t.Fatalf("method=%q", req.Method)
	}
}

func TestClassifyNotification(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	kind, _, _, note, err := Classify(frame)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if kind != KindNotification || note == nil {
		t.Fatalf("expected Notification, got kind=%v note=%v", kind, note)
	}
	if note.Method != "notifications/initialized" {
		t.Fatalf("method=%q", note.Method)
	}
}

func TestClassifyResponseResult(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	kind, _, resp, _, err := Classify(frame)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if kind != KindResponse || resp == nil {
		t.Fatalf("expected Response, got kind=%v resp=%v", kind, resp)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
}

func TestClassifyResponseError(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`)
	kind, _, resp, _, err := Classify(frame)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if kind != KindResponse || resp == nil {
		t.Fatalf("expected Response, got kind=%v resp=%v", kind, resp)
	}
	if resp.Error == nil || resp.Error.Code != ErrCodeMethodNotFound {
		t.Fatalf("expected method-not-found, got %+v", resp.Error)
	}
}

func TestClassifyRejectsBadVersion(t *testing.T) {
	frame := []byte(`{"jsonrpc":"1.0","id":1,"method":"ping"}`)
	_, _, _, _, err := Classify(frame)
	if err != ErrInvalidVersion {
		t.Fatalf("expected ErrInvalidVersion, got %v", err)
	}
}

func TestClassifyRejectsAmbiguousResponse(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"bad"}}`)
	_, _, _, _, err := Classify(frame)
	if err != ErrAmbiguousResponse {
		t.Fatalf("expected ErrAmbiguousResponse, got %v", err)
	}
}

func TestClassifyRejectsUnknownShape(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0"}`)
	_, _, _, _, err := Classify(frame)
	if err != ErrUnknownMessage {
		t.Fatalf("expected ErrUnknownMessage, got %v", err)
	}
}

func TestClassifyHandlesNullID(t *testing.T) {
	// JSON-RPC permits id=null but treats it as a notification-shaped
	// response in some edge cases. We treat null id with method as a
	// notification (no response expected).
	frame := []byte(`{"jsonrpc":"2.0","id":null,"method":"notifications/cancelled"}`)
	kind, _, _, note, err := Classify(frame)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if kind != KindNotification || note == nil {
		t.Fatalf("expected Notification, got kind=%v", kind)
	}
}

func TestRPCErrorErrorMethod(t *testing.T) {
	e := &RPCError{Code: ErrCodeMethodNotFound, Message: "method not found"}
	if got := e.Error(); !strings.Contains(got, "-32601") || !strings.Contains(got, "method not found") {
		t.Fatalf("unexpected: %q", got)
	}
	var nilErr *RPCError
	if got := nilErr.Error(); got != "<nil rpc error>" {
		t.Fatalf("nil receiver: %q", got)
	}
}

func TestNewIDIntAndString(t *testing.T) {
	if got := string(NewIDInt(42)); got != "42" {
		t.Fatalf("int id: %q", got)
	}
	if got := string(NewIDString("init-1")); got != `"init-1"` {
		t.Fatalf("string id: %q", got)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
