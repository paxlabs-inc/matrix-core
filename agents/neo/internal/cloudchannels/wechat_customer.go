// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package cloudchannels

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"centra/agents/neo/internal/channelgateway"
)

type WeChatKFCallback struct {
	XMLName    xml.Name `xml:"xml"`
	ToUserName string   `xml:"ToUserName"`
	CreateTime int64    `xml:"CreateTime"`
	MsgType    string   `xml:"MsgType"`
	Event      string   `xml:"Event"`
	Token      string   `xml:"Token"`
	OpenKFID   string   `xml:"OpenKfId"`
}
type WeChatKFMessage struct {
	MessageID      string `json:"msgid"`
	OpenKFID       string `json:"open_kfid"`
	ExternalUserID string `json:"external_userid"`
	SendTime       int64  `json:"send_time"`
	Origin         int    `json:"origin"`
	MessageType    string `json:"msgtype"`
	Text           struct {
		Content string `json:"content"`
	} `json:"text"`
	Image struct {
		MediaID string `json:"media_id"`
	} `json:"image"`
	Voice struct {
		MediaID string `json:"media_id"`
	} `json:"voice"`
	Video struct {
		MediaID string `json:"media_id"`
	} `json:"video"`
	File struct {
		MediaID  string `json:"media_id"`
		Filename string `json:"filename"`
	} `json:"file"`
}
type WeChatKFSync struct {
	ErrorCode    int               `json:"errcode"`
	ErrorMessage string            `json:"errmsg"`
	NextCursor   string            `json:"next_cursor"`
	HasMore      int               `json:"has_more"`
	Messages     []WeChatKFMessage `json:"msg_list"`
}

func NormalizeWeChatKFMessage(config WeChatKFConfig, message WeChatKFMessage) (channelgateway.Envelope, bool) {
	if message.MessageID == "" || message.OpenKFID == "" || message.ExternalUserID == "" {
		return channelgateway.Envelope{}, false
	}
	text := strings.TrimSpace(message.Text.Content)
	var media []channelgateway.Media
	switch message.MessageType {
	case "text":
	case "image":
		media = append(media, channelgateway.Media{Kind: channelgateway.MediaImage, Ref: "wechat-kf-media:" + message.Image.MediaID, Name: message.Image.MediaID + ".png"})
	case "voice":
		media = append(media, channelgateway.Media{Kind: channelgateway.MediaAudio, Ref: "wechat-kf-media:" + message.Voice.MediaID, Name: message.Voice.MediaID})
	case "video":
		media = append(media, channelgateway.Media{Kind: channelgateway.MediaVideo, Ref: "wechat-kf-media:" + message.Video.MediaID, Name: message.Video.MediaID + ".mp4"})
	case "file":
		media = append(media, channelgateway.Media{Kind: channelgateway.MediaFile, Ref: "wechat-kf-media:" + message.File.MediaID, Name: filepath.Base(message.File.Filename)})
	default:
		return channelgateway.Envelope{}, false
	}
	if text == "" && len(media) == 0 {
		return channelgateway.Envelope{}, false
	}
	kind := channelgateway.KindMessage
	if strings.EqualFold(text, "/stop") {
		kind = channelgateway.KindInterrupt
	}
	occurred := time.Now().UTC()
	if message.SendTime > 0 {
		occurred = time.Unix(message.SendTime, 0).UTC()
	}
	return channelgateway.Envelope{Direction: channelgateway.Inbound, Kind: kind, Address: channelgateway.Address{Channel: channelgateway.ChannelWeChatKF, AccountID: config.CorpID, ConversationID: message.OpenKFID + ":" + message.ExternalUserID, ParticipantID: message.ExternalUserID, Scope: channelgateway.ScopeDirect}, ExternalEventID: message.MessageID, ExternalMessageID: message.MessageID, IdempotencyKey: "wechat-kf:" + message.MessageID, Text: text, Media: media, OccurredAt: occurred, Metadata: map[string]string{"channel": message.OpenKFID, "open_kfid": message.OpenKFID, "reply_window_started_at": strconv.FormatInt(occurred.Unix(), 10)}}, true
}

type WeChatKFAPI struct {
	Client          *http.Client
	BaseURL         string
	mu              sync.Mutex
	token, tokenFor string
	expires         time.Time
}

func NewWeChatKFAPI() *WeChatKFAPI {
	return &WeChatKFAPI{Client: &http.Client{Timeout: 30 * time.Second}}
}
func (a *WeChatKFAPI) base() string {
	if a.BaseURL != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	return "https://qyapi.weixin.qq.com"
}
func (a *WeChatKFAPI) client() *http.Client {
	if a != nil && a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}
