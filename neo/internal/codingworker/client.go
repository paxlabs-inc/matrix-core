package codingworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      RequestID       `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type rpcFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      RequestID       `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type pendingCall struct {
	response chan rpcResponse
}

type Client struct {
	cfg       ClientConfig
	ws        *wsConn
	events    chan Event
	done      chan struct{}
	ready     chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	pending   map[RequestID]pendingCall
	readErr   error
	nextID    atomic.Uint64
}

func Dial(ctx context.Context, cfg ClientConfig) (*Client, error) {
	cfg = cfg.normalized()
	ws, err := dialWebSocket(ctx, cfg.Endpoint, cfg.Token, cfg.ConnectTimeout, cfg.MaxFrameBytes)
	if err != nil {
		return nil, err
	}
	c := &Client{
		cfg:     cfg,
		ws:      ws,
		events:  make(chan Event, cfg.EventBuffer),
		done:    make(chan struct{}),
		ready:   make(chan struct{}),
		pending: make(map[RequestID]pendingCall),
	}
	go c.readLoop()
	readyCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	select {
	case <-c.ready:
		return c, nil
	case <-c.done:
		return nil, c.Err()
	case <-readyCtx.Done():
		_ = c.Close()
		return nil, fmt.Errorf("wait for agentcore gateway.ready: %w", readyCtx.Err())
	}
}

func (c *Client) Events() <-chan Event  { return c.events }
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil {
		return c.readErr
	}
	return ErrClosed
}

func (c *Client) Close() error {
	c.finish(ErrClosed)
	return nil
}

func (c *Client) finish(err error) {
	c.closeOnce.Do(func() {
		_ = c.ws.close()
		c.mu.Lock()
		c.readErr = err
		for id, call := range c.pending {
			close(call.response)
			delete(c.pending, id)
		}
		close(c.done)
		c.mu.Unlock()
	})
}

func (c *Client) readLoop() {
	defer close(c.events)
	ready := false
	for {
		message, err := c.ws.readMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = ErrClosed
			}
			c.finish(err)
			return
		}
		var frame rpcFrame
		if err := json.Unmarshal(message, &frame); err != nil {
			c.finish(fmt.Errorf("decode agentcore frame: %w", err))
			return
		}
		if frame.ID != "" {
			var response rpcResponse
			if err := json.Unmarshal(message, &response); err != nil {
				c.finish(fmt.Errorf("decode agentcore response: %w", err))
				return
			}
			c.mu.Lock()
			call, ok := c.pending[frame.ID]
			if ok {
				delete(c.pending, frame.ID)
			}
			c.mu.Unlock()
			if ok {
				call.response <- response
				close(call.response)
			}
			continue
		}
		if frame.Method != "event" {
			continue
		}
		var event Event
		if err := json.Unmarshal(frame.Params, &event); err != nil {
			c.finish(fmt.Errorf("decode agentcore event: %w", err))
			return
		}
		if event.Type == "gateway.ready" && !ready {
			ready = true
			close(c.ready)
		}
		select {
		case c.events <- event:
		default:
			c.finish(ErrEventBackpressure)
			return
		}
	}
}

