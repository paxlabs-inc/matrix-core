package codingworker

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type protocolServer struct {
	listener net.Listener
	token    string
	mu       sync.Mutex
	methods  []string
	handler  func(string, json.RawMessage) (any, *RPCError)
}

func startProtocolServer(t *testing.T, handler func(string, json.RawMessage) (any, *RPCError)) *protocolServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &protocolServer{
		listener: listener,
		token:    strings.Repeat("t", 32),
		handler:  handler,
	}
	go server.serve(t)
	t.Cleanup(func() { _ = listener.Close() })
	return server
}

func (s *protocolServer) endpoint() string {
	return "ws://" + s.listener.Addr().String() + "/api/ws"
}

func (s *protocolServer) serve(t *testing.T) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.serveConn(t, conn)
	}
}

func (s *protocolServer) serveConn(t *testing.T, conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	if req.URL.Query().Get("token") != s.token {
		_, _ = io.WriteString(conn, "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n")
		return
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	acceptRaw := sha1.Sum([]byte(key + websocketGUID))
	accept := base64.StdEncoding.EncodeToString(acceptRaw[:])
	_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+accept+"\r\n\r\n")
	serverConn := &wsConn{conn: conn, reader: reader, maxFrame: 1 << 20, expectMasked: true}
	_ = writeServerJSON(conn, map[string]any{
		"jsonrpc": "2.0",
		"method":  "event",
		"params":  map[string]any{"type": "gateway.ready", "payload": map[string]any{"change_events": true}},
	})
	for {
		message, err := serverConn.readMessage()
		if err != nil {
			return
		}
		var request struct {
			ID     RequestID       `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(message, &request) != nil {
			return
		}
		s.mu.Lock()
		s.methods = append(s.methods, request.Method)
		s.mu.Unlock()
		result, rpcErr := s.handler(request.Method, request.Params)
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		if rpcErr != nil {
			response["error"] = rpcErr
		} else {
			response["result"] = result
		}
		if writeServerJSON(conn, response) != nil {
			return
		}
	}
}

func writeServerJSON(conn net.Conn, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	header := []byte{0x81}
	switch n := len(payload); {
	case n < 126:
		header = append(header, byte(n))
	case n <= 65535:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		return fmt.Errorf("test frame too large")
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err = conn.Write(payload)
	return err
}

func TestClientUsesPinnedSessionProtocol(t *testing.T) {
	handler := func(method string, params json.RawMessage) (any, *RPCError) {
		var values map[string]any
		_ = json.Unmarshal(params, &values)
		switch method {
		case "session.create":
			return map[string]any{"session_id": "runtime-1", "stored_session_id": "stored-1"}, nil
		case "prompt.submit":
			return map[string]any{"status": "streaming"}, nil
		case "approval.respond":
			return map[string]any{"resolved": true}, nil
		case "session.interrupt":
			return map[string]any{"status": "interrupted"}, nil
		case "session.branch":
			return map[string]any{"session_id": "runtime-2", "stored_session_id": "stored-2"}, nil
		case "session.close":
			return map[string]any{"closed": true}, nil
		case "verification.status":
			return map[string]any{"verification": map[string]any{"status": "passed", "evidence": map[string]any{"canonical_command": "npm test"}}}, nil
		default:
			return nil, &RPCError{Code: -32601, Message: "method not found"}
		}
	}
	server := startProtocolServer(t, handler)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, ClientConfig{Endpoint: server.endpoint(), Token: []byte(server.token)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	created, err := client.Create(ctx, CreateOptions{CWD: t.TempDir(), Source: "centra-neo"})
	if err != nil || created.RuntimeID != "runtime-1" || created.DurableID() != "stored-1" {
		t.Fatalf("create: session=%+v err=%v", created, err)
	}
	if result, err := client.Prompt(ctx, created.RuntimeID, "Build the requested page"); err != nil || result.Status != "streaming" {
		t.Fatalf("prompt: result=%+v err=%v", result, err)
	}
	if result, err := client.Approve(ctx, created.RuntimeID, ApproveOnce, false); err != nil || !result.Resolved {
		t.Fatalf("approval: result=%+v err=%v", result, err)
	}
	if result, err := client.Interrupt(ctx, created.RuntimeID); err != nil || result.Status != "interrupted" {
		t.Fatalf("interrupt: result=%+v err=%v", result, err)
	}
	branched, err := client.Branch(ctx, created.RuntimeID, BranchOptions{Name: "revision"})
	if err != nil || branched.DurableID() != "stored-2" {
		t.Fatalf("branch: session=%+v err=%v", branched, err)
	}
	if result, err := client.CloseSession(ctx, created.RuntimeID); err != nil || !result.Closed {
		t.Fatalf("close: result=%+v err=%v", result, err)
	}
	if verification, err := client.VerificationStatus(ctx, created.DurableID(), t.TempDir()); err != nil || verification.Status != "passed" {
		t.Fatalf("verification: result=%+v err=%v", verification, err)
	}
	server.mu.Lock()
	methods := append([]string(nil), server.methods...)
	server.mu.Unlock()
	want := []string{"session.create", "prompt.submit", "approval.respond", "session.interrupt", "session.branch", "session.close", "verification.status"}
	if fmt.Sprint(methods) != fmt.Sprint(want) {
		t.Fatalf("methods=%v want=%v", methods, want)
	}
}

func TestDialAndResumeNeverCreatesReplacementSession(t *testing.T) {
	server := startProtocolServer(t, func(method string, params json.RawMessage) (any, *RPCError) {
		if method != "session.resume" {
			return nil, &RPCError{Code: -32601, Message: "unexpected method"}
		}
		return map[string]any{"session_id": "runtime-new", "resumed": "different-stored", "session_key": "different-stored"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := DialAndResume(ctx, ClientConfig{Endpoint: server.endpoint(), Token: []byte(server.token)}, "expected-stored", ResumeOptions{})
	if client != nil {
		client.Close()
	}
	if !errors.Is(err, ErrSessionMismatch) {
		t.Fatalf("err=%v want ErrSessionMismatch", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.methods) != 1 || server.methods[0] != "session.resume" {
		t.Fatalf("reconnect methods=%v", server.methods)
	}
}

func TestClientRejectsNonLoopbackEndpoint(t *testing.T) {
	_, err := Dial(context.Background(), ClientConfig{Endpoint: "ws://example.com/api/ws", Token: []byte(strings.Repeat("x", 32))})
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("err=%v", err)
	}
}
