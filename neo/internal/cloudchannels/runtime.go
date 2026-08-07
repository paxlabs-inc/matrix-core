// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package cloudchannels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type RuntimeStatus struct {
	Running   bool      `json:"running"`
	Connected bool      `json:"connected"`
	LastError string    `json:"last_error,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type SlackSocket struct {
	api               *SlackAPI
	config            func() SlackConfig
	handle            func(context.Context, SlackEventPayload) error
	handleInteractive func(context.Context, SlackActionPayload) error
	dialer            *websocket.Dialer

	mu        sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	connected bool
	lastError string
	updatedAt time.Time
}

func NewSlackSocket(api *SlackAPI, config func() SlackConfig, handle func(context.Context, SlackEventPayload) error, interactive ...func(context.Context, SlackActionPayload) error) *SlackSocket {
	socket := &SlackSocket{api: api, config: config, handle: handle, dialer: websocket.DefaultDialer}
	if len(interactive) > 0 {
		socket.handleInteractive = interactive[0]
	}
	return socket
}

func (s *SlackSocket) Start(parent context.Context) {
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
	go func() {
		defer close(done)
		s.run(ctx)
	}()
}

func (s *SlackSocket) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func (s *SlackSocket) Status() RuntimeStatus {
	if s == nil {
		return RuntimeStatus{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return RuntimeStatus{Running: s.cancel != nil, Connected: s.connected, LastError: s.lastError, UpdatedAt: s.updatedAt}
}

func (s *SlackSocket) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		config := s.config()
		if !config.Enabled || config.Mode != "socket" || !config.Configured() {
			s.setStatus(false, "")
			return
		}
		openCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		endpoint, err := s.api.OpenSocket(openCtx, config.AppToken)
		cancel()
		if err == nil {
			err = s.serve(ctx, endpoint)
		}
		if ctx.Err() != nil {
			return
		}
		s.setStatus(false, err.Error())
		if !waitContext(ctx, jitter(backoff)) {
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (s *SlackSocket) serve(ctx context.Context, endpoint string) error {
	connection, _, err := s.dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return err
	}
	defer connection.Close()
	stopClose := make(chan struct{})
	defer close(stopClose)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-stopClose:
		}
	}()
	connection.SetReadLimit(slackBodyLimit)
	s.setStatus(true, "")
	for ctx.Err() == nil {
		_ = connection.SetReadDeadline(time.Now().Add(90 * time.Second))
		var frame struct {
			EnvelopeID string          `json:"envelope_id"`
			Type       string          `json:"type"`
			Payload    json.RawMessage `json:"payload"`
		}
		if err := connection.ReadJSON(&frame); err != nil {
			return err
		}
		if frame.EnvelopeID != "" {
			_ = connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := connection.WriteJSON(map[string]string{"envelope_id": frame.EnvelopeID}); err != nil {
				return err
			}
		}
		if frame.Type == "events_api" && s.handle != nil {
			var payload SlackEventPayload
			if err := json.Unmarshal(frame.Payload, &payload); err != nil {
				s.setStatus(true, err.Error())
				continue
			}
			if err := s.handle(ctx, payload); err != nil {
				s.setStatus(true, err.Error())
			}
		} else if frame.Type == "interactive" && s.handleInteractive != nil {
			var payload SlackActionPayload
			if err := json.Unmarshal(frame.Payload, &payload); err != nil {
				s.setStatus(true, err.Error())
				continue
			}
			if err := s.handleInteractive(ctx, payload); err != nil {
				s.setStatus(true, err.Error())
			}
		}
	}
	return ctx.Err()
}

func (s *SlackSocket) setStatus(connected bool, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = connected
	s.lastError = message
	s.updatedAt = time.Now().UTC()
}

type DiscordGateway struct {
	api    *DiscordAPI
	store  *Store
	config func() DiscordConfig
	handle func(context.Context, DiscordMessage) error
	dialer *websocket.Dialer

	mu        sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	connected bool
	lastError string
	updatedAt time.Time
}

func NewDiscordGateway(api *DiscordAPI, store *Store, config func() DiscordConfig, handle func(context.Context, DiscordMessage) error) *DiscordGateway {
	return &DiscordGateway{api: api, store: store, config: config, handle: handle, dialer: websocket.DefaultDialer}
}

func (g *DiscordGateway) Start(parent context.Context) {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.cancel != nil {
		g.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	g.cancel = cancel
	g.done = make(chan struct{})
	done := g.done
	g.updatedAt = time.Now().UTC()
	g.mu.Unlock()
	go func() {
		defer close(done)
		g.run(ctx)
	}()
}

func (g *DiscordGateway) Stop() {
	if g == nil {
		return
	}
	g.mu.Lock()
	cancel, done := g.cancel, g.done
	g.cancel, g.done = nil, nil
	g.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func (g *DiscordGateway) Status() RuntimeStatus {
	if g == nil {
		return RuntimeStatus{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return RuntimeStatus{Running: g.cancel != nil, Connected: g.connected, LastError: g.lastError, UpdatedAt: g.updatedAt}
}

func (g *DiscordGateway) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		config := g.config()
		if !config.Enabled || !config.Gateway || !config.Configured() {
			g.setStatus(false, "")
			return
		}
		endpoint := config.ResumeURL
		if endpoint == "" {
			var err error
			discoverCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			endpoint, err = g.api.GatewayURL(discoverCtx, config.BotToken)
			cancel()
			if err != nil {
				g.setStatus(false, err.Error())
				if !waitContext(ctx, jitter(backoff)) {
					return
				}
				continue
			}
		} else {
			var err error
			endpoint, err = normalizeDiscordGatewayURL(endpoint, g.api.BaseURL)
			if err != nil {
				_ = g.store.UpdateDiscordSession("", "", 0)
				continue
			}
		}
		err := g.serve(ctx, endpoint, config)
		if ctx.Err() != nil {
			return
		}
		g.setStatus(false, err.Error())
		if !waitContext(ctx, jitter(backoff)) {
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (g *DiscordGateway) serve(ctx context.Context, endpoint string, config DiscordConfig) error {
	connection, _, err := g.dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return err
	}
	defer connection.Close()
	stopClose := make(chan struct{})
	defer close(stopClose)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-stopClose:
		}
	}()
	connection.SetReadLimit(discordBodyLimit)
	var hello DiscordGatewayPayload
	_ = connection.SetReadDeadline(time.Now().Add(30 * time.Second))
	if err := connection.ReadJSON(&hello); err != nil {
		return err
	}
	if hello.Op != 10 {
		return errors.New("Discord Gateway did not begin with Hello")
	}
	var heartbeat struct {
		Interval int `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.Data, &heartbeat); err != nil || heartbeat.Interval < 1000 {
		return errors.New("Discord Gateway heartbeat is invalid")
	}
	writer := &lockedWebsocketWriter{connection: connection}
	if config.SessionID != "" && config.Sequence > 0 {
		err = writer.JSON(map[string]any{"op": 6, "d": map[string]any{"token": config.BotToken, "session_id": config.SessionID, "seq": config.Sequence}})
	} else {
		err = writer.JSON(map[string]any{"op": 2, "d": map[string]any{
			"token": config.BotToken, "intents": 37376,
			"properties": map[string]string{"os": "linux", "browser": "matrix-neo", "device": "matrix-neo"},
		}})
	}
	if err != nil {
		return err
	}
	g.setStatus(true, "")
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	go func() {
		ticker := time.NewTicker(time.Duration(heartbeat.Interval) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				current := g.config()
				_ = writer.JSON(map[string]any{"op": 1, "d": current.Sequence})
			}
		}
	}()
	for ctx.Err() == nil {
		_ = connection.SetReadDeadline(time.Now().Add(time.Duration(heartbeat.Interval)*time.Millisecond*3 + 10*time.Second))
		var payload DiscordGatewayPayload
		if err := connection.ReadJSON(&payload); err != nil {
			return err
		}
		if payload.Sequence != nil {
			config.Sequence = *payload.Sequence
			_ = g.store.UpdateDiscordSession(config.SessionID, config.ResumeURL, config.Sequence)
		}
		switch payload.Op {
		case 0:
			switch payload.Event {
			case "READY":
				var ready struct {
					SessionID        string `json:"session_id"`
					ResumeGatewayURL string `json:"resume_gateway_url"`
				}
				if json.Unmarshal(payload.Data, &ready) == nil {
					config.SessionID, config.ResumeURL = ready.SessionID, ready.ResumeGatewayURL
					_ = g.store.UpdateDiscordSession(config.SessionID, config.ResumeURL, config.Sequence)
				}
			case "MESSAGE_CREATE":
				var message DiscordMessage
				if json.Unmarshal(payload.Data, &message) == nil && g.handle != nil {
					if err := g.handle(ctx, message); err != nil {
						g.setStatus(true, err.Error())
					}
				}
			}
		case 1:
			if err := writer.JSON(map[string]any{"op": 1, "d": config.Sequence}); err != nil {
				return err
			}
		case 7:
			return errors.New("Discord Gateway requested reconnect")
		case 9:
			_ = g.store.UpdateDiscordSession("", "", 0)
			return errors.New("Discord Gateway session is invalid")
		}
	}
	return ctx.Err()
}

func (g *DiscordGateway) setStatus(connected bool, message string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.connected = connected
	g.lastError = message
	g.updatedAt = time.Now().UTC()
}

type lockedWebsocketWriter struct {
	mu         sync.Mutex
	connection *websocket.Conn
}

func (w *lockedWebsocketWriter) JSON(value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.connection.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return w.connection.WriteJSON(value)
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func jitter(duration time.Duration) time.Duration {
	if duration <= 0 {
		return time.Second
	}
	return duration + time.Duration(rand.Int63n(max(int64(duration/3), 1)))
}

func runtimeError(prefix string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", prefix, err)
}