func (c *Client) request(ctx context.Context, method string, params any, out any) error {
	if stringsTrimmedEmpty(method) {
		return errors.New("agentcore rpc method is required")
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encode %s params: %w", method, err)
	}
	id := RequestID(fmt.Sprintf("neo-%d", c.nextID.Add(1)))
	request := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      RequestID       `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{JSONRPC: "2.0", ID: id, Method: method, Params: paramsJSON}
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	call := pendingCall{response: make(chan rpcResponse, 1)}
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return c.Err()
	default:
	}
	c.pending[id] = call
	c.mu.Unlock()
	if err := c.ws.writeText(payload); err != nil {
		c.removePending(id)
		c.finish(err)
		return err
	}
	waitCtx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		waitCtx, cancel = context.WithTimeout(ctx, c.cfg.RequestTimeout)
	}
	defer cancel()
	select {
	case response, ok := <-call.response:
		if !ok {
			return c.Err()
		}
		if response.Error != nil {
			return response.Error
		}
		if out == nil || len(response.Result) == 0 || bytes.Equal(response.Result, []byte("null")) {
			return nil
		}
		if err := json.Unmarshal(response.Result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
		return nil
	case <-waitCtx.Done():
		c.removePending(id)
		return fmt.Errorf("agentcore request %s: %w", method, waitCtx.Err())
	case <-c.done:
		c.removePending(id)
		return c.Err()
	}
}

func (c *Client) removePending(id RequestID) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func stringsTrimmedEmpty(value string) bool {
	for _, r := range value {
		if r != ' ' && r != '\t' && r != '\r' && r != '\n' {
			return false
		}
	}
	return true
}

func (c *Client) Create(ctx context.Context, options CreateOptions) (Session, error) {
	var session Session
	if err := c.request(ctx, "session.create", options, &session); err != nil {
		return Session{}, err
	}
	if err := session.validate(); err != nil {
		return Session{}, err
	}
	if !session.DurableID().Valid() {
		return Session{}, errors.New("agentcore created a session without a durable id")
	}
	return session, nil
}

func (c *Client) Resume(ctx context.Context, id StoredSessionID, options ResumeOptions) (Session, error) {
	if !id.Valid() {
		return Session{}, errors.New("durable session id is required")
	}
	params := struct {
		SessionID StoredSessionID `json:"session_id"`
		ResumeOptions
	}{SessionID: id, ResumeOptions: options}
	var session Session
	if err := c.request(ctx, "session.resume", params, &session); err != nil {
		return Session{}, err
	}
	if err := session.validate(); err != nil {
		return Session{}, err
	}
	resolved := session.DurableID()
	if !resolved.Valid() {
		return Session{}, errors.New("agentcore resumed a session without a durable id")
	}
	if session.ResumedID.Valid() && session.ResumedID != id {
		return Session{}, fmt.Errorf("%w: requested %q, got %q", ErrSessionMismatch, id, session.ResumedID)
	}
	return session, nil
}

func DialAndResume(ctx context.Context, cfg ClientConfig, id StoredSessionID, options ResumeOptions) (*Client, Session, error) {
	client, err := Dial(ctx, cfg)
	if err != nil {
		return nil, Session{}, err
	}
	session, err := client.Resume(ctx, id, options)
	if err != nil {
		_ = client.Close()
		return nil, Session{}, err
	}
	return client, session, nil
}

func (c *Client) ReconnectAndResume(ctx context.Context, id StoredSessionID, options ResumeOptions) (*Client, Session, error) {
	cfg := c.cfg
	_ = c.Close()
	return DialAndResume(ctx, cfg, id, options)
}

func (c *Client) Branch(ctx context.Context, runtimeID SessionID, options BranchOptions) (Session, error) {
	if !runtimeID.Valid() {
		return Session{}, errors.New("runtime session id is required")
	}
	params := struct {
		SessionID SessionID `json:"session_id"`
		BranchOptions
	}{SessionID: runtimeID, BranchOptions: options}
	var session Session
	if err := c.request(ctx, "session.branch", params, &session); err != nil {
		return Session{}, err
	}
	if err := session.validate(); err != nil {
		return Session{}, err
	}
	if !session.DurableID().Valid() {
		return Session{}, errors.New("agentcore branched a session without a durable id")
	}
	return session, nil
}

func (c *Client) Prompt(ctx context.Context, runtimeID SessionID, text string) (PromptResult, error) {
	if !runtimeID.Valid() || stringsTrimmedEmpty(text) {
		return PromptResult{}, errors.New("runtime session id and prompt text are required")
	}
	var result PromptResult
	err := c.request(ctx, "prompt.submit", map[string]any{"session_id": runtimeID, "text": text}, &result)
	return result, err
}

func (c *Client) Approve(ctx context.Context, runtimeID SessionID, choice ApprovalChoice, all bool) (ApprovalResult, error) {
	if !runtimeID.Valid() || !choice.Valid() {
		return ApprovalResult{}, errors.New("runtime session id and approval choice are required")
	}
	var result ApprovalResult
	err := c.request(ctx, "approval.respond", map[string]any{"session_id": runtimeID, "choice": choice, "all": all}, &result)
	return result, err
}

func (c *Client) Interrupt(ctx context.Context, runtimeID SessionID) (InterruptResult, error) {
	if !runtimeID.Valid() {
		return InterruptResult{}, errors.New("runtime session id is required")
	}
	var result InterruptResult
	err := c.request(ctx, "session.interrupt", map[string]any{"session_id": runtimeID}, &result)
	return result, err
}

func (c *Client) CloseSession(ctx context.Context, runtimeID SessionID) (CloseResult, error) {
	if !runtimeID.Valid() {
		return CloseResult{}, errors.New("runtime session id is required")
	}
	var result CloseResult
	err := c.request(ctx, "session.close", map[string]any{"session_id": runtimeID}, &result)
	return result, err
}

func (c *Client) VerificationStatus(ctx context.Context, sessionID StoredSessionID, cwd string) (Verification, error) {
	if !sessionID.Valid() || stringsTrimmedEmpty(cwd) {
		return Verification{}, errors.New("durable session id and cwd are required")
	}
	var result VerificationResult
	if err := c.request(ctx, "verification.status", map[string]any{"session_id": sessionID, "cwd": cwd}, &result); err != nil {
		return Verification{}, err
	}
	result.Verification.Status = strings.ToLower(strings.TrimSpace(result.Verification.Status))
	if result.Verification.Status == "" {
		return Verification{}, errors.New("agentcore returned an empty verification status")
	}
	return result.Verification, nil
}

func HealthCheck(ctx context.Context, cfg ClientConfig) (Health, error) {
	cfg = cfg.normalized()
	healthURL, err := wsHTTPURL(cfg.Endpoint)
	if err != nil {
		return Health{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, cfg.HealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		return Health{}, err
	}
	client := &http.Client{Timeout: cfg.HealthTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return Health{}, fmt.Errorf("agentcore health request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Health{}, fmt.Errorf("agentcore health returned %s", resp.Status)
	}
	var health Health
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	if err := decoder.Decode(&health); err != nil {
		return Health{}, fmt.Errorf("decode agentcore health: %w", err)
	}
	if !health.OK {
		return Health{}, errors.New("agentcore reported unhealthy")
	}
	return health, nil
}
