// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package cloudchannels

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type qqGatewayPayload struct {
	Op       int             `json:"op"`
	Data     json.RawMessage `json:"d"`
	Sequence *int64          `json:"s,omitempty"`
	Event    string          `json:"t,omitempty"`
}

type QQGateway struct {
	api       *QQAPI
	store     *Store
	config    func() QQConfig
	handle    func(context.Context, string, QQMessage) error
	dialer    *websocket.Dialer
	mu        sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	conn      *websocket.Conn
	connected bool
	lastError string
	updatedAt time.Time
}

func NewQQGateway(api *QQAPI, store *Store, config func() QQConfig, handle func(context.Context, string, QQMessage) error) *QQGateway {
	return &QQGateway{api: api, store: store, config: config, handle: handle, dialer: websocket.DefaultDialer}
}
func (g *QQGateway) Start(parent context.Context) {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.cancel != nil {
		g.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	g.cancel, g.done, g.updatedAt = cancel, make(chan struct{}), time.Now().UTC()
	done := g.done
	g.mu.Unlock()
	go func() { defer close(done); g.run(ctx) }()
}
func (g *QQGateway) Stop() {
	if g == nil {
		return
	}
	g.mu.Lock()
	cancel, done, conn := g.cancel, g.done, g.conn
	g.cancel, g.done = nil, nil
	g.mu.Unlock()
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
func (g *QQGateway) Status() RuntimeStatus {
	if g == nil {
		return RuntimeStatus{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return RuntimeStatus{Running: g.cancel != nil, Connected: g.connected, LastError: g.lastError, UpdatedAt: g.updatedAt}
}
func (g *QQGateway) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		config := g.config()
		if !config.Enabled || !config.Configured() {
			g.setStatus(false, "")
			return
		}
		endpoint := config.ResumeURL
		if endpoint == "" {
			var err error
			endpoint, err = g.api.GatewayURL(ctx, config)
			if err != nil {
				g.setStatus(false, err.Error())
				if !waitContext(ctx, jitter(backoff)) {
					return
				}
				continue
			}
		}
		err := g.serve(ctx, endpoint, config)
		if ctx.Err() != nil {
			return
		}
		g.setStatus(false, errorString(err))
		if !waitContext(ctx, jitter(backoff)) {
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
func (g *QQGateway) serve(ctx context.Context, endpoint string, config QQConfig) error {
	conn, _, err := g.dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.conn = conn
	g.mu.Unlock()
	defer func() {
		_ = conn.Close()
		g.mu.Lock()
		if g.conn == conn {
			g.conn = nil
		}
		g.mu.Unlock()
	}()
	conn.SetReadLimit(4 << 20)
	var hello qqGatewayPayload
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if err = conn.ReadJSON(&hello); err != nil {
		return err
	}
	if hello.Op != 10 {
		return errors.New("QQ Gateway did not begin with Hello")
	}
	var hd struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	if json.Unmarshal(hello.Data, &hd) != nil || hd.HeartbeatInterval < 1000 {
		return errors.New("QQ Gateway heartbeat is invalid")
	}
	token, err := g.api.AccessToken(ctx, config)
	if err != nil {
		return err
	}
	writer := &lockedWebsocketWriter{connection: conn}
	if config.SessionID != "" && config.Sequence > 0 {
		err = writer.JSON(map[string]any{"op": 6, "d": map[string]any{"token": "QQBot " + token, "session_id": config.SessionID, "seq": config.Sequence}})
	} else {
		err = writer.JSON(map[string]any{"op": 2, "d": map[string]any{"token": "QQBot " + token, "intents": qqIntents, "shard": []int{0, 1}, "properties": map[string]string{"$os": "linux", "$browser": "centra-neo", "$device": "centra-neo"}}})
	}
	if err != nil {
		return err
	}
	g.setStatus(true, "")
	hbCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(time.Duration(hd.HeartbeatInterval) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				current := g.config()
				_ = writer.JSON(map[string]any{"op": 1, "d": current.Sequence})
			}
		}
	}()
	for ctx.Err() == nil {
		_ = conn.SetReadDeadline(time.Now().Add(time.Duration(hd.HeartbeatInterval)*3*time.Millisecond + 10*time.Second))
		var payload qqGatewayPayload
		if err = conn.ReadJSON(&payload); err != nil {
			return err
		}
		if payload.Sequence != nil {
			config.Sequence = *payload.Sequence
			_ = g.store.UpdateQQSession(config.SessionID, config.ResumeURL, config.Sequence)
		}
		switch payload.Op {
		case 0:
			switch payload.Event {
			case "READY":
				var ready struct {
					SessionID string `json:"session_id"`
					ResumeURL string `json:"resume_gateway_url"`
					User      struct {
						ID string `json:"id"`
					} `json:"user"`
				}
				if json.Unmarshal(payload.Data, &ready) == nil {
					config.SessionID, config.ResumeURL = ready.SessionID, ready.ResumeURL
					if config.ResumeURL == "" {
						config.ResumeURL = endpoint
					}
					_ = g.store.UpdateQQSession(config.SessionID, config.ResumeURL, config.Sequence)
				}
			case "RESUMED":
			case "GROUP_AT_MESSAGE_CREATE", "C2C_MESSAGE_CREATE", "AT_MESSAGE_CREATE", "DIRECT_MESSAGE_CREATE":
				var message QQMessage
				if json.Unmarshal(payload.Data, &message) == nil && g.handle != nil {
					if handleErr := g.handle(ctx, payload.Event, message); handleErr != nil {
						g.setStatus(true, handleErr.Error())
					}
				}
			}
		case 1:
			if err = writer.JSON(map[string]any{"op": 1, "d": config.Sequence}); err != nil {
				return err
			}
		case 7:
			return errors.New("QQ Gateway requested reconnect")
		case 9:
			_ = g.store.UpdateQQSession("", "", 0)
			return errors.New("QQ Gateway session is invalid")
		}
	}
	return ctx.Err()
}
func (g *QQGateway) setStatus(connected bool, message string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.connected = connected
	g.lastError = message
	g.updatedAt = time.Now().UTC()
}
