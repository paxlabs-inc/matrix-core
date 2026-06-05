// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// httpFakeServer wires a MockServer behind an httptest.Server so the
// HTTPTransport has something to talk to. Each POST body is a JSON-RPC
// frame; the response body is the corresponding response frame.
func httpFakeServer(t *testing.T, mock *MockServer) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := mock.handle(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(resp) == 0 {
			// Notification path — 202 with empty body per MCP HTTP spec.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
}

func TestHTTPInitializeAndCall(t *testing.T) {
	mock := NewMockServer(MockServerParams{
		Tools: []Tool{{Name: "echo"}},
		CallHandler: func(name string, args map[string]interface{}) (*CallToolResult, error) {
			msg, _ := args["msg"].(string)
			return &CallToolResult{Content: []Content{{Type: ContentTypeText, Text: msg}}}, nil
		},
		ServerName: "http-mock",
	})
	srv := httpFakeServer(t, mock)
	defer srv.Close()

	tr, err := NewHTTPTransport(HTTPParams{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("NewHTTPTransport: %v", err)
	}
	c, err := NewClient(ClientParams{Transport: tr})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	info, err := c.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.ServerInfo.Name != "http-mock" {
		t.Fatalf("server name=%q", info.ServerInfo.Name)
	}

	res, err := c.ToolsCall(ctx, "echo", map[string]interface{}{"msg": "hi-http"})
	if err != nil {
		t.Fatalf("ToolsCall: %v", err)
	}
	if got := ExtractText(res); got != "hi-http" {
		t.Fatalf("text=%q", got)
	}
}

func TestHTTPCustomHeaders(t *testing.T) {
	got := make(chan string, 1)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		// Build a generic ping success response so the call doesn't fail.
		body, _ := io.ReadAll(r.Body)
		var f rawFrame
		_ = json.Unmarshal(body, &f)
		resp := &Response{ID: f.ID, Result: json.RawMessage(`{}`)}
		b, _ := EncodeResponse(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer httpSrv.Close()

	tr, err := NewHTTPTransport(HTTPParams{
		Endpoint: httpSrv.URL,
		Headers:  http.Header{"Authorization": []string{"Bearer secret"}},
	})
	if err != nil {
		t.Fatalf("NewHTTPTransport: %v", err)
	}
	defer tr.Close()

	ctx := context.Background()
	frame, _ := EncodeRequest(&Request{
		ID:     NewIDInt(1),
		Method: MethodPing,
		Params: json.RawMessage(`{}`),
	})
	if err := tr.Send(ctx, frame); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case auth := <-got:
		if auth != "Bearer secret" {
			t.Fatalf("auth header=%q want %q", auth, "Bearer secret")
		}
	case <-time.After(time.Second):
		t.Fatal("no request received")
	}
}

func TestHTTPRejectsSSEResponse(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer httpSrv.Close()

	tr, err := NewHTTPTransport(HTTPParams{Endpoint: httpSrv.URL})
	if err != nil {
		t.Fatalf("NewHTTPTransport: %v", err)
	}
	defer tr.Close()

	frame, _ := EncodeRequest(&Request{
		ID:     NewIDInt(1),
		Method: MethodPing,
		Params: json.RawMessage(`{}`),
	})
	err = tr.Send(context.Background(), frame)
	if err == nil || !strings.Contains(err.Error(), "SSE") {
		t.Fatalf("expected SSE rejection, got %v", err)
	}
}

func TestHTTPSurfacesNon200(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server boom", http.StatusInternalServerError)
	}))
	defer httpSrv.Close()

	tr, err := NewHTTPTransport(HTTPParams{Endpoint: httpSrv.URL})
	if err != nil {
		t.Fatalf("NewHTTPTransport: %v", err)
	}
	defer tr.Close()

	frame, _ := EncodeRequest(&Request{
		ID:     NewIDInt(1),
		Method: MethodPing,
		Params: json.RawMessage(`{}`),
	})
	err = tr.Send(context.Background(), frame)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500 surfaced, got %v", err)
	}
}

func TestHTTPNotificationGets202(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Confirm we received a notification (no id).
		if bytes.Contains(body, []byte(`"id"`)) {
			http.Error(w, "expected notification", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer httpSrv.Close()

	tr, err := NewHTTPTransport(HTTPParams{Endpoint: httpSrv.URL})
	if err != nil {
		t.Fatalf("NewHTTPTransport: %v", err)
	}
	defer tr.Close()

	frame, _ := EncodeNotification(&Notification{
		Method: MethodNotificationsInit,
		Params: json.RawMessage(`{}`),
	})
	if err := tr.Send(context.Background(), frame); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestHTTPRejectsEmptyEndpoint(t *testing.T) {
	if _, err := NewHTTPTransport(HTTPParams{}); err == nil {
		t.Fatal("expected error on empty endpoint")
	}
}

func TestHTTPCloseReturnsErrClosed(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer httpSrv.Close()

	tr, err := NewHTTPTransport(HTTPParams{Endpoint: httpSrv.URL})
	if err != nil {
		t.Fatalf("NewHTTPTransport: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tr.Send(context.Background(), []byte("{}")); err != ErrClosed {
		t.Fatalf("Send post-close: %v", err)
	}
	if _, err := tr.Recv(context.Background()); err != ErrClosed {
		t.Fatalf("Recv post-close: %v", err)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
