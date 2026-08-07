// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package cloudchannels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const dingTalkBotTopic = "/v1.0/im/bot/messages/get"

type DingTalkStream struct {
	config      func() DingTalkConfig
	handle      func(context.Context, DingTalkMessage) error
	OpenAPIHost string
	Client      *http.Client
	dialer      *websocket.Dialer

	mu        sync.Mutex
	writeMu   sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	conn      *websocket.Conn
	connected bool
	lastError string
	updatedAt time.Time
}

type dingTalkFrame struct {
	SpecVersion string            `json:"specVersion"`
	Type        string            `json:"type"`
	Time        int64             `json:"time"`
	Headers     map[string]string `json:"headers"`
	Data        string            `json:"data"`
}
type dingTalkFrameResponse struct {
	Code    int               `json:"code"`
	Headers map[string]string `json:"headers"`
	Message string            `json:"message"`
	Data    string            `json:"data"`
}

func NewDingTalkStream(config func() DingTalkConfig, handle func(context.Context, DingTalkMessage) error) *DingTalkStream {
	return &DingTalkStream{config: config, handle: handle, Client: &http.Client{Timeout: 10 * time.Second}, dialer: websocket.DefaultDialer}
}
func (s *DingTalkStream) Start(parent context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.done = make(chan struct{})
	done := s.done
	s.updatedAt = time.Now().UTC()
	s.mu.Unlock()
	go func() { defer close(done); s.run(ctx) }()
}
func (s *DingTalkStream) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel, done, conn := s.cancel, s.done, s.conn
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if conn != nil {
		_ = conn.Close()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}
func (s *DingTalkStream) Status() RuntimeStatus {
	if s == nil {
		return RuntimeStatus{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return RuntimeStatus{Running: s.cancel != nil, Connected: s.connected, LastError: s.lastError, UpdatedAt: s.updatedAt}
}

func (s *DingTalkStream) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		config := s.config()
		if !config.Enabled || config.Mode != "stream" || !config.Configured() {
			s.setStatus(false, "")
			return
		}
		endpoint, ticket, err := s.bootstrap(ctx, config)
		if err == nil {
			err = s.serve(ctx, endpoint, ticket)
		}
		if ctx.Err() != nil {
			return
		}
		s.setStatus(false, errorString(err))
		if !waitContext(ctx, jitter(backoff)) {
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (s *DingTalkStream) bootstrap(ctx context.Context, config DingTalkConfig) (string, string, error) {
	host := strings.TrimRight(strings.TrimSpace(s.OpenAPIHost), "/")
	if host == "" {
		host = "https://api.dingtalk.com"
	}
	payload := map[string]any{"clientId": config.ClientID, "clientSecret": config.ClientSecret, "subscriptions": []map[string]string{{"type": "CALLBACK", "topic": dingTalkBotTopic}}, "ua": "Matrix Neo", "extras": map[string]string{}}
	body, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/v1.0/gateway/connections/open", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	request.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	var result struct {
		Endpoint string `json:"endpoint"`
		Ticket   string `json:"ticket"`
	}
	if json.Unmarshal(data, &result) != nil || result.Endpoint == "" || result.Ticket == "" {
		return "", "", fmt.Errorf("DingTalk Stream bootstrap failed with status %d", response.StatusCode)
	}
	return result.Endpoint, result.Ticket, nil
}

func (s *DingTalkStream) serve(ctx context.Context, endpoint, ticket string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	query := parsed.Query()
	query.Set("ticket", ticket)
	parsed.RawQuery = query.Encode()
	conn, _, err := s.dialer.DialContext(ctx, parsed.String(), nil)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
		}
		s.mu.Unlock()
	}()
	conn.SetReadLimit(4 << 20)
	conn.SetPongHandler(func(string) error { _ = conn.SetReadDeadline(time.Now().Add(75 * time.Second)); return nil })
	s.setStatus(true, "")
	stop := make(chan struct{})
	defer close(stop)
	go s.ping(ctx, conn, stop)
	for ctx.Err() == nil {
		_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if messageType != websocket.TextMessage {
			continue
		}
		var frame dingTalkFrame
		if json.Unmarshal(data, &frame) != nil || frame.Headers == nil {
			continue
		}
		messageID := frame.Headers["messageId"]
		response := dingTalkFrameResponse{Code: http.StatusOK, Headers: map[string]string{"messageId": messageID, "contentType": "application/json"}, Message: "ok"}
		if frame.Headers["topic"] != dingTalkBotTopic {
			response.Code = http.StatusNotFound
			response.Message = "handler not found"
		} else {
			var message DingTalkMessage
			handleErr := json.Unmarshal([]byte(frame.Data), &message)
			if handleErr == nil && s.handle != nil {
				handleErr = s.handle(ctx, message)
			}
			if handleErr != nil {
				response.Code = http.StatusInternalServerError
				response.Message = handleErr.Error()
				s.setStatus(true, handleErr.Error())
			}
		}
		if err := s.writeJSON(conn, response); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *DingTalkStream) ping(ctx context.Context, conn *websocket.Conn, stop <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			s.writeMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			s.writeMu.Unlock()
		}
	}
}
func (s *DingTalkStream) writeJSON(conn *websocket.Conn, value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteJSON(value)
}
func (s *DingTalkStream) setStatus(connected bool, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = connected
	s.lastError = message
	s.updatedAt = time.Now().UTC()
}
