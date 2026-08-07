// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package cloudchannels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

type LarkSocket struct {
	config     func() LarkConfig
	handle     func(context.Context, LarkEventPayload) error
	handleCard func(context.Context, LarkCardActionPayload) (any, error)
	Domain     string
	Client     *http.Client
	dialer     *websocket.Dialer

	mu        sync.Mutex
	writeMu   sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	conn      *websocket.Conn
	connected bool
	lastError string
	updatedAt time.Time
	parts     map[string][][]byte
}

func NewLarkSocket(config func() LarkConfig, handle func(context.Context, LarkEventPayload) error, card ...func(context.Context, LarkCardActionPayload) (any, error)) *LarkSocket {
	s := &LarkSocket{config: config, handle: handle, Client: &http.Client{Timeout: 20 * time.Second}, dialer: websocket.DefaultDialer, parts: map[string][][]byte{}}
	if len(card) > 0 {
		s.handleCard = card[0]
	}
	return s
}

func (s *LarkSocket) Start(parent context.Context) {
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
func (s *LarkSocket) Stop() {
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
func (s *LarkSocket) Status() RuntimeStatus {
	if s == nil {
		return RuntimeStatus{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return RuntimeStatus{Running: s.cancel != nil, Connected: s.connected, LastError: s.lastError, UpdatedAt: s.updatedAt}
}

func (s *LarkSocket) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		config := s.config()
		if !config.Enabled || config.Mode != "websocket" || !config.Configured() {
			s.setStatus(false, "")
			return
		}
		endpoint, err := s.bootstrap(ctx, config)
		if err == nil {
			err = s.serve(ctx, config, endpoint)
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

func (s *LarkSocket) domain(config LarkConfig) string {
	if strings.TrimSpace(s.Domain) != "" {
		return strings.TrimRight(s.Domain, "/")
	}
	if config.Region == "feishu" {
		return "https://open.feishu.cn"
	}
	return "https://open.larksuite.com"
}

func (s *LarkSocket) bootstrap(ctx context.Context, config LarkConfig) (string, error) {
	body, _ := json.Marshal(larkws.BootstrapRequest{AppID: config.AppID, AppSecret: config.AppSecret})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.domain(config)+larkws.GenEndpointUri, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.Client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var result larkws.EndpointResp
	if json.Unmarshal(data, &result) != nil {
		return "", errors.New("Lark WebSocket bootstrap response is invalid")
	}
	if response.StatusCode != http.StatusOK || result.Code != 0 || result.Data == nil || result.Data.Url == "" {
		return "", fmt.Errorf("Lark WebSocket bootstrap failed: %d %s", result.Code, result.Msg)
	}
	return result.Data.Url, nil
}

func (s *LarkSocket) serve(ctx context.Context, config LarkConfig, endpoint string) error {
	conn, _, err := s.dialer.DialContext(ctx, endpoint, nil)
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
	s.setStatus(true, "")
	parsed, _ := url.Parse(endpoint)
	serviceID64, _ := strconv.ParseInt(parsed.Query().Get(larkws.ServiceID), 10, 32)
	stop := make(chan struct{})
	defer close(stop)
	go s.ping(ctx, conn, int32(serviceID64), stop)
	for ctx.Err() == nil {
		_ = conn.SetReadDeadline(time.Now().Add(150 * time.Second))
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		var frame larkws.Frame
		if frame.Unmarshal(data) != nil {
			continue
		}
		headers := larkws.Headers(frame.Headers)
		if larkws.FrameType(frame.Method) == larkws.FrameTypeControl {
			continue
		}
		frameMessageType := larkws.MessageType(headers.GetString(larkws.HeaderType))
		if frameMessageType != larkws.MessageTypeEvent && frameMessageType != larkws.MessageTypeCard {
			continue
		}
		payload := s.combine(headers.GetString(larkws.HeaderMessageID), headers.GetInt(larkws.HeaderSum), headers.GetInt(larkws.HeaderSeq), frame.Payload)
		if payload == nil {
			continue
		}
		started := time.Now()
		var responseData any
		var handleErr error
		if frameMessageType == larkws.MessageTypeCard {
			var action LarkCardActionPayload
			action, handleErr = DecodeLarkCardAction(payload, config.EncryptKey)
			if handleErr == nil && s.handleCard != nil {
				responseData, handleErr = s.handleCard(ctx, action)
			}
		} else {
			var event LarkEventPayload
			event, handleErr = DecodeLarkPayload(payload, config.EncryptKey)
			if handleErr == nil && s.handle != nil {
				handleErr = s.handle(ctx, event)
			}
		}
		headers.Add(larkws.HeaderBizRt, strconv.FormatInt(time.Since(started).Milliseconds(), 10))
		status := http.StatusOK
		if handleErr != nil {
			status = http.StatusInternalServerError
			s.setStatus(true, handleErr.Error())
		}
		response := larkws.NewResponseByCode(status)
		if responseData != nil {
			response.Data, _ = json.Marshal(responseData)
		}
		responsePayload, _ := json.Marshal(response)
		frame.Payload = responsePayload
		frame.Headers = headers
		encoded, _ := frame.Marshal()
		if err := s.write(conn, websocket.BinaryMessage, encoded); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *LarkSocket) combine(messageID string, sum, seq int, payload []byte) []byte {
	if sum <= 1 {
		return payload
	}
	if messageID == "" || sum > 64 || seq < 0 || seq >= sum {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	parts := s.parts[messageID]
	if len(parts) != sum {
		parts = make([][]byte, sum)
	}
	parts[seq] = append([]byte(nil), payload...)
	s.parts[messageID] = parts
	size := 0
	for _, part := range parts {
		if len(part) == 0 {
			return nil
		}
		size += len(part)
	}
	delete(s.parts, messageID)
	combined := make([]byte, 0, size)
	for _, part := range parts {
		combined = append(combined, part...)
	}
	return combined
}

func (s *LarkSocket) ping(ctx context.Context, conn *websocket.Conn, serviceID int32, stop <-chan struct{}) {
	ticker := time.NewTicker(90 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			frame := larkws.NewPingFrame(serviceID)
			encoded, _ := frame.Marshal()
			_ = s.write(conn, websocket.BinaryMessage, encoded)
		}
	}
}
func (s *LarkSocket) write(conn *websocket.Conn, messageType int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(messageType, data)
}
func (s *LarkSocket) setStatus(connected bool, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = connected
	s.lastError = message
	s.updatedAt = time.Now().UTC()
}
