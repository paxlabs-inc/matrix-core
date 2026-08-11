// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package cloudchannels

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"matrix/neo/internal/channelgateway"
)

const weComBotWebSocketURL = "wss://openws.work.weixin.qq.com"

type WeComBotSocket struct {
	config func() WeComBotConfig
	handle func(context.Context, WeComBotMessage) error
	URL    string
	dialer *websocket.Dialer

	mu        sync.Mutex
	writeMu   sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	conn      *websocket.Conn
	connected bool
	lastError string
	updatedAt time.Time
	pending   map[string]chan weComFrame
}

type weComFrame struct {
	Cmd     string `json:"cmd"`
	ErrCode *int   `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
	Headers struct {
		ReqID string `json:"req_id"`
	} `json:"headers"`
	Body json.RawMessage `json:"body"`
}

func NewWeComBotSocket(config func() WeComBotConfig, handle func(context.Context, WeComBotMessage) error) *WeComBotSocket {
	return &WeComBotSocket{config: config, handle: handle, dialer: websocket.DefaultDialer, pending: map[string]chan weComFrame{}}
}

func (s *WeComBotSocket) Start(parent context.Context) {
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

func (s *WeComBotSocket) Stop() {
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

func (s *WeComBotSocket) Status() RuntimeStatus {
	if s == nil {
		return RuntimeStatus{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return RuntimeStatus{Running: s.cancel != nil, Connected: s.connected, LastError: s.lastError, UpdatedAt: s.updatedAt}
}

func (s *WeComBotSocket) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		config := s.config()
		if !config.Enabled || config.Mode != "websocket" || !config.Configured() {
			s.setStatus(false, "")
			return
		}
		err := s.serve(ctx, config)
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

func (s *WeComBotSocket) serve(ctx context.Context, config WeComBotConfig) error {
	endpoint := s.URL
	if endpoint == "" {
		endpoint = weComBotWebSocketURL
	}
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
	if err := s.write(conn, map[string]any{"cmd": "aibot_subscribe", "headers": map[string]string{"req_id": newWeComRequestID()}, "body": map[string]string{"bot_id": config.BotID, "secret": config.Secret}}); err != nil {
		return err
	}
	stop := make(chan struct{})
	defer close(stop)
	go s.heartbeat(ctx, conn, stop)
	for ctx.Err() == nil {
		_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		var frame weComFrame
		if err := conn.ReadJSON(&frame); err != nil {
			return err
		}
		s.mu.Lock()
		pending := s.pending[frame.Headers.ReqID]
		if pending != nil {
			delete(s.pending, frame.Headers.ReqID)
		}
		s.mu.Unlock()
		if pending != nil {
			pending <- frame
			close(pending)
			continue
		}
		if frame.ErrCode != nil && frame.Cmd == "" {
			if *frame.ErrCode != 0 {
				return fmt.Errorf("WeCom Bot subscribe failed: %d %s", *frame.ErrCode, frame.ErrMsg)
			}
			s.setStatus(true, "")
			continue
		}
		if frame.Cmd == "aibot_msg_callback" {
			var message WeComBotMessage
			if err := json.Unmarshal(frame.Body, &message); err != nil {
				s.setStatus(true, err.Error())
				continue
			}
			if message.AIBotID == "" {
				message.AIBotID = config.BotID
			}
			if s.handle != nil {
				if err := s.handle(ctx, message); err != nil {
					s.setStatus(true, err.Error())
				}
			}
		}
	}
	return ctx.Err()
}

func (s *WeComBotSocket) heartbeat(ctx context.Context, conn *websocket.Conn, stop <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			_ = s.write(conn, map[string]any{"cmd": "ping", "headers": map[string]string{"req_id": newWeComRequestID()}})
		}
	}
}

func (s *WeComBotSocket) SendText(ctx context.Context, conversation string, group bool, text string) (channelgateway.SendReceipt, error) {
	s.mu.Lock()
	conn, connected := s.conn, s.connected
	s.mu.Unlock()
	if conn == nil || !connected {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "disconnected", Message: "WeCom Bot socket is disconnected"}
	}
	reqID := newWeComRequestID()
	chatType := 1
	if group {
		chatType = 2
	}
	payload := map[string]any{"cmd": "aibot_send_msg", "headers": map[string]string{"req_id": reqID}, "body": map[string]any{"chatid": conversation, "chat_type": chatType, "msgtype": "markdown", "markdown": map[string]string{"content": text}}}
	done := make(chan error, 1)
	go func() { done <- s.write(conn, payload) }()
	select {
	case err := <-done:
		if err != nil {
			return channelgateway.SendReceipt{}, err
		}
		return channelgateway.SendReceipt{ExternalMessageID: reqID, Code: "accepted"}, nil
	case <-ctx.Done():
		return channelgateway.SendReceipt{}, ctx.Err()
	}
}

func (s *WeComBotSocket) SendMedia(ctx context.Context, conversation string, group bool, name string, data []byte, kind channelgateway.MediaKind) (channelgateway.SendReceipt, error) {
	s.mu.Lock()
	conn, connected := s.conn, s.connected
	s.mu.Unlock()
	if conn == nil || !connected {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "disconnected", Message: "WeCom Bot socket is disconnected"}
	}
	mediaType := "file"
	switch kind {
	case channelgateway.MediaImage:
		mediaType = "image"
	case channelgateway.MediaAudio:
		mediaType = "voice"
	case channelgateway.MediaVideo:
		mediaType = "video"
	}
	const chunkSize = 512 * 1024
	chunks := int(math.Ceil(float64(len(data)) / chunkSize))
	if len(data) < 5 || chunks > 100 {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "media_size", Message: "WeCom Bot media size is invalid", Permanent: true}
	}
	sum := md5.Sum(data)
	init, err := s.sendAndWait(ctx, conn, "aibot_upload_media_init", map[string]any{"type": mediaType, "filename": filepath.Base(name), "total_size": len(data), "total_chunks": chunks, "md5": fmt.Sprintf("%x", sum)})
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	var initBody struct {
		UploadID string `json:"upload_id"`
	}
	_ = json.Unmarshal(init.Body, &initBody)
	if initBody.UploadID == "" {
		return channelgateway.SendReceipt{}, errors.New("WeCom Bot upload did not return an upload id")
	}
	for index := 0; index < chunks; index++ {
		start := index * chunkSize
		end := start + chunkSize
		if end > len(data) {
			end = len(data)
		}
		if _, err := s.sendAndWait(ctx, conn, "aibot_upload_media_chunk", map[string]any{"upload_id": initBody.UploadID, "chunk_index": index, "base64_data": base64.StdEncoding.EncodeToString(data[start:end])}); err != nil {
			return channelgateway.SendReceipt{}, err
		}
	}
	finish, err := s.sendAndWait(ctx, conn, "aibot_upload_media_finish", map[string]any{"upload_id": initBody.UploadID})
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	var finishBody struct {
		MediaID string `json:"media_id"`
	}
	_ = json.Unmarshal(finish.Body, &finishBody)
	if finishBody.MediaID == "" {
		return channelgateway.SendReceipt{}, errors.New("WeCom Bot upload did not return a media id")
	}
	reqID := newWeComRequestID()
	chatType := 1
	if group {
		chatType = 2
	}
	if err := s.write(conn, map[string]any{"cmd": "aibot_send_msg", "headers": map[string]string{"req_id": reqID}, "body": map[string]any{"chatid": conversation, "chat_type": chatType, "msgtype": mediaType, mediaType: map[string]string{"media_id": finishBody.MediaID}}}); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	return channelgateway.SendReceipt{ExternalMessageID: reqID, Code: "accepted"}, nil
}

func (s *WeComBotSocket) sendAndWait(ctx context.Context, conn *websocket.Conn, cmd string, body any) (weComFrame, error) {
	reqID := newWeComRequestID()
	response := make(chan weComFrame, 1)
	s.mu.Lock()
	s.pending[reqID] = response
	s.mu.Unlock()
	if err := s.write(conn, map[string]any{"cmd": cmd, "headers": map[string]string{"req_id": reqID}, "body": body}); err != nil {
		s.mu.Lock()
		delete(s.pending, reqID)
		s.mu.Unlock()
		return weComFrame{}, err
	}
	select {
	case frame := <-response:
		if frame.ErrCode != nil && *frame.ErrCode != 0 {
			return frame, fmt.Errorf("WeCom Bot %s failed: %d %s", cmd, *frame.ErrCode, frame.ErrMsg)
		}
		return frame, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, reqID)
		s.mu.Unlock()
		return weComFrame{}, ctx.Err()
	case <-time.After(30 * time.Second):
		s.mu.Lock()
		delete(s.pending, reqID)
		s.mu.Unlock()
		return weComFrame{}, errors.New("WeCom Bot upload response timed out")
	}
}

func (s *WeComBotSocket) write(conn *websocket.Conn, value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteJSON(value)
}

func (s *WeComBotSocket) setStatus(connected bool, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = connected
	s.lastError = message
	s.updatedAt = time.Now().UTC()
}

func newWeComRequestID() string { return uuid.NewString()[:16] }
func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