func (a *WeChatKFAPI) AccessToken(ctx context.Context, config WeChatKFConfig) (string, error) {
	key := config.CorpID + "\x00" + config.Secret
	a.mu.Lock()
	if a.token != "" && a.tokenFor == key && time.Now().Before(a.expires.Add(-time.Minute)) {
		token := a.token
		a.mu.Unlock()
		return token, nil
	}
	a.mu.Unlock()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.base()+"/cgi-bin/gettoken?corpid="+url.QueryEscape(config.CorpID)+"&corpsecret="+url.QueryEscape(config.Secret), nil)
	resp, err := a.client().Do(req)
	if err != nil {
		return "", redactProtocolError(err, config.Secret)
	}
	defer resp.Body.Close()
	var result struct {
		ErrorCode    int    `json:"errcode"`
		ErrorMessage string `json:"errmsg"`
		Token        string `json:"access_token"`
		Expires      int    `json:"expires_in"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.ErrorCode != 0 || result.Token == "" {
		return "", fmt.Errorf("WeChat Customer Service token exchange failed: %d %s", result.ErrorCode, result.ErrorMessage)
	}
	a.mu.Lock()
	a.token, a.tokenFor, a.expires = result.Token, key, time.Now().Add(time.Duration(result.Expires)*time.Second)
	a.mu.Unlock()
	return result.Token, nil
}
func (a *WeChatKFAPI) Sync(ctx context.Context, config WeChatKFConfig, eventToken, openKFID, cursor string) (WeChatKFSync, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return WeChatKFSync{}, err
	}
	payload := map[string]any{"token": eventToken, "open_kfid": openKFID, "limit": 1000}
	if cursor != "" {
		payload["cursor"] = cursor
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+"/cgi-bin/kf/sync_msg?access_token="+url.QueryEscape(token), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return WeChatKFSync{}, redactProtocolError(err, token, eventToken)
	}
	defer resp.Body.Close()
	var result WeChatKFSync
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil {
		return result, err
	}
	if result.ErrorCode != 0 {
		return result, fmt.Errorf("WeChat Customer Service sync failed: %d %s", result.ErrorCode, result.ErrorMessage)
	}
	return result, nil
}
func (a *WeChatKFAPI) PostText(ctx context.Context, config WeChatKFConfig, externalUser, openKFID, text string, windowStarted int64) (channelgateway.SendReceipt, error) {
	if windowStarted <= 0 || time.Since(time.Unix(windowStarted, 0)) > 48*time.Hour {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "reply_window", Message: "WeChat Customer Service replies require user activity within 48 hours", Permanent: true}
	}
	return a.send(ctx, config, map[string]any{"touser": externalUser, "open_kfid": openKFID, "msgtype": "text", "text": map[string]string{"content": text}})
}
func (a *WeChatKFAPI) PostMedia(ctx context.Context, config WeChatKFConfig, externalUser, openKFID, name string, data []byte, kind channelgateway.MediaKind, windowStarted int64) (channelgateway.SendReceipt, error) {
	if windowStarted <= 0 || time.Since(time.Unix(windowStarted, 0)) > 48*time.Hour {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "reply_window", Message: "WeChat Customer Service media replies require user activity within 48 hours", Permanent: true}
	}
	mediaType, field := "file", "file"
	switch kind {
	case channelgateway.MediaImage:
		mediaType, field = "image", "image"
	case channelgateway.MediaAudio:
		mediaType, field = "voice", "voice"
	case channelgateway.MediaVideo:
		mediaType, field = "video", "video"
	}
	mediaID, err := a.upload(ctx, config, mediaType, name, data)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	return a.send(ctx, config, map[string]any{"touser": externalUser, "open_kfid": openKFID, "msgtype": field, field: map[string]string{"media_id": mediaID}})
}
func (a *WeChatKFAPI) send(ctx context.Context, config WeChatKFConfig, payload map[string]any) (channelgateway.SendReceipt, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+"/cgi-bin/kf/send_msg?access_token="+url.QueryEscape(token), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return channelgateway.SendReceipt{}, redactProtocolError(err, token)
	}
	defer resp.Body.Close()
	var result struct {
		ErrorCode    int    `json:"errcode"`
		ErrorMessage string `json:"errmsg"`
		MessageID    string `json:"msgid"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if result.ErrorCode != 0 {
		if result.ErrorCode == 45009 {
			return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "rate_limited", Message: "WeChat Customer Service rate limit", RetryAfter: time.Minute}
		}
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "wechat_kf_" + strconv.Itoa(result.ErrorCode), Message: result.ErrorMessage, Permanent: result.ErrorCode == 95017 || result.ErrorCode == 40003}
	}
	return channelgateway.SendReceipt{ExternalMessageID: result.MessageID, Code: "accepted"}, nil
}
func (a *WeChatKFAPI) upload(ctx context.Context, config WeChatKFConfig, mediaType, name string, data []byte) (string, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("media", filepath.Base(name))
	if err != nil {
		return "", err
	}
	if _, err = part.Write(data); err != nil {
		return "", err
	}
	_ = writer.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+"/cgi-bin/media/upload?access_token="+url.QueryEscape(token)+"&type="+url.QueryEscape(mediaType), &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := a.client().Do(req)
	if err != nil {
		return "", redactProtocolError(err, token)
	}
	defer resp.Body.Close()
	var result struct {
		MediaID      string `json:"media_id"`
		ErrorCode    int    `json:"errcode"`
		ErrorMessage string `json:"errmsg"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.MediaID == "" {
		return "", fmt.Errorf("WeChat Customer Service media upload failed: %d %s", result.ErrorCode, result.ErrorMessage)
	}
	return result.MediaID, nil
}
func (a *WeChatKFAPI) DownloadMedia(ctx context.Context, config WeChatKFConfig, mediaID string, maxBytes int64) ([]byte, string, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return nil, "", err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.base()+"/cgi-bin/media/get?access_token="+url.QueryEscape(token)+"&media_id="+url.QueryEscape(mediaID), nil)
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, "", redactProtocolError(err, token)
	}
	defer resp.Body.Close()
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, "", fmt.Errorf("WeChat Customer Service media download failed: %s", strings.TrimSpace(string(data)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maxBytes {
		return nil, "", errors.New("WeChat Customer Service media exceeds the configured size limit")
	}
	return data, resp.Header.Get("Content-Type"), nil
}
